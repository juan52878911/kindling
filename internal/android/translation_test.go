package android

import (
	"archive/tar"
	"archive/zip"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/juan52878911/kindling/internal/ext4"
)

func TestFixABIs(t *testing.T) {
	for _, c := range []struct{ in, bits, mode, want string }{
		{"x86_64,arm64-v8a", "", TranslationNone, "x86_64"},
		{"x86_64,arm64-v8a", "64", TranslationNone, "x86_64"},
		{"x86_64", "", TranslationLibndk, "x86_64,arm64-v8a"},
		{"x86_64,arm64-v8a", "64", TranslationLibndk, "x86_64,arm64-v8a"},
		{"", "32", TranslationLibndk, ""},
		{"x86_64,x86,arm64-v8a,armeabi-v7a,armeabi", "", TranslationNone, "x86_64,x86"},
	} {
		if got := fixABIs(c.in, c.bits, c.mode); got != c.want {
			t.Errorf("fixABIs(%q, %q, %s) = %q, want %q", c.in, c.bits, c.mode, got, c.want)
		}
	}
}

func TestValidateTranslation(t *testing.T) {
	ok := func(arch, tr string) error {
		s := Spec{Arch: arch, Service: "/usr/local/bin/x", ARMTranslation: tr}
		return s.Validate()
	}
	for _, tr := range []string{"", "none", "libndk", "redroid"} {
		if err := ok("amd64", tr); err != nil {
			t.Errorf("amd64 %q: %v", tr, err)
		}
	}
	for _, tr := range []string{"", "none"} {
		if err := ok("arm64", tr); err != nil {
			t.Errorf("arm64 %q: %v", tr, err)
		}
	}
	for _, tr := range []string{"libndk", "redroid"} {
		if err := ok("arm64", tr); err == nil || !strings.Contains(err.Error(), "natively") {
			t.Errorf("arm64 %q accepted: %v", tr, err)
		}
	}
	if err := ok("amd64", "houdini"); err == nil {
		t.Error("houdini accepted")
	}
	if s := (&Spec{Arch: "amd64"}); s.translation() != TranslationNone {
		t.Errorf("amd64 default %q", s.translation())
	}
	if s := (&Spec{Arch: "arm64", ARMTranslation: "none"}); s.translation() != "" {
		t.Errorf("arm64 %q", s.translation())
	}
}

const testRunner = ":arm64_exe:M::\\x7fELF\\x02\\x01\\x01\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\x02\\x00\\xb7::/system/bin/ndk_translation_program_runner_binfmt_misc_arm64:P\n"

// systemTree es un "system" de emulador en pequeño: lo de la traducción y
// cosas que no lo son.
func systemTree(t0 time.Time) *ext4.Node {
	root := ext4.NewDir(0o755, 0, 0, t0)
	put := func(p, data string, mode, gid uint32) {
		if err := root.Put(p, &ext4.Node{Mode: ext4.ModeReg | mode, GID: gid, Mtime: t0, Size: int64(len(data)), Data: ext4.Bytes(data)}, t0); err != nil {
			panic(err)
		}
	}
	for _, p := range []string{"/system/lib64/libndk_translation.so", "/system/lib64/libndk_translation_proxy_libc.so",
		"/system/lib64/arm64/libc.so", "/system/lib64/arm64/libdl.so", "/system/lib64/arm64/cpuinfo", "/system/lib64/libc.so"} {
		put(p, "lib "+p, 0o644, 0)
	}
	put("/system/bin/arm64/linker64", elf(0xb7), 0o755, 2000)
	put("/system/bin/ndk_translation_program_runner_binfmt_misc_arm64", elf(0x3e), 0o755, 2000)
	put("/system/etc/binfmt_misc/arm64_exe", testRunner, 0o644, 0)
	put("/system/etc/binfmt_misc/arm64_dyn", strings.Replace(testRunner, "\\x02\\x00\\xb7", "\\x03\\x00\\xb7", 1), 0o644, 0)
	put("/system/etc/binfmt_misc/arm_exe", "arm", 0o644, 0)
	put("/system/etc/init/ndk_translation.rc", "on early-init\n", 0o644, 0)
	put("/system/etc/ld.config.arm64.txt", "dir.system=/system\n", 0o644, 0)
	put("/system/build.prop", "ro.build.version.release=14\n", 0o600, 0)
	return root
}

// fakeDisk envuelve un ext4 en un disco GPT con "super" y dos particiones
// dinámicas; la de "system" en dos tramos, el segundo ANTES en el disco.
func fakeDisk(t *testing.T, sys []byte) []byte {
	le := binary.LittleEndian
	for len(sys)%sector != 0 {
		sys = append(sys, 0)
	}
	n := int64(len(sys) / sector)
	n1 := n / 2
	superLBA := int64(64)
	superOff := superLBA * sector
	// system: [0,n1) en el sector 8192 del super, [n1,n) en el 4096.
	size := superOff + (8192+n1)*sector + 4096
	disk := make([]byte, size)
	h := disk[sector:]
	copy(h, "EFI PART")
	le.PutUint64(h[72:], 2)
	le.PutUint32(h[80:], 4)
	le.PutUint32(h[84:], 128)
	for i, p := range []struct {
		name        string
		first, last int64
	}{{"vbmeta", 40, 47}, {"super", superLBA, size/sector - 1}} {
		e := disk[2*sector+i*128:]
		le.PutUint64(e[32:], uint64(p.first))
		le.PutUint64(e[40:], uint64(p.last))
		for j, c := range utf16.Encode([]rune(p.name)) {
			le.PutUint16(e[56+2*j:], c)
		}
	}
	g := disk[superOff+lpReserved:]
	le.PutUint32(g, lpGeometryMagic)
	le.PutUint32(g[4:], 52)
	le.PutUint32(g[40:], 65536)
	le.PutUint32(g[44:], 2)
	le.PutUint32(g[48:], 4096)
	hd := disk[superOff+lpReserved+2*lpGeometrySize:]
	le.PutUint32(hd, lpHeaderMagic)
	le.PutUint16(hd[4:], 10)
	le.PutUint32(hd[8:], 128)
	tables := hd[128:]
	// particiones: vendor (tramo 0), system (tramos 1 y 2)
	for i, p := range []struct {
		name         string
		first, count uint32
	}{{"vendor", 0, 1}, {"system", 1, 2}} {
		e := tables[i*52:]
		copy(e, p.name)
		le.PutUint32(e[40:], p.first)
		le.PutUint32(e[44:], p.count)
	}
	xo := 2 * 52
	for i, x := range []struct{ n, at int64 }{{8, 2048}, {n1, 8192}, {n - n1, 4096}} {
		e := tables[xo+i*24:]
		le.PutUint64(e, uint64(x.n))
		le.PutUint64(e[12:], uint64(x.at))
	}
	tsz := xo + 3*24
	le.PutUint32(hd[44:], uint32(tsz))
	for i, d := range [][3]uint32{{0, 2, 52}, {uint32(xo), 3, 24}, {uint32(tsz), 0, 48}, {uint32(tsz), 0, 64}} {
		le.PutUint32(hd[80+12*i:], d[0])
		le.PutUint32(hd[84+12*i:], d[1])
		le.PutUint32(hd[88+12*i:], d[2])
	}
	copy(disk[superOff+2048*sector:], "vendor junk")
	copy(disk[superOff+8192*sector:], sys[:n1*sector])
	copy(disk[superOff+4096*sector:], sys[n1*sector:])
	return disk
}

func ext4Bytes(t *testing.T, root *ext4.Node, t0 time.Time) []byte {
	p := filepath.Join(t.TempDir(), "sys.ext4")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ext4.Write(f, root, nil, ext4.Options{Time: t0, LostFound: true}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	b, _ := os.ReadFile(p)
	return b
}

func writeZip(t *testing.T, p string, files map[string][]byte) {
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, data := range files {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func tarNames(t *testing.T, p string) []string {
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
}

func TestExtractLibndk(t *testing.T) {
	t0 := time.Unix(1790000000, 0).UTC()
	disk := fakeDisk(t, ext4Bytes(t, systemTree(t0), t0))
	dir := t.TempDir()
	zp := filepath.Join(dir, "img.zip")
	writeZip(t, zp, map[string][]byte{"x86_64/system.img": disk, "x86_64/build.prop": []byte("x")})
	dst := filepath.Join(dir, "libndk.tar")
	n, err := extractLibndk(zp, "x86_64/system.img", "system", dst)
	if err != nil {
		t.Fatal(err)
	}
	got := tarNames(t, dst)
	want := []string{"system/bin/arm64/", "system/bin/arm64/linker64", "system/bin/ndk_translation_program_runner_binfmt_misc_arm64",
		"system/etc/binfmt_misc/", "system/etc/binfmt_misc/arm64_dyn", "system/etc/binfmt_misc/arm64_exe",
		"system/etc/init/ndk_translation.rc", "system/etc/ld.config.arm64.txt",
		"system/lib64/arm64/", "system/lib64/arm64/cpuinfo", "system/lib64/arm64/libc.so", "system/lib64/arm64/libdl.so",
		"system/lib64/libndk_translation.so", "system/lib64/libndk_translation_proxy_libc.so"}
	sort.Strings(got)
	if n != len(want) || strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("tar (%d):\n%s\nwant:\n%s", n, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Lo mismo otra vez: el tar sale igual.
	a, _ := os.ReadFile(dst)
	if _, err := extractLibndk(zp, "x86_64/system.img", "system", dst+"2"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst + "2"); string(a) != string(b) {
		t.Fatal("the libndk tar is not deterministic")
	}
	// Sin el lanzador de binfmt_misc, no vale.
	tree := systemTree(t0)
	tree.Remove("/system/bin/ndk_translation_program_runner_binfmt_misc_arm64")
	writeZip(t, zp, map[string][]byte{"x86_64/system.img": fakeDisk(t, ext4Bytes(t, tree, t0))})
	if _, err := extractLibndk(zp, "x86_64/system.img", "system", dst+"3"); err == nil || !strings.Contains(err.Error(), "ndk_translation_program_runner") {
		t.Fatalf("incomplete image accepted: %v", err)
	}
	if _, err := extractLibndk(zp, "x86_64/system.img", "product", dst+"4"); err == nil || !strings.Contains(err.Error(), `no dynamic partition "product"`) {
		t.Fatalf("missing partition: %v", err)
	}
}

// redroidTree es el rootfs de Redroid 13 amd64 en lo que toca a la
// traducción: su puente (libnb.so) y las ABIs repetidas en system, vendor y odm.
func redroidTree(t0 time.Time) *ext4.Node {
	a := ext4.NewDir(0o755, 0, 0, t0)
	put := func(p, data string) {
		a.Put(p, &ext4.Node{Mode: ext4.ModeReg | 0o644, Mtime: t0, Size: int64(len(data)), Data: ext4.Bytes(data)}, t0)
	}
	put("/system/build.prop", "ro.system.product.cpu.abilist=x86_64,arm64-v8a\nro.system.product.cpu.abilist32=\nro.system.product.cpu.abilist64=x86_64,arm64-v8a\nro.dalvik.vm.native.bridge=0\n")
	put("/vendor/build.prop", "ro.vendor.product.cpu.abilist=x86_64,arm64-v8a\nro.vendor.product.cpu.abilist64=x86_64,arm64-v8a\nro.enable.native.bridge.exec=1\nro.dalvik.vm.isa.arm64=x86_64\nro.dalvik.vm.native.bridge=libnb.so\n")
	put("/vendor/odm/etc/build.prop", "ro.odm.product.cpu.abilist=x86_64,arm64-v8a\nro.odm.product.cpu.abilist64=x86_64,arm64-v8a\n")
	a.Put("/odm", &ext4.Node{Mode: ext4.ModeLink | 0o777, Target: "/vendor/odm", Mtime: t0}, t0)
	a.Put("/system/lib64/libnb.so", &ext4.Node{Mode: ext4.ModeLink | 0o777, Target: "libndk_translation.so", Mtime: t0}, t0)
	for _, p := range []string{"/system/lib64/libndk_translation.so", "/system/lib64/libndk_translation_proxy_libEGL.so",
		"/system/lib64/arm64/libc.so", "/system/bin/arm64/linker64", "/system/bin/ndk_translation_program_runner_binfmt_misc_arm64",
		"/system/etc/binfmt_misc/arm64_exe", "/system/etc/init/ndk_translation.rc", "/system/lib64/libc.so"} {
		put(p, "redroid "+p)
	}
	return a
}

func TestArmTranslation(t *testing.T) {
	t0 := time.Unix(1790000000, 0).UTC()
	cache := t.TempDir()
	// La caché ya tiene el tar: no se baja nada.
	os.MkdirAll(filepath.Join(cache, "android"), 0o755)
	tp := filepath.Join(cache, "android", "libndk-"+libndkPin.SHA256[:16]+"-"+libndkCacheVersion+".tar")
	if _, err := writeLibndkTar(systemTree(t0), tp); err != nil {
		t.Fatal(err)
	}
	prop := func(a *ext4.Node, p string) string {
		n, _ := a.Resolve(p)
		b, _ := n.ReadAll()
		return string(b)
	}
	for _, mode := range []string{"", TranslationNone, TranslationLibndk, TranslationRedroid} {
		b := &builder{spec: Spec{Arch: "amd64", ARMTranslation: mode}, cache: cache, t: t0, log: io.Discard}
		a := redroidTree(t0)
		info, err := b.armTranslation(t.Context(), a)
		if err != nil {
			t.Fatalf("%q: %v", mode, err)
		}
		abis, bridge := androidABIs(a)
		switch mode {
		case "", TranslationNone:
			if info["mode"] != TranslationNone || abis != "x86_64" || bridge != "" {
				t.Fatalf("none: %v %q %q", info, abis, bridge)
			}
			for _, p := range []string{"/system/lib64/libnb.so", "/system/lib64/libndk_translation.so", "/system/lib64/arm64", "/system/etc/binfmt_misc"} {
				if a.Lookup(p) != nil {
					t.Fatalf("none: %s still there", p)
				}
			}
			if a.Lookup("/system/lib64/libc.so") == nil {
				t.Fatal("none removed libc.so")
			}
			v := prop(a, "/vendor/build.prop")
			if !strings.Contains(v, "ro.dalvik.vm.native.bridge=0\n") || !strings.Contains(v, "ro.enable.native.bridge.exec=0\n") ||
				!strings.Contains(v, "# kindling-arm-translation: ro.dalvik.vm.isa.arm64=x86_64\n") || strings.Contains(v, "arm64-v8a") {
				t.Fatalf("none vendor build.prop:\n%s", v)
			}
			if o := prop(a, "/odm/etc/build.prop"); strings.Contains(o, "arm64-v8a") {
				t.Fatalf("none odm build.prop:\n%s", o)
			}
		case TranslationLibndk:
			if abis != "x86_64,arm64-v8a" || bridge != "libndk_translation.so" || info["version"] != libndkPin.Version {
				t.Fatalf("libndk: %v %q %q", info, abis, bridge)
			}
			if a.Lookup("/system/lib64/libnb.so") != nil || a.Lookup("/system/lib64/libndk_translation_proxy_libEGL.so") != nil {
				t.Fatal("libndk kept Redroid's bridge")
			}
			if got := prop(a, "/system/lib64/libndk_translation.so"); got != "lib /system/lib64/libndk_translation.so" {
				t.Fatalf("libndk_translation.so is %q", got)
			}
			if n := a.Lookup("/system/bin/arm64/linker64"); n == nil || n.GID != 2000 || n.Mode != ext4.ModeReg|0o755 {
				t.Fatalf("linker64 %+v", n)
			}
			if w := a.Lookup(binfmtWrapper); w == nil || w.Mode&0o111 == 0 {
				t.Fatal("no binfmt wrapper")
			}
			if e := prop(a, "/system/etc/binfmt_misc/arm64_exe"); !strings.HasSuffix(e, "::"+binfmtWrapper+":P\n") {
				t.Fatalf("arm64_exe %q", e)
			}
			if a.Lookup("/system/etc/binfmt_misc/arm_exe") != nil {
				t.Fatal("32-bit binfmt copied")
			}
			v := prop(a, "/vendor/build.prop")
			for _, l := range []string{"ro.dalvik.vm.native.bridge=libndk_translation.so\n", "ro.enable.native.bridge.exec=1\n",
				"ro.dalvik.vm.isa.arm64=x86_64\n", "ro.ndk_translation.version=" + libndkPin.Version + "\n", "# >>> kindling-arm-translation (libndk)\n"} {
				if !strings.Contains(v, l) {
					t.Fatalf("libndk vendor build.prop lacks %q:\n%s", l, v)
				}
			}
			if s := prop(a, "/system/build.prop"); !strings.Contains(s, "ro.dalvik.vm.native.bridge=libndk_translation.so\n") {
				t.Fatalf("system build.prop:\n%s", s)
			}
		case TranslationRedroid:
			if abis != "x86_64,arm64-v8a" || bridge != "libnb.so" || a.Lookup("/system/lib64/libnb.so") == nil {
				t.Fatalf("redroid touched the tree: %q %q", abis, bridge)
			}
		}
	}
	// arm64: nada que hacer.
	b := &builder{spec: Spec{Arch: "arm64"}, cache: cache, t: t0, log: io.Discard}
	a := redroidTree(t0)
	if info, err := b.armTranslation(t.Context(), a); err != nil || info["mode"] != "native" || a.Lookup("/system/lib64/libnb.so") == nil {
		t.Fatalf("arm64: %v %v", info, err)
	}
}
