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
	"github.com/juan52878911/kindling/pkg/guest"
)

// alpineLike es una raíz mínima al estilo de Alpine: /sbin de verdad (no
// enlace) y las herramientas del init como enlaces a busybox: solo las que
// usa (ociInitTools), sin cat ni grep.
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
	for _, t := range []string{"sh", "mount", "mkdir", "ln"} {
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
	// Un agente que sabe hacer de init: lleva la marca, como el de verdad.
	os.WriteFile(agent, []byte(testELF(0x3e)+guest.InitMarker), 0o755)
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
		Ref, Digest, Manifest, Ready, Init string
		Ports, Volumes                     []string
		Service                            api.ServiceSpec
		Layers                             []struct{ Digest string }
	}
	if err := json.Unmarshal(hints.Built, &built); err != nil {
		t.Fatal(err)
	}
	if built.Digest != idx || built.Ready != "tcp 5432" || built.Init != "sh" || len(built.Layers) != 2 || hints.Base != "" ||
		strings.Join(built.Service.Argv, " ") != "docker-entrypoint.sh postgres" || built.Service.StopSignal != "SIGINT" ||
		built.Service.Restart != api.RestartOnFailure || built.Service.ProbeTimeoutSeconds != 0 {
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
	if !strings.Contains(cat("/sbin/overlay-init"), "exec /entrypoint") || cat("/usr/local/bin/kling-guest") != testELF(0x3e)+guest.InitMarker {
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
	if p := cat(api.GuestReadyProbe); !strings.HasPrefix(p, "#!/bin/sh\n") || !strings.Contains(p, "-probe-tcp 127.0.0.1:5432") {
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
	// Y deja al daemon la lista de blobs que usó, para pasarlos a la caché
	// verificada (internal/daemon/builders_cache.go).
	usados, err := os.ReadFile(filepath.Join(e.work, "cache-used"))
	if err != nil || !strings.Contains(string(usados), idx+"\n") || strings.Count(string(usados), "sha256:") != 5 {
		t.Fatalf("cache-used (índice, manifiesto, config y 2 capas): %q %v", usados, err)
	}
}

// El servicio que sale de la configuración de la imagen: un STOPSIGNAL que no
// se entiende pasa a SIGTERM con aviso (no deja la imagen sin servicio), los
// plazos del HEALTHCHECK llegan a la sonda con tope, y -restart manda.
func TestBuildOCIServiceFromConfig(t *testing.T) {
	e := newOCITest(t)
	_, idx := e.reg.ImageConfig("amd64", map[string]any{
		"Cmd": []string{"python3"}, "StopSignal": "SIGNOPE",
		"Healthcheck": map[string]any{"Test": []string{"CMD", "true"}, "Timeout": 2500e6, "StartPeriod": 3600e9,
			"Interval": 5e9, "Retries": 3},
	}, ocitest.TarGz(alpineLike()))
	service := func(spec OCISpec) (api.ServiceSpec, string) {
		t.Helper()
		hints, log, err := e.build("py", spec)
		if err != nil {
			t.Fatalf("%v\n%s", err, log)
		}
		var built struct{ Service api.ServiceSpec }
		if err := json.Unmarshal(hints.Built, &built); err != nil {
			t.Fatal(err)
		}
		return built.Service, log
	}
	svc, log := service(OCISpec{Ref: e.reg.Host() + "/x/py@" + idx, Arch: "amd64"})
	if svc.StopSignal != "" || !strings.Contains(log, "STOPSIGNAL") || !strings.Contains(log, "SIGTERM") {
		t.Fatalf("stop signal %q, log:\n%s", svc.StopSignal, log)
	}
	if svc.Restart != api.RestartOnFailure || svc.ProbeTimeoutSeconds != 3 || svc.ReadyStartPeriodSeconds != api.MaxReadyTimeoutSeconds {
		t.Fatalf("service %+v", svc)
	}
	if svc, _ = service(OCISpec{Ref: e.reg.Host() + "/x/py@" + idx, Arch: "amd64", Restart: api.RestartAlways}); svc.Restart != api.RestartAlways {
		t.Fatalf("restart %q", svc.Restart)
	}
}

func TestOCIReadyTimes(t *testing.T) {
	for _, c := range []struct {
		hc     *oci.Healthcheck
		to, sp int
	}{
		{nil, 0, 0},
		{&oci.Healthcheck{Test: []string{"NONE"}, Timeout: 5e9}, 0, 0},
		{&oci.Healthcheck{Test: []string{"CMD-SHELL", "true"}}, 0, 0},
		{&oci.Healthcheck{Test: []string{"CMD-SHELL", "true"}, Timeout: 30e9, StartPeriod: 1}, 30, 1},
		{&oci.Healthcheck{Test: []string{"CMD", "x"}, Timeout: -1, StartPeriod: 121e9}, 0, 120},
	} {
		if to, sp := ociReadyTimes(c.hc); to != c.to || sp != c.sp {
			t.Errorf("ociReadyTimes(%+v) = %d, %d; want %d, %d", c.hc, to, sp, c.to, c.sp)
		}
	}
}

// readImage lee la imagen construida como árbol, con cat de un fichero.
func readImage(t *testing.T, img string) (*ext4.Node, func(string) string) {
	t.Helper()
	fsckImage(t, img)
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	tree, err := ext4.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return tree, func(p string) string {
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
}

// Una imagen sin sh (distroless, scratch): arranca con el init en Go. Sin
// script de init ni /entrypoint; /sbin/overlay-init es el agente, y las
// sondas son #! al agente que el kernel ejecuta sin shell.
func TestBuildOCIDistroless(t *testing.T) {
	e := newOCITest(t)
	distroless := ocitest.TarGz([]ocitest.File{
		{Name: "app", Body: testELF(0x3e), Mode: 0o755},
		{Name: "etc/", Dir: true, Mode: 0o755},
		{Name: "etc/passwd", Body: "root:x:0:0:root:/root:/sbin/nologin\nnonroot:x:65532:65532::/home/nonroot:/sbin/nologin\n"},
	})
	build := func(hc map[string]any) (string, *ext4.Node, func(string) string, string) {
		t.Helper()
		cfg := map[string]any{"Entrypoint": []string{"/app"}, "User": "65532", "Env": []string{"MSG=it's", "MULTI=a\nb", "BAD=x\ry", "1X=v"},
			"ExposedPorts": map[string]any{"80/tcp": map[string]any{}}}
		if hc != nil {
			cfg["Healthcheck"] = hc
		}
		_, idx := e.reg.ImageConfig("amd64", cfg, distroless)
		hints, log, err := e.build("whoami", OCISpec{Ref: e.reg.Host() + "/x/whoami@" + idx, Arch: "amd64"})
		if err != nil {
			t.Fatalf("%v\n%s", err, log)
		}
		var built struct{ Init, Ready string }
		json.Unmarshal(hints.Built, &built)
		if built.Init != "go" || !strings.Contains(log, "no sh, mount, pivot_root, mkdir, ln") {
			t.Fatalf("init %q, log:\n%s", built.Init, log)
		}
		tree, cat := readImage(t, filepath.Join(e.root, "images", "whoami.ext4"))
		return built.Ready, tree, cat, log
	}

	ready, tree, cat, log := build(map[string]any{"Test": []string{"CMD-SHELL", "wget -q localhost"}, "Timeout": 30e9})
	if n := tree.Lookup("/sbin/overlay-init"); n == nil || !n.IsLink() || n.Target != "/usr/local/bin/kling-guest" {
		t.Fatalf("/sbin/overlay-init: %+v", n)
	}
	if tree.Lookup("/entrypoint") != nil {
		t.Fatal("an /entrypoint (a sh script) in an image without sh")
	}
	// Un ENV de varias líneas se queda (sh y el init en Go lo leen entre
	// comillas); uno con un CR o una clave que no lo es, fuera, con aviso del
	// nombre y sin el valor.
	if !strings.Contains(cat("/etc/kindling/IMAGE.txt"), "\ninit=go\n") || cat("/etc/kling/env") != "export MSG='it'\\''s'\nexport MULTI='a\nb'\n" {
		t.Fatalf("IMAGE.txt or env:\n%s%s", cat("/etc/kindling/IMAGE.txt"), cat("/etc/kling/env"))
	}
	if !strings.Contains(log, "ENV BAD is not") || !strings.Contains(log, "ENV 1X is not") || strings.Contains(log, "x\ry") {
		t.Fatalf("dropped ENV warning:\n%s", log)
	}
	// El CMD-SHELL no se puede correr: la sonda de EXPOSE, sin sus plazos.
	if p := cat(api.GuestReadyProbe); p != "#!/usr/local/bin/kling-guest -probe-tcp=127.0.0.1:80\n" || ready != "tcp 80" ||
		!strings.Contains(log, "HEALTHCHECK needs a shell") {
		t.Fatalf("ready %q, probe:\n%s", ready, p)
	}
	var svc api.ServiceSpec
	if err := json.Unmarshal([]byte(cat(api.GuestServiceSpec)), &svc); err != nil || svc.User != "65532" || svc.ProbeTimeoutSeconds != 0 {
		t.Fatalf("service %+v %v", svc, err)
	}
	if !strings.Contains(cat("/etc/kindling/oci.json"), "wget -q localhost") {
		t.Fatal("oci.json lost the image's HEALTHCHECK")
	}

	// HEALTHCHECK CMD: el argv en JSON, con su plazo.
	ready, _, cat, _ = build(map[string]any{"Test": []string{"CMD", "/app", "-health", "it's"}, "Timeout": 30e9})
	if p := cat(api.GuestReadyProbe); p != "#!/usr/local/bin/kling-guest -exec-json\n[\"/app\",\"-health\",\"it's\"]\n" ||
		ready != "healthcheck: /app -health it's" {
		t.Fatalf("ready %q, probe:\n%s", ready, p)
	}
	var svc2 api.ServiceSpec
	if json.Unmarshal([]byte(cat(api.GuestServiceSpec)), &svc2); svc2.ProbeTimeoutSeconds != 30 {
		t.Fatalf("service %+v", svc2)
	}
}

// Una imagen con sh y su propio /entrypoint arranca con el init en Go, que no
// lo ejecuta: se queda tal cual, para su ENTRYPOINT.
func TestBuildOCIOwnEntrypoint(t *testing.T) {
	e := newOCITest(t)
	own := ocitest.TarGz(append(alpineLike(), ocitest.File{Name: "entrypoint", Body: "#!/bin/sh\nexec \"$@\"\n", Mode: 0o755}))
	_, idx := e.reg.ImageConfig("amd64", map[string]any{"Entrypoint": []string{"/entrypoint"}, "Cmd": []string{"redis-server"},
		"ExposedPorts": map[string]any{"6379/tcp": map[string]any{}}}, own)
	hints, log, err := e.build("x", OCISpec{Ref: e.reg.Host() + "/x/y@" + idx, Arch: "amd64"})
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	var built struct{ Init string }
	json.Unmarshal(hints.Built, &built)
	tree, cat := readImage(t, filepath.Join(e.root, "images", "x.ext4"))
	if built.Init != "go" || !tree.Lookup("/sbin/overlay-init").IsLink() || cat("/entrypoint") != "#!/bin/sh\nexec \"$@\"\n" {
		t.Fatalf("init %q, /entrypoint %q", built.Init, cat("/entrypoint"))
	}
	// Con sh, la sonda sigue siendo la de siempre.
	if p := cat(api.GuestReadyProbe); !strings.HasPrefix(p, "#!/bin/sh\n") || !strings.Contains(p, "-probe-tcp 127.0.0.1:6379") {
		t.Fatalf("probe:\n%s", p)
	}
}

// Un agente anterior al init en Go (sin guest.InitMarker) no hace de init:
// una imagen que lo necesita se niega a construirse, diciendo cuál y qué
// hacer, en vez de salir una imagen que no arranca. Las que van con el script
// de sh no lo necesitan y se construyen igual.
func TestBuildOCIAgenteSinInit(t *testing.T) {
	e := newOCITest(t)
	viejo := filepath.Join(t.TempDir(), "kling-guest-viejo")
	os.WriteFile(viejo, []byte(testELF(0x3e)+api.LayerBootParam), 0o755)
	t.Setenv("KLING_GUEST_AGENT_amd64", viejo)
	t.Setenv("KLING_GUEST_AGENT", viejo)

	distroless := ocitest.TarGz([]ocitest.File{{Name: "app", Body: testELF(0x3e), Mode: 0o755}})
	_, idx := e.reg.ImageConfig("amd64", map[string]any{"Entrypoint": []string{"/app"}}, distroless)
	_, log, err := e.build("d", OCISpec{Ref: e.reg.Host() + "/x/d@" + idx, Arch: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "the guest agent at "+viejo+" predates the Go init") {
		t.Fatalf("an agent without the Go init was accepted: %v\n%s", err, log)
	}
	if _, err := os.Stat(filepath.Join(e.root, "images", "d.ext4")); !os.IsNotExist(err) {
		t.Fatalf("an image was left behind: %v", err)
	}
	// Uno de otra arquitectura dice eso primero, que es lo que hay que arreglar.
	otra := filepath.Join(t.TempDir(), "kling-guest-arm64")
	os.WriteFile(otra, []byte(testELF(0xb7)), 0o755)
	t.Setenv("KLING_GUEST_AGENT_amd64", otra) // en un host arm64
	t.Setenv("KLING_GUEST_AGENT", otra)       // en uno amd64
	if _, _, err := e.build("d", OCISpec{Ref: e.reg.Host() + "/x/d@" + idx, Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "not amd64") {
		t.Fatalf("an arm64 agent for an amd64 image: %v", err)
	}
	t.Setenv("KLING_GUEST_AGENT_amd64", viejo)
	t.Setenv("KLING_GUEST_AGENT", viejo)

	_, idx = e.reg.ImageConfig("amd64", map[string]any{"Cmd": []string{"sh"}}, ocitest.TarGz(alpineLike()))
	if _, log, err := e.build("a", OCISpec{Ref: e.reg.Host() + "/x/a@" + idx, Arch: "amd64"}); err != nil {
		t.Fatalf("an image with sh needs no Go init: %v\n%s", err, log)
	}
}

func TestBuildOCIRejects(t *testing.T) {
	e := newOCITest(t)
	_, idx := e.reg.ImageConfig("arm64", nil, ocitest.TarGz(alpineLike()))
	if _, _, err := e.build("x", OCISpec{Ref: e.reg.Host() + "/x/y@" + idx, Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "no linux/amd64") {
		t.Fatalf("wrong arch: %v", err)
	}
	if _, _, err := e.build("x", OCISpec{Ref: e.reg.Host() + "/x/y@" + idx, Arch: "amd64", MaxMB: 1, Env: []string{"A=1\nB=2"}}); err == nil {
		t.Fatal("multi-line env accepted")
	}
}

// Una imagen de un registro privado: con las credenciales que deja el daemon
// en registry-auth.json se construye, el fichero se borra al leerlo, y la
// contraseña no sale ni en el log ni en recipe.json; sin ellas, el error dice
// cómo darlas (y tampoco la lleva).
func TestBuildOCIRegistroPrivado(t *testing.T) {
	const pass = "s3cr3t-registry-pass"
	e := newOCITest(t)
	e.reg.User, e.reg.Pass = "juan", pass
	_, idx := e.reg.ImageConfig("amd64", map[string]any{"Entrypoint": []string{"/bin/sh"}}, ocitest.TarGz(alpineLike()))
	e.reg.Tag("v1", idx)
	ref := e.reg.Host() + "/priv/app:v1"
	key, _ := oci.CredentialKey(e.reg.Host())
	authFile := filepath.Join(e.work, ficheroCredencialesRegistro)

	if _, log, err := e.build("priv", OCISpec{Ref: ref, Arch: "amd64"}); err == nil ||
		!strings.Contains(err.Error(), "kling registry login "+key) {
		t.Fatalf("sin credenciales: %v\n%s", err, log)
	}

	auth, _ := json.Marshal(map[string]oci.Credential{key: {Username: "juan", Password: pass}, "ghcr.io": {Username: "x", Password: "otra"}})
	os.WriteFile(authFile, auth, 0o600)
	_, log, err := e.build("priv", OCISpec{Ref: ref, Arch: "amd64"})
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if _, err := os.Lstat(authFile); !os.IsNotExist(err) {
		t.Fatalf("el constructor no borró %s: %v", ficheroCredencialesRegistro, err)
	}
	rec, _ := os.ReadFile(filepath.Join(e.work, "recipe.json"))
	if strings.Contains(log, pass) || strings.Contains(string(rec), pass) {
		t.Fatalf("la contraseña salió en el log o en recipe.json:\n%s\n%s", log, rec)
	}

	// Credenciales malas: el error tampoco las cita.
	auth, _ = json.Marshal(map[string]oci.Credential{key: {Username: "juan", Password: "mala-" + pass}})
	os.WriteFile(authFile, auth, 0o600)
	if _, _, err := e.build("priv2", OCISpec{Ref: ref, Arch: "amd64"}); err == nil ||
		!strings.Contains(err.Error(), "were refused") || strings.Contains(err.Error(), pass) {
		t.Fatalf("credenciales malas: %v", err)
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
		{api.BuildImageRequest{Name: "x"}, OCISpec{Ref: "x", Arch: "amd64", Restart: "sometimes"}},
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

// Un tope de la caché por encima del máximo, por entorno, se avisa y se
// queda en el de por defecto: no llega a desbordar en el daemon.
func TestBuildCacheConfigTopes(t *testing.T) {
	t.Setenv("KLING_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KLING_BUILD_CACHE_MAX_GIB", "17179869184")
	t.Setenv("KLING_BUILD_CACHE_MAX_DAYS", "200000")
	if l := buildCacheConfig(); l.MaxGiB != 0 || l.MaxDays != 0 {
		t.Fatalf("%+v", l)
	}
	t.Setenv("KLING_BUILD_CACHE_MAX_GIB", "50")
	t.Setenv("KLING_BUILD_CACHE_MAX_DAYS", "7")
	if l := buildCacheConfig(); l.MaxGiB != 50 || l.MaxDays != 7 {
		t.Fatalf("%+v", l)
	}
}
