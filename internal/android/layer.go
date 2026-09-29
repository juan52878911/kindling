package android

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/oci"
)

// LA CAPA. Lo que deja 81-base-image.sh con ROOTFS_DIR y SERVICE: un ext4
// cuyo /upper es el upperdir de un overlay sobre la base, con Redroid en
// /android, el agente de invitado, el /entrypoint que arranca el servicio y
// los ficheros del spec. Aquí no hay overlay: se escribe directamente lo que
// habría quedado en upper. Diferencias con la de build-image.sh: los
// directorios que ya estaban en la base (/etc, /usr/local...) no llevan los
// xattrs trusted.overlay.* que pone la copia hacia arriba del overlay (no hacen
// falta: sin "opaque" el directorio se mezcla con el de la base igual), y un
// bloque de 4 KiB que es todo ceros queda como hueco.

type inputFile struct {
	spec File
	node *ext4.Node
	sha  string
}

// prepareFiles lee y comprueba los ficheros del spec (sin escribir nada).
func (b *builder) prepareFiles() ([]inputFile, error) {
	var out []inputFile
	for _, f := range b.spec.Files {
		n := &ext4.Node{Mode: ext4.ModeReg, UID: uint32(f.UID), GID: uint32(f.GID), Mtime: b.t}
		var mode uint32 = 0o644
		var sum string
		switch {
		case f.Src != "":
			src, err := b.allowedSrc(f.Src)
			if err != nil {
				return nil, fmt.Errorf("file %s: %w", f.Path, err)
			}
			st, err := os.Stat(src)
			if err != nil {
				return nil, fmt.Errorf("file %s: %w", f.Path, err)
			}
			if !st.Mode().IsRegular() {
				return nil, fmt.Errorf("file %s: %s is not a regular file", f.Path, f.Src)
			}
			if st.Mode()&0o111 != 0 {
				mode = 0o755
			}
			if sum, err = sha256Path(src); err != nil {
				return nil, err
			}
			n.Size, n.Data = st.Size(), ext4.HostFile{Path: src}
		case f.Content != nil:
			n.Size, n.Data = int64(len(*f.Content)), ext4.Bytes(*f.Content)
			sum = sha256Hex([]byte(*f.Content))
		default:
			d, _ := base64.StdEncoding.DecodeString(f.ContentBase64)
			n.Size, n.Data = int64(len(d)), ext4.Bytes(d)
			sum = sha256Hex(d)
		}
		if f.Mode != "" {
			m, _ := strconv.ParseUint(f.Mode, 8, 32)
			mode = uint32(m)
		}
		n.Mode |= mode
		if f.SHA256 != "" && f.SHA256 != sum {
			return nil, fmt.Errorf("file %s: sha256 is %s, the spec pins %s", f.Path, sum, f.SHA256)
		}
		out = append(out, inputFile{f, n, sum})
	}
	return out, nil
}

// allowedSrc decide si el constructor puede leer esa ruta del host. Como
// usuario normal (el daemon de un Mac) lee lo que ese usuario pueda. Como
// root solo lo que el administrador puso a propósito en el directorio de
// bibliotecas de kindling o en KLING_ANDROID_INPUTS: si no, cualquiera con
// acceso al socket del daemon metería /etc/shadow del host en una imagen.
func (b *builder) allowedSrc(src string) (string, error) {
	real, err := evalSymlinks(src)
	if err != nil {
		return "", err
	}
	if os.Geteuid() != 0 {
		return real, nil
	}
	dirs := []string{b.lib}
	for _, d := range strings.Split(os.Getenv("KLING_ANDROID_INPUTS"), ":") {
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		rd, err := evalSymlinks(d)
		if err != nil {
			continue
		}
		if strings.HasPrefix(real, strings.TrimSuffix(rd, "/")+"/") {
			return real, nil
		}
	}
	return "", fmt.Errorf("%s is outside %s (as root the builder only reads files from there; set KLING_ANDROID_INPUTS on the daemon to allow another directory)", src, strings.Join(dirs, ", "))
}

func (b *builder) buildLayer(ctx context.Context, dst string, files []inputFile) (map[string]any, error) {
	src := b.redroid()
	img, err := b.oci.Pull(ctx, src.Repo, src.Digest, b.spec.Arch)
	if err != nil {
		return nil, fmt.Errorf("redroid: %w", err)
	}
	if src.Layer != "" && (len(img.Layers) != 1 || img.Layers[0].Digest != src.Layer) {
		return nil, fmt.Errorf("redroid %s: layers are not the pinned %s", src.Digest, src.Layer)
	}
	b.logf("redroid: %s (%d layer(s), %d MiB)", img.Ref, len(img.Layers), totalSize(img)>>20)

	root := ext4.NewDir(0o755, 0, 0, b.t)
	upper, _ := root.MkdirAll("/upper", 0o755, 0, 0, b.t)
	const andr = "/android"
	keep := func(p string, _ int64) bool {
		p = strings.TrimPrefix(p, andr)
		return p == "/system/build.prop" || p == "/vendor/build.prop" || p == "/system/vendor/build.prop" ||
			p == "/system/bin/init" || (b.spec.Slim != nil && strings.HasSuffix(p, ".rc"))
	}
	var streams []ext4.Stream
	for i, l := range img.Layers {
		rc, err := oci.OpenLayer(l)
		if err != nil {
			return nil, err
		}
		err = upper.AddTar(rc, ext4.TarOptions{Prefix: andr, Stream: i, Whiteouts: true, Time: b.t, Keep: keep})
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("redroid layer %s: %w", l.Digest, err)
		}
		l := l
		streams = append(streams, ext4.TarStream(func() (io.ReadCloser, error) { return oci.OpenLayer(l) }))
	}
	android := upper.Lookup(andr)
	info, err := b.checkAndroid(android)
	if err != nil {
		return nil, err
	}
	info["redroid"] = img.Ref
	info["redroid_layers"] = layerDigests(img)

	// Argumentos de /init: ENTRYPOINT + CMD de la imagen menos argv[0].
	args := b.spec.InitArgs
	if len(args) == 0 {
		argv := append(append([]string{}, img.Config.Config.Entrypoint...), img.Config.Config.Cmd...)
		if len(argv) == 0 || argv[0] != "/init" {
			b.logf("warning: unexpected entrypoint %q; using 'qemu=1 androidboot.hardware=redroid'", argv)
			args = []string{"qemu=1", "androidboot.hardware=redroid"}
		} else {
			args = argv[1:]
		}
	}
	b.logf("/init arguments from the image: %s", strings.Join(args, " "))

	if b.spec.Slim != nil {
		if err := b.slim(android); err != nil {
			return nil, err
		}
	}

	// Lo que genera el constructor.
	agent, err := os.ReadFile(b.agent)
	if err != nil {
		return nil, fmt.Errorf("guest agent: %w", err)
	}
	if err := checkELF(agent, b.spec.Arch); err != nil {
		return nil, fmt.Errorf("guest agent %s: %w", b.agent, err)
	}
	b.putFile(upper, "/usr/local/bin/kling-guest", &ext4.Node{Mode: ext4.ModeReg | 0o755, Size: int64(len(agent)), Data: ext4.HostFile{Path: b.agent}})
	b.put(upper, libDir+"/entrypoint.args", []byte(strings.Join(args, "\n")+"\n"), 0o644)
	b.put(upper, libDir+"/android.conf", []byte(b.spec.confText()), 0o644)
	var imgtxt strings.Builder
	fmt.Fprintf(&imgtxt, "redroid=%s\nredroid_tag=%s\nandroid_release=%v\nabilist=%v\nkindling_builder=android\nbuilt_at=%s\n",
		img.Ref, b.redroidTag(), info["android_release"], info["abilist"], b.t.UTC().Format("2006-01-02T15:04:05Z"))
	b.put(upper, libDir+"/IMAGE.txt", []byte(imgtxt.String()), 0o644)
	if b.spec.DataExt4MiB > 0 {
		p, err := b.emptyExt4(b.spec.DataExt4MiB)
		if err != nil {
			return nil, err
		}
		st, _ := os.Stat(p)
		b.putFile(upper, libDir+"/data.ext4", &ext4.Node{Mode: ext4.ModeReg | 0o644, Size: st.Size(), Data: ext4.HostFile{Path: p}})
	}
	for _, f := range files {
		b.putFile(upper, f.spec.Path, f.node)
	}
	b.put(upper, "/entrypoint", []byte(b.entrypoint()), 0o755)
	b.put(upper, "/etc/resolv.conf", []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0o644)
	if len(b.errs) > 0 {
		return nil, b.errs[0]
	}
	svc, _ := upper.Resolve(b.spec.Service)
	if svc == nil || !svc.IsReg() || svc.Mode&0o111 == 0 {
		return nil, fmt.Errorf("service %s is not an executable file in the image (add it to files)", b.spec.Service)
	}

	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stats, err := ext4.Write(f, root, streams, ext4.Options{
		Time: b.t, UUID: b.uuid("layer"), LostFound: true, ZeroHoles: true,
		SlackBlocks: 16 << 20 / ext4.BlockSize, SlackInodes: 256,
	})
	if err != nil {
		return nil, fmt.Errorf("writing the layer: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	b.logf("layer: %d MiB (%d files)", stats.Bytes()>>20, stats.Files)
	info["layer_blocks"] = stats.Blocks
	return info, nil
}

func totalSize(img *oci.Image) int64 {
	var t int64
	for _, l := range img.Layers {
		t += l.Size
	}
	return t
}

func layerDigests(img *oci.Image) []string {
	var out []string
	for _, l := range img.Layers {
		out = append(out, l.Digest)
	}
	return out
}

func (b *builder) redroid() RedroidSource {
	if b.spec.Redroid != nil {
		return *b.spec.Redroid
	}
	p := redroidPins[b.spec.Arch]
	return RedroidSource{Repo: p.Repo, Digest: p.Manifest, Layer: p.Layer}
}

func (b *builder) redroidTag() string {
	if b.spec.Redroid != nil {
		return "custom"
	}
	return redroidPins[b.spec.Arch].Tag
}

func (b *builder) putFile(root *ext4.Node, p string, n *ext4.Node) {
	if n.Mtime.IsZero() {
		n.Mtime = b.t
	}
	if err := root.Put(p, n, b.t); err != nil {
		b.errs = append(b.errs, fmt.Errorf("%s: %w", p, err))
		return
	}
	touchParent(root, p, b.t)
}

// touchParent pone la hora del directorio donde se añadió algo, como haría
// el sistema de ficheros (install en un directorio del rootfs de Android le
// cambia la mtime).
func touchParent(root *ext4.Node, p string, t time.Time) {
	if d, _ := root.Resolve(path.Dir(p)); d != nil && d.IsDir() {
		d.Mtime = t
	}
}

// sq entrecomilla para sh.
func sq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// entrypoint es el /entrypoint de 81-base-image.sh con SERVICE.
func (b *builder) entrypoint() string {
	var e strings.Builder
	e.WriteString("#!/bin/sh\n# Generado por el constructor android de kindling: el agente de invitado es PID 1.\n")
	e.WriteString("export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\nexport HOME=/root\n")
	for _, kv := range b.spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Fprintf(&e, "export %s=%s\n", k, sq(v))
	}
	fmt.Fprintf(&e, "( while :; do %s; echo \"service exited with $?, restarting in 1s\"; sleep 1; done ) </dev/null >>/var/log/service.log 2>&1 &\n", sq(b.spec.Service))
	e.WriteString("exec /usr/local/bin/kling-guest -listen :8080\n")
	return e.String()
}

var reProp = regexp.MustCompile(`(?m)^ro\.(?:system\.|vendor\.)?product\.cpu\.abilist=(.*)$`)

// checkAndroid comprueba lo mismo que build-image.sh: /init es un ELF de la
// arquitectura, hay build.prop y es Android 13.
func (b *builder) checkAndroid(android *ext4.Node) (map[string]any, error) {
	info := map[string]any{}
	if android == nil {
		return nil, fmt.Errorf("the image has no rootfs")
	}
	init, _ := android.Resolve("/init")
	if init == nil || !init.IsReg() {
		return nil, fmt.Errorf("no /init in the Redroid rootfs")
	}
	if data, ok := init.Data.(ext4.Bytes); ok {
		if err := checkELF(data, b.spec.Arch); err != nil {
			return nil, fmt.Errorf("/init: %w: is this really the %s image?", err, b.spec.Arch)
		}
	}
	prop := android.Lookup("/system/build.prop")
	if prop == nil {
		return nil, fmt.Errorf("no /system/build.prop in the rootfs")
	}
	sys, _ := prop.Data.(ext4.Bytes)
	ver := ""
	if m := regexp.MustCompile(`(?m)^ro\.build\.version\.release=(.*)$`).FindSubmatch(sys); m != nil {
		ver = strings.TrimSpace(string(m[1]))
	}
	all := append([]byte{}, sys...)
	if v, _ := android.Resolve("/vendor/build.prop"); v != nil {
		if d, ok := v.Data.(ext4.Bytes); ok {
			all = append(all, d...)
		}
	}
	abis := ""
	if m := reProp.FindSubmatch(all); m != nil {
		abis = strings.TrimSpace(string(m[1]))
	}
	b.logf("Android %s, ABIs: %s", ver, abis)
	if ver != "13" {
		b.logf("warning: expected Android 13 and build.prop says %q", ver)
	}
	if strings.Contains(abis, "armeabi") || abis == "x86" || strings.Contains(abis, "x86,") {
		b.logf("warning: the image has 32-bit ABIs (%s)", abis)
	}
	if apex := android.Lookup("/system/apex"); apex != nil {
		n := 0
		for _, c := range apex.Children() {
			if strings.HasSuffix(c, ".apex") {
				n++
			}
		}
		if n > 0 {
			b.logf("warning: %d unflattened APEX in /system/apex: apexd will need loop devices", n)
		}
	}
	info["android_release"] = ver
	info["abilist"] = abis
	return info, nil
}

func checkELF(b []byte, arch string) error {
	if len(b) < 20 || !bytes.Equal(b[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return fmt.Errorf("not an ELF binary")
	}
	if m := binary.LittleEndian.Uint16(b[18:]); m != elfMachine[arch] {
		return fmt.Errorf("ELF machine %#x is not %s", m, arch)
	}
	return nil
}

func sha256Hex(b []byte) string {
	return fmt.Sprintf("%x", sha256Sum(b))
}

// slim hace lo de prototypes/android/image/slim/apply.sh sobre el árbol.
func (b *builder) slim(android *ext4.Node) error {
	const mark = "kindling-slim"
	s := b.spec.Slim
	clean := func(txt string) []string {
		var out []string
		for _, l := range strings.Split(txt, "\n") {
			if i := strings.Index(l, "#"); i >= 0 {
				l = l[:i]
			}
			if l = strings.TrimRight(l, " \t\r"); l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	vendor, vpath := android.Resolve("/vendor")
	if vendor == nil || !vendor.IsDir() {
		return fmt.Errorf("slim: no /vendor in the rootfs")
	}
	if s.Prop != "" {
		prop := vendor.Child("build.prop")
		if prop == nil {
			return fmt.Errorf("slim: no %s/build.prop", vpath)
		}
		data, ok := prop.Data.(ext4.Bytes)
		if !ok {
			return fmt.Errorf("slim: can't read %s/build.prop", vpath)
		}
		txt := regexp.MustCompile(`(?ms)^# >>> `+mark+`.*?^# <<< `+mark+`\n`).ReplaceAllString(string(data), "")
		name := s.PropName
		if name == "" {
			name = "spec.slim.prop"
		}
		txt += "# >>> " + mark + " (" + name + ")\n" + strings.Join(clean(s.Prop), "\n") + "\n# <<< " + mark + "\n"
		prop.Data, prop.Size, prop.Mtime = ext4.Bytes(txt), int64(len(txt)), b.t
	}
	if len(s.Services) > 0 {
		var rcs []*ext4.Node
		var rcNames []string
		for _, d := range []string{"/system/etc/init", "/system/etc/init/hw", "/vendor/etc/init", "/odm/etc/init", "/product/etc/init", "/system_ext/etc/init"} {
			dir, dp := android.Resolve(d)
			if dir == nil || !dir.IsDir() {
				continue
			}
			for _, c := range dir.Children() {
				if n := dir.Child(c); n.IsReg() && strings.HasSuffix(c, ".rc") {
					rcs, rcNames = append(rcs, n), append(rcNames, path.Join(dp, c))
				}
			}
		}
		if apex := android.Lookup("/system/apex"); apex != nil {
			for _, a := range apex.Children() {
				etc := apex.Child(a).Child("etc")
				if etc == nil || !etc.IsDir() {
					continue
				}
				for _, c := range etc.Children() {
					if n := etc.Child(c); n.IsReg() && strings.HasSuffix(c, ".rc") {
						rcs, rcNames = append(rcs, n), append(rcNames, "/system/apex/"+a+"/etc/"+c)
					}
				}
			}
		}
		for _, svc := range s.Services {
			hits := 0
			for i, rc := range rcs {
				data, ok := rc.Data.(ext4.Bytes)
				if !ok {
					return fmt.Errorf("slim: %s was not loaded", rcNames[i])
				}
				out, n := commentService(string(data), svc, mark)
				if n == 0 {
					continue
				}
				hits += n
				rc.Data, rc.Size, rc.Mtime = ext4.Bytes(out), int64(len(out)), b.t
			}
			if hits == 0 {
				b.logf("slim: warning: service %s not found", svc)
			}
		}
	}
	for _, app := range s.Apps {
		if n, p := android.Resolve(app); n != nil && n.IsDir() {
			android.Remove(p)
			touchParent(android, p, b.t)
		}
	}
	if s.FeaturesXML != "" {
		perm, _ := vendor.MkdirAll("/etc/permissions", 0o755, 0, 0, b.t)
		if perm == nil {
			return fmt.Errorf("slim: no %s/etc/permissions", vpath)
		}
		perm.SetChild(mark+".xml", &ext4.Node{Mode: ext4.ModeReg | 0o644, Mtime: b.t, Size: int64(len(s.FeaturesXML)), Data: ext4.Bytes(s.FeaturesXML)})
		perm.Mtime = b.t
	}
	b.logf("slim: %d props lines, %d services, %d apps, features %v", len(clean(s.Prop)), len(s.Services), len(s.Apps), s.FeaturesXML != "")
	return nil
}

// commentService comenta el bloque "service <svc> ..." hasta la siguiente
// sección (la primera línea que no empieza por espacio), como el awk de
// apply.sh. Devuelve el texto y cuántos bloques comentó.
func commentService(txt, svc, mark string) (string, int) {
	lines := strings.SplitAfter(txt, "\n")
	in, hits := false, 0
	for i, l := range lines {
		body := strings.TrimSuffix(l, "\n")
		switch {
		case strings.HasPrefix(body, "service "+svc+" "):
			in = true
			hits++
			lines[i] = "# " + mark + ": " + l
		case in && body != "" && body[0] != ' ' && body[0] != '\t':
			in = false
		case in && strings.TrimSpace(body) != "":
			lines[i] = "# " + mark + ": " + l
		}
	}
	return strings.Join(lines, ""), hits
}
