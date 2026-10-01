package main

import (
	"bytes"
	"context"
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
	"strconv"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/imagen"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
	"github.com/juan52878911/kindling/internal/verity"
	"github.com/juan52878911/kindling/pkg/api"
)

func testELF(machine uint16) string {
	b := make([]byte, 64)
	copy(b, "\x7fELF\x02\x01\x01")
	b[18], b[19] = byte(machine), byte(machine>>8)
	return string(b)
}

// testDeb arma un .deb: ar con control.tar.gz y data.tar.gz.
func testDeb(control string, data []ocitest.File) []byte {
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
	member("control.tar.gz", ocitest.TarGz([]ocitest.File{{Name: "./", Dir: true, Mode: 0o755}, {Name: "./control", Body: control}}))
	member("data.tar.gz", ocitest.TarGz(data))
	return out.Bytes()
}

func sum256(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// TestBuildDebian construye una imagen debian entera contra un registro y un
// archivo de Debian de mentira: resuelve, instala en la capa, verity, base y
// receta; y con el lock de la receta sale lo mismo, bit a bit.
func TestBuildDebian(t *testing.T) {
	reg := ocitest.New()
	defer reg.Close()
	slim := ocitest.TarGz([]ocitest.File{
		{Name: "bin", Link: "usr/bin"}, {Name: "sbin", Link: "usr/sbin"}, {Name: "usr/", Dir: true, Mode: 0o755}, {Name: "usr/bin/", Dir: true, Mode: 0o755}, {Name: "usr/sbin/", Dir: true, Mode: 0o755},
		{Name: "usr/bin/sh", Body: testELF(0x3e), Mode: 0o755},
		{Name: "usr/share/doc/a/x", Body: "doc"},
		{Name: "var/lib/dpkg/status", Body: "Package: libc6\nStatus: install ok installed\nVersion: 2.41-12\n"},
	})
	man, _ := reg.Image("amd64", nil, slim)

	debs := map[string][]byte{
		"pool/main/d/dmsetup.deb": testDeb("Package: dmsetup\nVersion: 2:1.02.205-2\nArchitecture: amd64\n",
			[]ocitest.File{{Name: "./usr/", Dir: true, Mode: 0o755}, {Name: "./usr/sbin/", Dir: true, Mode: 0o755}, {Name: "./usr/sbin/dmsetup", Body: testELF(0x3e), Mode: 0o755}}),
		"pool/main/h/hello.deb": testDeb("Package: hello\nVersion: 2.10-5\nArchitecture: amd64\nDepends: libhello1 (>= 1), libc6\n",
			[]ocitest.File{{Name: "./usr/", Dir: true, Mode: 0o755}, {Name: "./usr/bin/", Dir: true, Mode: 0o755}, {Name: "./usr/bin/hello", Body: testELF(0x3e), Mode: 0o755},
				{Name: "./usr/share/doc/hello/copyright", Body: "GPL"}}),
		"pool/main/h/libhello1.deb": testDeb("Package: libhello1\nVersion: 1.0-1\nArchitecture: amd64\nMulti-Arch: same\n",
			[]ocitest.File{{Name: "./usr/", Dir: true, Mode: 0o755}, {Name: "./usr/lib/", Dir: true, Mode: 0o755}, {Name: "./usr/lib/libhello.so.1", Body: "lib"}}),
	}
	var index strings.Builder
	for _, n := range []string{"hello", "libhello1"} {
		b := debs["pool/main/h/"+n+".deb"]
		ctl := map[string]string{"hello": "Version: 2.10-5\nDepends: libhello1 (>= 1), libc6\n", "libhello1": "Version: 1.0-1\n"}[n]
		fmt.Fprintf(&index, "Package: %s\n%sFilename: pool/main/h/%s.deb\nSize: %d\nSHA256: %s\n\n", n, ctl, n, len(b), sum256(b))
	}
	var hits = map[string]int{}
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		hits[p]++
		if p == "dists/trixie/main/binary-amd64/Packages" {
			io.WriteString(w, index.String())
			return
		}
		if b, ok := debs[p]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	defer pool.Close()

	oldL, oldS, oldH := imagen.DebianLock["amd64"], imagen.IndexSources, imagen.PinHosts
	defer func() { imagen.DebianLock["amd64"], imagen.IndexSources, imagen.PinHosts = oldL, oldS, oldH }()
	dm := debs["pool/main/d/dmsetup.deb"]
	imagen.DebianLock["amd64"] = imagen.DebianBase{Image: reg.Host() + "/library/debian", Manifest: man, Snapshot: "20260929T192314Z",
		Packages: []imagen.DebPin{{Name: "dmsetup", Version: "2:1.02.205-2", URL: pool.URL + "/pool/main/d/dmsetup.deb", SHA256: sum256(dm), Size: int64(len(dm))}}}
	imagen.IndexSources = func(snap, arch string) []imagen.IndexSource {
		return []imagen.IndexSource{{URL: pool.URL + "/dists/trixie/main/binary-" + arch + "/Packages", Base: pool.URL}}
	}
	imagen.PinHosts = []string{pool.URL + "/"}

	root, work := t.TempDir(), t.TempDir()
	agent := filepath.Join(t.TempDir(), "kling-guest")
	os.WriteFile(agent, []byte(testELF(0x3e)), 0o755)
	t.Setenv("KLING_ROOT", root)
	t.Setenv("KLING_GUEST_AGENT", agent)
	t.Setenv("KLING_GUEST_AGENT_amd64", agent)
	t.Setenv("SOURCE_DATE_EPOCH", "1790000000")
	build := func(spec DebianSpec) (api.BuildRecipeHints, error) {
		sb, _ := json.Marshal(spec)
		req, _ := json.Marshal(api.BuildImageRequest{Name: "deb1", Builder: "debian", Spec: sb})
		os.WriteFile(filepath.Join(work, "request.json"), req, 0o600)
		var log bytes.Buffer
		err := buildDebian(context.Background(), work, &log)
		var h api.BuildRecipeHints
		if err == nil {
			hb, _ := os.ReadFile(filepath.Join(work, "recipe.json"))
			err = json.Unmarshal(hb, &h)
		}
		if err != nil {
			return h, fmt.Errorf("%w\n%s", err, log.String())
		}
		return h, nil
	}
	spec := DebianSpec{Arch: "amd64", Packages: []string{"hello"}, Env: []string{"GREETING=hi"}, Service: "/usr/bin/hello", Verity: true}
	hints, err := build(spec)
	if err != nil {
		t.Fatal(err)
	}
	if hints.Base != "deb1-base" {
		t.Fatalf("hints %+v", hints)
	}
	var built struct {
		Lock     []imagen.DebPin `json:"lock"`
		Packages []string        `json:"packages"`
		Table    string          `json:"verity_table"`
		Root     string          `json:"verity_root_hash"`
		Salt     string          `json:"verity_salt"`
	}
	if err := json.Unmarshal(hints.Built, &built); err != nil {
		t.Fatal(err)
	}
	if strings.Join(built.Packages, " ") != "hello=2.10-5 libhello1=1.0-1" || len(built.Lock) != 2 {
		t.Fatalf("built %s", hints.Built)
	}

	layerPath := filepath.Join(root, "images", "deb1.layer.ext4")
	basePath := filepath.Join(root, "images", "deb1-base.ext4")
	fsckImage(t, layerPath)
	fsckImage(t, basePath)

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
	lt, err := ext4.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	up := lt.Lookup("/upper")
	if cat(up, "/usr/bin/hello") != testELF(0x3e) || cat(up, "/usr/lib/libhello.so.1") != "lib" {
		t.Fatal("package files")
	}
	if up.Lookup("/usr/share/doc") != nil {
		t.Fatal("layer not slimmed")
	}
	st := cat(up, "/var/lib/dpkg/status")
	if !strings.HasPrefix(st, "Package: libc6\n") || !strings.Contains(st, "Package: dmsetup\nStatus: install ok installed\n") ||
		!strings.Contains(st, "Package: hello\nStatus: install ok installed\n") {
		t.Fatalf("status:\n%s", st)
	}
	if cat(up, "/var/lib/dpkg/info/libhello1:amd64.list") == "" {
		t.Fatal("dpkg list")
	}
	if e := cat(up, "/entrypoint"); !strings.Contains(e, "export GREETING='hi'") || !strings.Contains(e, "'/usr/bin/hello'") {
		t.Fatalf("entrypoint:\n%s", e)
	}
	if up.Lookup("/usr/local/bin/kling-guest") == nil {
		t.Fatal("agent")
	}

	bf, _ := os.Open(basePath)
	defer bf.Close()
	bt, err := ext4.Read(bf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cat(bt, "/sbin/overlay-init"), "dmsetup create kindling-layer") {
		t.Fatal("overlay-init without verity")
	}
	if !strings.Contains(cat(bt, "/etc/kindling/layer.verity"), built.Table) || bt.Lookup("/usr/sbin/dmsetup") == nil {
		t.Fatal("base: layer.verity or dmsetup")
	}
	if bt.Lookup("/usr/bin/hello") != nil {
		t.Fatal("the recipe packages went to the base")
	}
	var baseRec api.ImageRecipe
	rb, _ := os.ReadFile(filepath.Join(root, "images", "deb1-base.recipe.json"))
	if json.Unmarshal(rb, &baseRec) != nil || baseRec.Builder != DebianBaseBuilder {
		t.Fatalf("base recipe %s", rb)
	}

	// Con el lock de la receta: no se baja ningún índice y sale lo mismo.
	sumOf := func(p string) string { b, _ := os.ReadFile(p); return sum256(b) }
	l1, b1 := sumOf(layerPath), sumOf(basePath)
	idx := hits["dists/trixie/main/binary-amd64/Packages"]
	os.RemoveAll(filepath.Join(root, "cache", "debian-index"))
	spec.Lock = built.Lock
	if _, err := build(spec); err != nil {
		t.Fatal(err)
	}
	if hits["dists/trixie/main/binary-amd64/Packages"] != idx {
		t.Fatal("downloaded the index with a lock")
	}
	if sumOf(layerPath) != l1 || sumOf(basePath) != b1 {
		t.Fatal("the build with the lock is not the same image")
	}

	// Sin paquetes ni verity: la capa es el agente y el entrypoint.
	if _, err := build(DebianSpec{Arch: "amd64"}); err != nil {
		t.Fatal(err)
	}
	bf2, _ := os.Open(basePath)
	defer bf2.Close()
	bt2, _ := ext4.Read(bf2)
	if strings.Contains(cat(bt2, "/sbin/overlay-init"), "dmsetup") || bt2.Lookup("/etc/kindling/layer.verity") != nil {
		t.Fatal("verity without verity")
	}

	// Una base que no es suya no se pisa.
	os.WriteFile(filepath.Join(root, "images", "otra.ext4"), []byte("x"), 0o644)
	if _, err := build(DebianSpec{Arch: "amd64", BaseName: "otra"}); err == nil || !strings.Contains(err.Error(), "not written by the debian builder") {
		t.Fatalf("overwrote a foreign base: %v", err)
	}
	// Un servicio que no está.
	if _, err := build(DebianSpec{Arch: "amd64", Service: "/usr/bin/nope"}); err == nil || !strings.Contains(err.Error(), "not an executable") {
		t.Fatalf("missing service accepted: %v", err)
	}
}

func TestValidateDebian(t *testing.T) {
	req := api.BuildImageRequest{Name: "d"}
	pin := imagen.DebPin{Name: "hello", Version: "1", URL: "https://deb.debian.org/debian/pool/h/hello.deb", SHA256: strings.Repeat("a", 64), Size: 1}
	if err := validateDebian(req, DebianSpec{Arch: "amd64", Packages: []string{"hello"}, Lock: []imagen.DebPin{pin}}); err != nil {
		t.Fatal(err)
	}
	r := func(n int) *int { return &n }
	bad := []DebianSpec{
		{Arch: "riscv64"},
		{Arch: "amd64", Packages: []string{"Hello"}},
		{Arch: "amd64", Packages: []string{"a;b"}},
		{Arch: "amd64", Packages: []string{"other"}, Lock: []imagen.DebPin{pin}},
		{Arch: "amd64", Lock: []imagen.DebPin{{Name: "x", Version: "1", URL: "http://10.0.0.1/x.deb", SHA256: pin.SHA256, Size: 1}}},
		{Arch: "amd64", Env: []string{"A=b\nc"}},
		{Arch: "amd64", Service: "rel/path"},
		{Arch: "amd64", Service: "/a/../b"},
		{Arch: "amd64", FECRoots: r(1)},
		{Arch: "amd64", BaseName: "Bad"},
	}
	for i, s := range bad {
		if err := validateDebian(req, s); err == nil {
			t.Errorf("case %d accepted: %+v", i, s)
		}
	}
}

func fsckImage(t *testing.T, img string) {
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
