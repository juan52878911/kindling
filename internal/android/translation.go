package android

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/juan52878911/kindling/internal/ext4"
)

// LA TRADUCCIÓN ARM (issue #93, prototypes/android/docs/traduccion-arm.md).
//
// Redroid 13 amd64 (13.0.0_64only-240527) ya trae un libndk_translation (con
// libnb.so como puente y ro.dalvik.vm.native.bridge=libnb.so en
// vendor/build.prop), sin decir de dónde sale ni con qué licencia. Por eso el
// constructor decide qué lleva la imagen:
//
//   - none (por defecto en amd64): se quita ese puente y las ABIs quedan en
//     x86_64. Una app solo-ARM da INSTALL_FAILED_NO_MATCHING_ABIS al
//     instalarse, en vez de instalarse y fallar después.
//   - libndk: se quita el de Redroid y se pone el de la imagen del emulador
//     de Google (libndkPin), bajado y comprobado por el constructor.
//   - redroid: se deja el de Redroid tal cual.
//
// En arm64 no se toca nada: las apps ARM corren nativas.

// Modos de Spec.ARMTranslation.
const (
	TranslationNone    = "none"
	TranslationLibndk  = "libndk"
	TranslationRedroid = "redroid"
)

// translation es el modo efectivo ("" en arm64: nativo).
func (s *Spec) translation() string {
	if s.Arch != "amd64" {
		return ""
	}
	if s.ARMTranslation == "" {
		return TranslationNone
	}
	return s.ARMTranslation
}

// bridgePaths son las rutas del puente nativo en el rootfs de Android (las
// de 32 bits también, por si la imagen de Redroid no es 64only).
var bridgePaths = []string{
	"/system/lib64/libnb.so", "/system/lib/libnb.so",
	"/system/lib64/arm64", "/system/lib/arm",
	"/system/bin/arm64", "/system/bin/arm",
	"/system/bin/ndk_translation_program_runner_binfmt_misc_arm64",
	"/system/bin/ndk_translation_program_runner_binfmt_misc",
	"/system/etc/binfmt_misc", "/system/etc/init/ndk_translation.rc",
	"/system/etc/ld.config.arm64.txt", "/system/etc/ld.config.arm.txt",
}

// stripBridge quita el puente nativo del árbol y dice cuántas rutas quitó.
func stripBridge(android *ext4.Node) int {
	n := 0
	for _, lib := range []string{"/system/lib64", "/system/lib"} {
		d := android.Lookup(lib)
		if d == nil || !d.IsDir() {
			continue
		}
		for _, c := range d.Children() {
			if strings.HasPrefix(c, "libndk_translation") && strings.HasSuffix(c, ".so") {
				d.RemoveChild(c)
				n++
			}
		}
	}
	for _, p := range bridgePaths {
		if android.Lookup(p) != nil {
			android.Remove(p)
			n++
		}
	}
	return n
}

// propFiles son las build.prop que carga init, en su orden (system,
// system_ext, vendor, odm, product: la última que define una propiedad
// manda). La lista de ABIs está repetida en varias (en Redroid 13, también en
// la de odm), y ro.product.cpu.abilist la deriva init de la primera que
// encuentra entre product, odm, vendor y system: hay que tocarlas todas.
var propFiles = []string{"/system/build.prop", "/system_ext/etc/build.prop", "/system/system_ext/etc/build.prop",
	"/vendor/build.prop", "/odm/etc/build.prop", "/vendor/odm/etc/build.prop",
	"/product/etc/build.prop", "/system/product/etc/build.prop"}

// Las propiedades de la traducción. Las que no están en ninguna build.prop se
// añaden a la de vendor (se carga después de la de system y manda).
var (
	reAbilist   = regexp.MustCompile(`^ro\.(?:system\.|vendor\.|odm\.|product\.)?product\.cpu\.abilist(32|64)?$`)
	armABIs     = map[string]bool{"arm64-v8a": true, "armeabi-v7a": true, "armeabi": true}
	bridgeProps = []string{"ro.dalvik.vm.native.bridge", "ro.enable.native.bridge.exec",
		"ro.vendor.enable.native.bridge.exec", "ro.vendor.enable.native.bridge.exec64",
		"ro.dalvik.vm.isa.arm", "ro.dalvik.vm.isa.arm64", "ro.ndk_translation.version", "ro.ndk_translation.flags"}
)

// translationProps son las propiedades de cada modo; "" borra la línea.
func translationProps(mode string) map[string]string {
	m := map[string]string{}
	for _, k := range bridgeProps {
		m[k] = ""
	}
	switch mode {
	case TranslationNone:
		m["ro.dalvik.vm.native.bridge"] = "0"
		m["ro.enable.native.bridge.exec"] = "0"
	case TranslationLibndk:
		m["ro.dalvik.vm.native.bridge"] = "libndk_translation.so"
		m["ro.enable.native.bridge.exec"] = "1"
		m["ro.vendor.enable.native.bridge.exec"] = "1"
		m["ro.vendor.enable.native.bridge.exec64"] = "1"
		m["ro.dalvik.vm.isa.arm64"] = "x86_64"
		m["ro.ndk_translation.version"] = libndkPin.Version
		// La de la imagen del emulador: SIGSEGV con el contexto exacto de la
		// instrucción arm64 (lo necesitan ART y los manejadores de señales).
		m["ro.ndk_translation.flags"] = "accurate-sigsegv"
	}
	return m
}

// fixABIs rehace una lista de ABIs: sin las ARM (none) o con arm64-v8a
// detrás de las x86 (libndk). Las listas de 32 bits no ganan nada: la
// traducción que se pone es solo arm64.
func fixABIs(list, bits, mode string) string {
	var out []string
	for _, a := range strings.Split(list, ",") {
		if a = strings.TrimSpace(a); a != "" && !armABIs[a] {
			out = append(out, a)
		}
	}
	if mode == TranslationLibndk && bits != "32" {
		out = append(out, "arm64-v8a")
	}
	return strings.Join(out, ",")
}

const translationMark = "kindling-arm-translation"

// editProps aplica el modo a una build.prop. Devuelve el texto y las claves de
// set que ya estaban (para no añadirlas otra vez en vendor).
func editProps(txt, mode string, set map[string]string, found map[string]bool) string {
	lines := strings.SplitAfter(txt, "\n")
	for i, l := range lines {
		body := strings.TrimRight(l, "\r\n")
		k, v, ok := strings.Cut(body, "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(k), "#") {
			continue
		}
		k = strings.TrimSpace(k)
		nl := l[len(body):]
		if m := reAbilist.FindStringSubmatch(k); m != nil {
			if nv := fixABIs(v, m[1], mode); nv != v {
				lines[i] = k + "=" + nv + nl
			}
			continue
		}
		nv, ok := set[k]
		if !ok {
			continue
		}
		found[k] = true
		switch {
		case nv == "":
			lines[i] = "# " + translationMark + ": " + l
		case nv != v:
			lines[i] = k + "=" + nv + nl
		}
	}
	return strings.Join(lines, "")
}

// armTranslation aplica el modo del spec al árbol de Android y devuelve lo que
// se apunta de él en la receta.
func (b *builder) armTranslation(ctx context.Context, android *ext4.Node) (map[string]any, error) {
	mode := b.spec.translation()
	info := map[string]any{"mode": mode}
	if mode == "" {
		info["mode"] = "native"
		return info, nil
	}
	if mode == TranslationRedroid {
		return info, nil
	}
	removed := stripBridge(android)
	if mode == TranslationLibndk {
		tp, err := b.libndkTar(ctx)
		if err != nil {
			return nil, err
		}
		n, err := b.addTarFiles(android, tp)
		if err != nil {
			return nil, fmt.Errorf("libndk_translation: %w", err)
		}
		if err := b.binfmtWrapper(android); err != nil {
			return nil, err
		}
		sum, err := sha256Path(tp)
		if err != nil {
			return nil, err
		}
		info["source"] = libndkPin.URL
		info["source_sha256"] = libndkPin.SHA256
		info["source_android"] = libndkPin.Android
		info["version"] = libndkPin.Version
		info["files_sha256"] = sum
		info["files"] = n
		b.logf("arm translation: libndk_translation %s from the Android %s emulator image (%d entries, replaced %d of Redroid's)", libndkPin.Version, libndkPin.Android, n, removed)
	} else {
		b.logf("arm translation: none (removed %d native bridge paths shipped by Redroid)", removed)
	}
	set := translationProps(mode)
	found := map[string]bool{}
	var vendor *ext4.Node
	seen := map[*ext4.Node]bool{}
	for _, p := range propFiles {
		n, _ := android.Resolve(p)
		if n == nil || !n.IsReg() || seen[n] {
			continue
		}
		seen[n] = true
		data, ok := n.Data.(ext4.Bytes)
		if !ok {
			return nil, fmt.Errorf("arm translation: %s was not loaded", p)
		}
		txt := editProps(string(data), mode, set, found)
		if p == "/vendor/build.prop" {
			vendor = n
			var add []string
			for _, k := range bridgeProps {
				if v := set[k]; v != "" && !found[k] {
					add = append(add, k+"="+v)
				}
			}
			if len(add) > 0 {
				if !strings.HasSuffix(txt, "\n") {
					txt += "\n"
				}
				txt += "# >>> " + translationMark + " (" + mode + ")\n" + strings.Join(add, "\n") + "\n# <<< " + translationMark + "\n"
			}
		}
		n.Data, n.Size, n.Mtime = ext4.Bytes(txt), int64(len(txt)), b.t
	}
	if vendor == nil {
		return nil, errors.New("arm translation: no /vendor/build.prop in the rootfs")
	}
	info["native_bridge"] = set["ro.dalvik.vm.native.bridge"]
	return info, nil
}

// El lanzador de binfmt_misc de libndk_translation toma el argv[0] ORIGINAL
// (el que binfmt_misc conserva con la bandera P) como ruta del programa y le
// hace realpath: si un shell ejecuta un programa del PATH por su nombre ("id",
// argv[0]="id"), falla con "Unable to get realpath of id". Es lo que rompe el
// bootstrap de Termux arm64 en un x86_64 (Termux lo documenta en su
// termux-bootstrap-second-stage.sh). binfmtWrapperSh va de intérprete: el
// kernel le da la ruta de verdad ($1) y el argv[0] original ($2), y si este no
// es una ruta, se lo cambia por la ruta antes de llamar al lanzador. Es un
// script de sh (binfmt_misc admite un intérprete con #!): no añade binarios.
const (
	binfmtRunner    = "/system/bin/ndk_translation_program_runner_binfmt_misc_arm64"
	binfmtWrapper   = "/system/bin/kindling-ndk-binfmt"
	binfmtWrapperSh = `#!/system/bin/sh
# Generado por kindling (arm_translation libndk; docs/traduccion-arm.md).
# binfmt_misc (bandera P) llama: <esto> <ruta del programa> <argv[0]> <args...>
f="$1"; a0="$2"; shift 2
case "$a0" in */*) ;; *) a0="$f" ;; esac
exec ` + binfmtRunner + ` "$f" "$a0" "$@"
`
)

// binfmtWrapper pone el intérprete de arriba y apunta a él los registros de
// binfmt_misc de arm64.
func (b *builder) binfmtWrapper(android *ext4.Node) error {
	b.putFile(android, binfmtWrapper, &ext4.Node{Mode: ext4.ModeReg | 0o755, GID: 2000,
		Size: int64(len(binfmtWrapperSh)), Data: ext4.Bytes(binfmtWrapperSh)})
	for _, p := range []string{"/system/etc/binfmt_misc/arm64_exe", "/system/etc/binfmt_misc/arm64_dyn"} {
		n := android.Lookup(p)
		var d ext4.Bytes
		ok := false
		if n != nil {
			d, ok = n.Data.(ext4.Bytes)
		}
		if !ok || !strings.Contains(string(d), ":"+binfmtRunner+":") {
			return fmt.Errorf("arm translation: %s does not register %s", p, binfmtRunner)
		}
		nd := strings.Replace(string(d), ":"+binfmtRunner+":", ":"+binfmtWrapper+":", 1)
		n.Data, n.Size, n.Mtime = ext4.Bytes(nd), int64(len(nd)), b.t
	}
	return nil
}

// addTarFiles cuelga del árbol lo del tar de la caché (datos en memoria:
// son ~21 MiB).
func (b *builder) addTarFiles(android *ext4.Node, tp string) (int, error) {
	f, err := os.Open(tp)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	n := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		p := path.Clean("/" + h.Name)
		if !libndkWanted(p) {
			return n, fmt.Errorf("unexpected entry %s in %s", p, tp)
		}
		perm := uint32(h.Mode) & ext4.ModePerm
		switch h.Typeflag {
		case tar.TypeDir:
			d, err := android.MkdirAll(p, perm, uint32(h.Uid), uint32(h.Gid), h.ModTime)
			if err != nil {
				return n, err
			}
			d.Mode, d.UID, d.GID, d.Mtime = ext4.ModeDir|perm, uint32(h.Uid), uint32(h.Gid), h.ModTime
		case tar.TypeReg:
			data, err := io.ReadAll(io.LimitReader(tr, 64<<20))
			if err != nil {
				return n, err
			}
			b.putFile(android, p, &ext4.Node{Mode: ext4.ModeReg | perm, UID: uint32(h.Uid), GID: uint32(h.Gid),
				Mtime: h.ModTime, Size: int64(len(data)), Data: ext4.Bytes(data)})
		case tar.TypeSymlink:
			b.putFile(android, p, &ext4.Node{Mode: ext4.ModeLink | 0o777, UID: uint32(h.Uid), GID: uint32(h.Gid),
				Mtime: h.ModTime, Target: h.Linkname})
		default:
			return n, fmt.Errorf("unexpected type %c for %s", h.Typeflag, p)
		}
		n++
	}
}

// propValue lee una propiedad de build.prop (la última que aparece manda).
func propValue(txt []byte, key string) string {
	v := ""
	for _, l := range strings.Split(string(txt), "\n") {
		if k, val, ok := strings.Cut(strings.TrimRight(l, "\r"), "="); ok && strings.TrimSpace(k) == key {
			v = strings.TrimSpace(val)
		}
	}
	return v
}
