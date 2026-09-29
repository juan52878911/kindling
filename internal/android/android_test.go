package android

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
	"github.com/juan52878911/kindling/internal/verity"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/scripts"
)

func TestVerityInit(t *testing.T) {
	s, err := verityInit(scripts.MinimalInit)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(s, "dmsetup create android-layer")
	j := strings.Index(s, verityAnchor)
	if i < 0 || j < i || strings.Count(s, verityAnchor) != 1 {
		t.Fatal("the verity block is not right before the layer mount")
	}
	if _, err := verityInit("#!/bin/sh\n"); err == nil {
		t.Fatal("an init without the anchor was accepted")
	}
}

func TestCommentService(t *testing.T) {
	rc := "service a /bin/a\n    class main\n\n    user root\nservice b /bin/b\n    class late\non boot\n    start a\n"
	out, n := commentService(rc, "a", "kindling-slim")
	want := "# kindling-slim: service a /bin/a\n# kindling-slim:     class main\n\n# kindling-slim:     user root\nservice b /bin/b\n    class late\non boot\n    start a\n"
	if n != 1 || out != want {
		t.Fatalf("%d\n%s", n, out)
	}
	if _, n := commentService(rc, "c", "m"); n != 0 {
		t.Fatal("found a service that is not there")
	}
}

func TestValidate(t *testing.T) {
	ok := Spec{Arch: "arm64", Service: "/usr/local/bin/x"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	s := func(c string) *string { return &c }
	bad := []Spec{
		{Arch: "riscv64", Service: "/x"},
		{Arch: "arm64", Service: "x"},
		{Arch: "arm64", Service: "/x", Files: []File{{Path: "/a/../b", Content: s("")}}},
		{Arch: "arm64", Service: "/x", Files: []File{{Path: "/proc/x", Content: s("")}}},
		{Arch: "arm64", Service: "/x", Files: []File{{Path: "/a", Src: "rel/path"}}},
		{Arch: "arm64", Service: "/x", Files: []File{{Path: "/a", Src: "/x", Content: s("y")}}},
		{Arch: "arm64", Service: "/x", Files: []File{{Path: "/a", Content: s(""), Mode: "999"}}},
		{Arch: "arm64", Service: "/x", Conf: map[string]string{"A": "$(reboot)"}},
		{Arch: "arm64", Service: "/x", Conf: map[string]string{"lower": "1"}},
		{Arch: "arm64", Service: "/x", Env: []string{"A=b\nc"}},
		{Arch: "arm64", Service: "/x", Slim: &Slim{Services: []string{"a b"}}},
		{Arch: "arm64", Service: "/x", Packages: []string{"nonexistent"}},
		{Arch: "arm64", Service: "/x", Redroid: &RedroidSource{Repo: "r", Digest: "latest"}},
	}
	for i, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("case %d accepted: %+v", i, b)
		}
	}
}

func TestConfText(t *testing.T) {
	s := Spec{Conf: map[string]string{"ANDROID_NET": "isolated", "ANDROID_EXTRA_ARGS": "a=1 b=2"}}
	c := s.confText()
	for _, w := range []string{"ANDROID_NET=isolated\n", `ANDROID_EXTRA_ARGS="a=1 b=2"` + "\n", "ANDROID_ROOT=/android\n"} {
		if !strings.Contains(c, w) {
			t.Errorf("android.conf lacks %q:\n%s", w, c)
		}
	}
}

func elf(machine uint16) string {
	b := make([]byte, 64)
	copy(b, "\x7fELF\x02\x01\x01")
	b[18], b[19] = byte(machine), byte(machine>>8)
	return string(b)
}

func tgz(files map[string]string) []byte {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})
	for _, n := range names {
		body := files[n]
		h := &tar.Header{Name: n, Mode: 0o644, Format: tar.FormatPAX}
		switch {
		case strings.HasSuffix(n, "/"):
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		case strings.HasPrefix(body, "->"):
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, body[2:], 0o777
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(body))
			if strings.HasPrefix(body, "\x7fELF") || strings.Contains(n, "bin/") {
				h.Mode = 0o755
			}
		}
		if n == "system/bin/run-as" {
			h.Mode, h.Gid = 0o750, 2000
			h.PAXRecords = map[string]string{"SCHILY.xattr.security.capability": "\x01\x00\x00\x02\xc0\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"}
		}
		tw.WriteHeader(h)
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	return b.Bytes()
}

func fakeDeb(t *testing.T, control, conffiles string, data map[string]string) []byte {
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	member := func(n string, b []byte) {
		fmt.Fprintf(&out, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", n, 0, 0, 0, "100644", len(b))
		out.Write(b)
		if len(b)%2 == 1 {
			out.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.gz", tgz(map[string]string{"./control": control, "./conffiles": conffiles, "./md5sums": "x  usr/sbin/xtables-legacy-multi\n"}))
	member("data.tar.gz", tgz(data))
	return out.Bytes()
}

// TestBuild construye una imagen entera contra un registro y un archivo de
// Debian de mentira: Redroid, la base, dm-verity y la receta.
func TestBuild(t *testing.T) {
	reg := ocitest.New()
	defer reg.Close()
	redroid := tgz(map[string]string{
		"init":                             "->/system/bin/init",
		"system/":                          "",
		"system/bin/init":                  elf(0xb7),
		"system/bin/run-as":                elf(0xb7),
		"system/build.prop":                "ro.build.version.release=13\nro.system.product.cpu.abilist=arm64-v8a\n",
		"vendor":                           "->/system/vendor",
		"system/vendor/build.prop":         "ro.vendor.x=1\n",
		"system/vendor/etc/init/wifi.rc":   "service wifi /vendor/bin/wifi\n    class hal\n",
		"system/vendor/etc/permissions/":   "",
		"system/app/Browser2/Browser2.apk": "zip",
	})
	man, _ := reg.Image("arm64", []string{"/init", "qemu=1", "androidboot.hardware=redroid"}, redroid)
	debianImg := tgz(map[string]string{
		"bin":                 "->usr/bin",
		"sbin":                "->usr/sbin",
		"usr/":                "",
		"usr/bin/":            "",
		"usr/sbin/":           "",
		"usr/bin/sh":          elf(0xb7),
		"usr/share/doc/a/x":   "doc",
		"var/lib/dpkg/status": "Package: base-files\nStatus: install ok installed\nVersion: 13\n",
		"var/log/old.log":     "x",
	})
	dman, _ := reg.Image("arm64", nil, debianImg)
	deb := fakeDeb(t, "Package: iptables\nVersion: 1.8.11-2\nArchitecture: arm64\n", "/etc/iptables.conf\n",
		map[string]string{"./usr/": "", "./usr/sbin/": "", "./usr/sbin/xtables-legacy-multi": elf(0xb7),
			"./usr/sbin/iptables-legacy": "->xtables-legacy-multi", "./etc/": "", "./etc/iptables.conf": "conf\n"})
	sum := sha256.Sum256(deb)
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(deb) }))
	defer pool.Close()

	oldR, oldD := redroidPins["arm64"], debianLock["arm64"]
	defer func() { redroidPins["arm64"], debianLock["arm64"] = oldR, oldD }()
	redroidPins["arm64"] = redroidPin{Repo: reg.Host() + "/redroid/redroid", Tag: "test", Manifest: man}
	debianLock["arm64"] = debianBase{Image: reg.Host() + "/library/debian", Manifest: dman,
		Packages: []debPin{{"iptables", "1.8.11-2", pool.URL + "/pool/iptables.deb", hex.EncodeToString(sum[:]), int64(len(deb))}}}

	root := t.TempDir()
	work := t.TempDir()
	agent := filepath.Join(t.TempDir(), "kling-guest")
	os.WriteFile(agent, []byte(elf(0xb7)), 0o755)
	inputs := t.TempDir()
	launcher := filepath.Join(inputs, "phoned")
	os.WriteFile(launcher, []byte(elf(0xb7)), 0o755)
	// Como root (el daemon de Linux), solo se leen ficheros de aquí.
	t.Setenv("KLING_ANDROID_INPUTS", inputs)
	t.Setenv("KLING_ROOT", root)
	t.Setenv("KLING_GUEST_AGENT", agent)
	t.Setenv("SOURCE_DATE_EPOCH", "1790000000")
	ready := "#!/bin/sh\nexec /usr/local/bin/kling-phoned ready\n"
	spec := Spec{Arch: "arm64", Service: "/usr/local/bin/kling-phoned",
		Files: []File{{Path: "/usr/local/bin/kling-phoned", Src: launcher},
			{Path: "/etc/kindling/ready", Content: &ready, Mode: "0755"}},
		Conf:        map[string]string{"ANDROID_NET": "veth"},
		DataExt4MiB: 64,
		Slim:        &Slim{Prop: "ro.config.low_ram=true # comentario\n", Services: []string{"wifi"}, Apps: []string{"/system/app/Browser2"}, FeaturesXML: "<permissions/>\n"},
	}
	sb, _ := json.Marshal(spec)
	req, _ := json.Marshal(api.BuildImageRequest{Name: "android13", Builder: "android", Spec: sb})
	os.WriteFile(filepath.Join(work, "request.json"), req, 0o600)
	var log bytes.Buffer
	if err := Build(work, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}

	// La receta para el daemon.
	var hints api.BuildRecipeHints
	hb, _ := os.ReadFile(filepath.Join(work, "recipe.json"))
	if err := json.Unmarshal(hb, &hints); err != nil || hints.Base != "android13-base" || hints.CPUPctPerVCPU != 100 || !hints.GuestIPv6Stack {
		t.Fatalf("hints %s %v", hb, err)
	}
	var built struct {
		Table string `json:"verity_table"`
		Root  string `json:"verity_root_hash"`
		Salt  string `json:"verity_salt"`
	}
	json.Unmarshal(hints.Built, &built)

	layerPath := filepath.Join(root, "images", "android13.layer.ext4")
	basePath := filepath.Join(root, "images", "android13-base.ext4")
	fsck(t, layerPath)
	fsck(t, basePath)

	// dm-verity: el árbol y el FEC que hay detrás cuadran con la tabla.
	f, _ := os.Open(layerPath)
	defer f.Close()
	tab := strings.Fields(built.Table)
	nb, _ := strconv.ParseUint(tab[8], 10, 64)
	rh, _ := hex.DecodeString(built.Root)
	salt, _ := hex.DecodeString(built.Salt)
	res := verity.Result{RootHash: rh, Salt: salt, DataBlocks: nb, HashBlocks: verity.HashBlocks(nb), FECRoots: 2}
	res.FECBlocks = verity.FECBlocks(res.FECStart(), 2)
	if err := verity.Verify(f, res); err != nil {
		t.Fatal(err)
	}
	if res.Table("@DEV@") != built.Table {
		t.Fatalf("table %q vs %q", res.Table("@DEV@"), built.Table)
	}

	lt, err := ext4.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	cat := func(root *ext4.Node, p string) string {
		n, _ := root.Resolve(p)
		if n == nil {
			t.Fatalf("%s missing", p)
		}
		b, err := n.ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up := lt.Lookup("/upper")
	if n := up.Lookup("/android/system/bin/run-as"); n == nil || n.Mode != ext4.ModeReg|0o750 || n.GID != 2000 || len(n.Xattrs["security.capability"]) != 20 {
		t.Fatalf("run-as %+v", n)
	}
	if got := cat(up, "/usr/local/lib/kindling-android/entrypoint.args"); got != "qemu=1\nandroidboot.hardware=redroid\n" {
		t.Fatalf("entrypoint.args %q", got)
	}
	if e := cat(up, "/entrypoint"); !strings.Contains(e, "'/usr/local/bin/kling-phoned'") || !strings.Contains(e, "exec /usr/local/bin/kling-guest -listen :8080") {
		t.Fatalf("entrypoint:\n%s", e)
	}
	if !strings.Contains(cat(up, "/usr/local/lib/kindling-android/android.conf"), "ANDROID_NET=veth") {
		t.Fatal("android.conf")
	}
	if n := up.Lookup("/etc/kindling/ready"); n == nil || n.Mode&0o777 != 0o755 {
		t.Fatal("ready probe")
	}
	if n := up.Lookup("/usr/local/lib/kindling-android/data.ext4"); n == nil || n.Size != 64<<20 {
		t.Fatal("data.ext4")
	}
	// slim
	if up.Lookup("/android/system/app/Browser2") != nil {
		t.Fatal("slim did not remove the app")
	}
	andr := up.Lookup("/android")
	if rc := cat(andr, "/vendor/etc/init/wifi.rc"); !strings.HasPrefix(rc, "# kindling-slim: service wifi") {
		t.Fatalf("wifi.rc %q", rc)
	}
	if p := cat(andr, "/vendor/build.prop"); !strings.Contains(p, "# >>> kindling-slim (spec.slim.prop)\nro.config.low_ram=true\n# <<< kindling-slim\n") {
		t.Fatalf("build.prop %q", p)
	}
	if up.Lookup("/android/system/vendor/etc/permissions/kindling-slim.xml") == nil {
		t.Fatal("features.xml")
	}

	// La base: Debian, el paquete, el init con verity y la tabla.
	bf, _ := os.Open(basePath)
	defer bf.Close()
	bt, err := ext4.Read(bf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cat(bt, "/sbin/overlay-init"), "dmsetup create android-layer") {
		t.Fatal("overlay-init without verity")
	}
	if !strings.Contains(cat(bt, "/etc/kindling-android/layer.verity"), built.Table) {
		t.Fatal("layer.verity")
	}
	if st := cat(bt, "/var/lib/dpkg/status"); !strings.Contains(st, "Package: iptables\nStatus: install ok installed\n") ||
		!strings.Contains(st, "Conffiles:\n /etc/iptables.conf ") {
		t.Fatalf("status:\n%s", st)
	}
	if n := bt.Lookup("/usr/sbin/iptables"); n == nil || n.Target != "/etc/alternatives/iptables" {
		t.Fatal("iptables alternative")
	}
	if cat(bt, "/usr/sbin/iptables") != elf(0xb7) {
		t.Fatal("iptables does not resolve to xtables-legacy-multi")
	}
	if bt.Lookup("/usr/share/doc") != nil || bt.Lookup("/var/log/old.log") != nil {
		t.Fatal("base not slimmed")
	}
	if cat(bt, "/var/lib/dpkg/info/iptables.list") == "" {
		t.Fatal("dpkg list")
	}

	// Mismas entradas y SOURCE_DATE_EPOCH: la misma imagen, bit a bit.
	sumOf := func(p string) string {
		b, _ := os.ReadFile(p)
		s := sha256.Sum256(b)
		return hex.EncodeToString(s[:])
	}
	l1, b1 := sumOf(layerPath), sumOf(basePath)
	if err := Build(work, io.Discard); err != nil {
		t.Fatal(err)
	}
	if sumOf(layerPath) != l1 || sumOf(basePath) != b1 {
		t.Fatal("the build is not reproducible")
	}

	// Una base que no es suya no se pisa.
	os.WriteFile(filepath.Join(root, "images", "otra.ext4"), []byte("x"), 0o644)
	spec.BaseName = "otra"
	sb, _ = json.Marshal(spec)
	req, _ = json.Marshal(api.BuildImageRequest{Name: "android13", Builder: "android", Spec: sb})
	os.WriteFile(filepath.Join(work, "request.json"), req, 0o600)
	if err := Build(work, io.Discard); err == nil || !strings.Contains(err.Error(), "not written by the android builder") {
		t.Fatalf("overwrote a foreign base: %v", err)
	}
}

// Como root, el constructor solo lee ficheros del directorio de kindling o de
// KLING_ANDROID_INPUTS (el daemon de Linux lo corre como root).
func TestAllowedSrcAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("only as root")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	os.WriteFile(p, []byte("x"), 0o644)
	b := &builder{lib: "/nonexistent"}
	t.Setenv("KLING_ANDROID_INPUTS", "")
	if _, err := b.allowedSrc(p); err == nil {
		t.Fatal("read a file outside the allowed directories as root")
	}
	if _, err := b.allowedSrc("/etc/passwd"); err == nil {
		t.Fatal("read /etc/passwd as root")
	}
	t.Setenv("KLING_ANDROID_INPUTS", dir)
	if _, err := b.allowedSrc(p); err != nil {
		t.Fatal(err)
	}
	// Un enlace desde dentro hacia fuera no sirve para escapar.
	os.Symlink("/etc/passwd", filepath.Join(dir, "l"))
	if _, err := b.allowedSrc(filepath.Join(dir, "l")); err == nil {
		t.Fatal("followed a symlink out of the allowed directory")
	}
}

func fsck(t *testing.T, img string) {
	t.Helper()
	for _, c := range []string{"e2fsck", "/sbin/e2fsck", "/usr/sbin/e2fsck", "/opt/homebrew/opt/e2fsprogs/sbin/e2fsck"} {
		if p, err := exec.LookPath(c); err == nil {
			out, err := exec.Command(p, "-fn", img).CombinedOutput()
			if err != nil || strings.Contains(string(out), "? no") {
				t.Fatalf("e2fsck -fn %s: %v\n%s", img, err, out)
			}
			return
		}
	}
}
