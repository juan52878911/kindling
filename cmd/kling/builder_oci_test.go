package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
	"github.com/juan52878911/kindling/pkg/api"
)

// alpineLike es una raíz mínima al estilo de Alpine: /sbin de verdad (no
// enlace) y las herramientas del init como enlaces a busybox.
func alpineLike() []ocitest.File {
	fs := []ocitest.File{
		{Name: "bin/", Dir: true, Mode: 0o755}, {Name: "sbin/", Dir: true, Mode: 0o755}, {Name: "usr/", Dir: true, Mode: 0o755},
		{Name: "usr/bin/", Dir: true, Mode: 0o755}, {Name: "etc/", Dir: true, Mode: 0o755},
		{Name: "bin/busybox", Body: testELF(0x3e), Mode: 0o755},
		{Name: "etc/passwd", Body: "root:x:0:0:root:/root:/bin/sh\npostgres:x:70:70::/var/lib/postgresql:/bin/sh\n"},
		{Name: "usr/local/", Dir: true, Mode: 0o755}, {Name: "usr/local/bin/", Dir: true, Mode: 0o755},
		{Name: "usr/local/bin/docker-entrypoint.sh", Body: "#!/bin/sh\nexec \"$@\"\n", Mode: 0o755},
		{Name: "var/", Dir: true, Mode: 0o755}, {Name: "var/old", Body: "borrado por la capa 2"},
	}
	for _, t := range []string{"sh", "mount", "mkdir", "ln", "cat", "grep"} {
		fs = append(fs, ocitest.File{Name: "bin/" + t, Link: "busybox"})
	}
	return append(fs, ocitest.File{Name: "sbin/pivot_root", Link: "/bin/busybox"})
}

type ociTestEnv struct {
	reg        *ocitest.Registry
	root, work string
}

func newOCITest(t *testing.T) *ociTestEnv {
	t.Helper()
	e := &ociTestEnv{reg: ocitest.New(), root: t.TempDir(), work: t.TempDir()}
	t.Cleanup(e.reg.Close)
	agent := filepath.Join(t.TempDir(), "kling-guest")
	os.WriteFile(agent, []byte(testELF(0x3e)), 0o755)
	t.Setenv("KLING_ROOT", e.root)
	t.Setenv("KLING_GUEST_AGENT", agent)
	t.Setenv("KLING_GUEST_AGENT_amd64", agent)
	t.Setenv("SOURCE_DATE_EPOCH", "1790000000")
	return e
}

func (e *ociTestEnv) build(name string, spec OCISpec) (api.BuildRecipeHints, string, error) {
	sb, _ := json.Marshal(spec)
	req, _ := json.Marshal(api.BuildImageRequest{Name: name, Builder: "oci", Spec: sb})
	os.WriteFile(filepath.Join(e.work, "request.json"), req, 0o600)
	os.Remove(filepath.Join(e.work, "recipe.json"))
	var log bytes.Buffer
	err := buildOCI(context.Background(), e.work, &log)
	var h api.BuildRecipeHints
	if err == nil {
		hb, _ := os.ReadFile(filepath.Join(e.work, "recipe.json"))
		err = json.Unmarshal(hb, &h)
	}
	return h, log.String(), err
}

func TestBuildOCI(t *testing.T) {
	e := newOCITest(t)
	l1 := ocitest.TarGz(alpineLike())
	l2 := ocitest.TarGz([]ocitest.File{{Name: "var/.wh.old", Body: ""}, {Name: "var/lib/postgresql/", Dir: true, Mode: 0o755}})
	_, idx := e.reg.ImageConfig("amd64", map[string]any{
		"Entrypoint": []string{"docker-entrypoint.sh"}, "Cmd": []string{"postgres"},
		"Env":          []string{"PATH=/usr/local/bin:/usr/bin:/bin", "PGDATA=/var/lib/postgresql/data", "LANG=C"},
		"StopSignal":   "SIGINT",
		"WorkingDir":   "/var/lib/postgresql",
		"ExposedPorts": map[string]any{"5432/tcp": map[string]any{}, "9000/udp": map[string]any{}},
		"Volumes":      map[string]any{"/var/lib/postgresql/data": map[string]any{}},
		"Labels":       map[string]any{"org.opencontainers.image.licenses": "PostgreSQL"},
	}, l1, l2)
	e.reg.Tag("17-alpine", idx)
	ref := e.reg.Host() + "/library/postgres:17-alpine"

	spec := OCISpec{Ref: ref, Arch: "amd64", Env: []string{"POSTGRES_PASSWORD=s3cr3t", "LANG=C.UTF-8"}}
	hints, log, err := e.build("pg", spec)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if strings.Contains(log, "s3cr3t") {
		t.Fatalf("the build log shows an env value:\n%s", log)
	}
	var built struct {
		Ref, Digest, Manifest, Ready string
		Ports, Volumes               []string
		Service                      api.ServiceSpec
		Layers                       []struct{ Digest string }
	}
	if err := json.Unmarshal(hints.Built, &built); err != nil {
		t.Fatal(err)
	}
	if built.Digest != idx || built.Ready != "tcp 5432" || len(built.Layers) != 2 || hints.Base != "" ||
		strings.Join(built.Service.Argv, " ") != "docker-entrypoint.sh postgres" || built.Service.StopSignal != "SIGINT" {
		t.Fatalf("built %s", hints.Built)
	}

	img := filepath.Join(e.root, "images", "pg.ext4")
	if _, err := os.Stat(filepath.Join(e.work, "layers")); !os.IsNotExist(err) {
		t.Fatalf("the unpacked layers stayed in the work dir: %v", err)
	}
	fsckImage(t, img)
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tree, err := ext4.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	cat := func(p string) string {
		t.Helper()
		n, _ := tree.Resolve(p)
		if n == nil {
			t.Fatalf("%s missing", p)
		}
		b, err := n.ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if !strings.Contains(cat("/sbin/overlay-init"), "exec /entrypoint") || cat("/usr/local/bin/kling-guest") != testELF(0x3e) {
		t.Fatal("init or agent")
	}
	if tree.Lookup("/sbin").IsLink() || tree.Lookup("/sbin/overlay-init") == nil {
		t.Fatal("/sbin/overlay-init must land in the image's own /sbin")
	}
	if tree.Lookup("/var/old") != nil || tree.Lookup("/var/lib/postgresql") == nil {
		t.Fatal("whiteouts of the second layer not applied")
	}
	ep := cat("/entrypoint")
	if !strings.Contains(ep, ". /etc/kling/env\n") || strings.Contains(ep, "s3cr3t") || !strings.Contains(ep, "exec /usr/local/bin/kling-guest") {
		t.Fatalf("entrypoint:\n%s", ep)
	}
	envFile := cat("/etc/kling/env")
	if n := tree.Lookup("/etc/kling/env"); n.Mode&0o777 != 0o600 || n.UID != 0 {
		t.Fatalf("/etc/kling/env mode %o", n.Mode)
	}
	if !strings.Contains(envFile, "export PGDATA='/var/lib/postgresql/data'\n") ||
		!strings.Contains(envFile, "export LANG='C.UTF-8'\n") || strings.Contains(envFile, "LANG='C'") ||
		!strings.Contains(envFile, "export POSTGRES_PASSWORD='s3cr3t'\n") {
		t.Fatalf("env:\n%s", envFile)
	}
	var svc api.ServiceSpec
	if err := json.Unmarshal([]byte(cat(api.GuestServiceSpec)), &svc); err != nil || svc.WorkingDir != "/var/lib/postgresql" || svc.Argv[0] != "docker-entrypoint.sh" {
		t.Fatalf("service %+v %v", svc, err)
	}
	if p := cat(api.GuestReadyProbe); !strings.Contains(p, "-probe-tcp 127.0.0.1:5432") {
		t.Fatalf("ready probe:\n%s", p)
	}
	if n := tree.Lookup("/tmp"); n == nil || n.Mode&0o1777 != 0o1777 {
		t.Fatal("/tmp")
	}
	for _, d := range []string{"/overlay", "/rom", "/proc", "/sys", "/dev", "/run"} {
		if n := tree.Lookup(d); n == nil || !n.IsDir() {
			t.Fatalf("%s missing", d)
		}
	}

	// Otra vez el mismo digest: nada de capas del registro (solo la
	// etiqueta, que es lo que se resuelve), y la misma imagen bit a bit.
	sum1, _ := os.ReadFile(img)
	hits := e.reg.Hits
	if _, log, err := e.build("pg", spec); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	// Dos peticiones: la etiqueta, con el 401 del token y su reintento.
	if d := e.reg.Hits - hits; d != 2 {
		t.Fatalf("re-import hit the registry %d times, want 2 (the tag and its token retry)", d)
	}
	if sum2, _ := os.ReadFile(img); !bytes.Equal(sum1, sum2) {
		t.Fatal("same inputs, different image")
	}
	// Por digest, ni la etiqueta.
	hits = e.reg.Hits
	spec.Ref = e.reg.Host() + "/library/postgres@" + idx
	if _, log, err := e.build("pg", spec); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if e.reg.Hits != hits {
		t.Fatalf("a pinned re-import hit the registry %d times", e.reg.Hits-hits)
	}

	// Como lo corre el daemon sin root: la imagen en KLING_OUT_DIR y la caché
	// en KLING_CACHE_DIR, nada en $KLING_ROOT.
	out, cache := filepath.Join(e.work, "out"), t.TempDir()
	t.Setenv("KLING_OUT_DIR", out)
	t.Setenv("KLING_CACHE_DIR", cache)
	pinned, _ := os.ReadFile(img)
	os.Remove(img)
	if _, log, err := e.build("pg", spec); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if sum3, _ := os.ReadFile(filepath.Join(out, "pg.ext4")); !bytes.Equal(pinned, sum3) {
		t.Fatal("KLING_OUT_DIR: not the same image")
	}
	if _, err := os.Stat(img); err == nil {
		t.Fatal("KLING_OUT_DIR: the image also landed in $KLING_ROOT/images")
	}
	if blobs, _ := os.ReadDir(filepath.Join(cache, "oci", "sha256")); len(blobs) == 0 {
		t.Fatal("KLING_CACHE_DIR: no blobs in <cache>/oci")
	}
}

func TestBuildOCIRejects(t *testing.T) {
	e := newOCITest(t)
	distroless := ocitest.TarGz([]ocitest.File{{Name: "app", Body: testELF(0x3e), Mode: 0o755}})
	_, idx := e.reg.ImageConfig("amd64", map[string]any{"Entrypoint": []string{"/app"}}, distroless)
	if _, _, err := e.build("x", OCISpec{Ref: e.reg.Host() + "/x/y@" + idx, Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "distroless") {
		t.Fatalf("distroless: %v", err)
	}
	own := ocitest.TarGz(append(alpineLike(), ocitest.File{Name: "entrypoint", Body: "#!/bin/sh\n", Mode: 0o755}))
	_, idx = e.reg.ImageConfig("amd64", map[string]any{"Entrypoint": []string{"/entrypoint"}}, own)
	if _, _, err := e.build("x", OCISpec{Ref: e.reg.Host() + "/x/y@" + idx, Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "/entrypoint") {
		t.Fatalf("own /entrypoint: %v", err)
	}
	_, idx = e.reg.ImageConfig("arm64", nil, ocitest.TarGz(alpineLike()))
	if _, _, err := e.build("x", OCISpec{Ref: e.reg.Host() + "/x/y@" + idx, Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "no linux/amd64") {
		t.Fatalf("wrong arch: %v", err)
	}
	if _, _, err := e.build("x", OCISpec{Ref: e.reg.Host() + "/x/y@" + idx, Arch: "amd64", MaxMB: 1, Env: []string{"A=1\nB=2"}}); err == nil {
		t.Fatal("multi-line env accepted")
	}
}

func TestValidateOCI(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	ok := OCISpec{Ref: "postgres:17-alpine", Arch: "amd64"}
	for i, c := range []struct {
		req  api.BuildImageRequest
		spec OCISpec
	}{
		{api.BuildImageRequest{Name: "Bad Name"}, ok},
		{api.BuildImageRequest{Name: "x", Base: "min"}, ok},
		{api.BuildImageRequest{Name: "x"}, OCISpec{Ref: "x@" + d, Digest: "sha256:" + strings.Repeat("b", 64), Arch: "amd64"}},
		{api.BuildImageRequest{Name: "x"}, OCISpec{Ref: "x", Arch: "riscv64"}},
		{api.BuildImageRequest{Name: "x"}, OCISpec{Ref: "x", Arch: "amd64", User: "root; rm"}},
		{api.BuildImageRequest{Name: "x"}, OCISpec{Ref: "x", Arch: "amd64", MaxMB: -1}},
		{api.BuildImageRequest{Name: "x"}, OCISpec{Ref: "x", Arch: "amd64", Cmd: []string{"a\x00b"}}},
	} {
		if _, err := validateOCI(c.req, c.spec); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	r, err := validateOCI(api.BuildImageRequest{Name: "x"}, OCISpec{Ref: "x:1", Digest: d, Arch: "arm64", User: "70:70"})
	if err != nil || r.Digest != d || r.Tag != "1" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestImageNameFor(t *testing.T) {
	for in, want := range map[string]string{
		"postgres:17-alpine": "postgres-17-alpine", "redis": "redis",
		"docker.io/timescale/timescaledb:latest-pg16": "timescaledb-latest-pg16",
		"ghcr.io/o/My.Server:v1.2":                    "", // repo con mayúsculas: no es una referencia
		"ghcr.io/o/srv:V1.2":                          "srv-v1-2",
	} {
		r, err := oci.ParseImageRef(in)
		if want == "" {
			if err == nil {
				t.Errorf("%s accepted", in)
			}
			continue
		}
		if got := imageNameFor(r); err != nil || got != want {
			t.Errorf("imageNameFor(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestMaskSpecEnv(t *testing.T) {
	got := string(maskSpecEnv(json.RawMessage(`{"ref":"x","env":["A=1","PASS=s3cr3t"]}`)))
	if strings.Contains(got, "s3cr3t") || !strings.Contains(got, `"PASS=***"`) || !strings.Contains(got, `"ref":"x"`) {
		t.Fatal(got)
	}
	if got := string(maskSpecEnv(json.RawMessage(`[1]`))); got != "[1]" {
		t.Fatal(got)
	}
}
