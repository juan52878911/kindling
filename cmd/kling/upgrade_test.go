package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/internal/upgrade"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// `upgrade -schemas` es lo que otro kling le pregunta a este antes de
// instalarlo: su versión y lo que sabe leer, sin hablar con ningún daemon.
func TestUpgradeSchemas(t *testing.T) {
	t.Setenv("KLING_HOST", "unix:///nonexistent/kling.sock")
	out, err := salida(t, func() error { return cmdUpgrade([]string{"-schemas"}) })
	if err != nil {
		t.Fatal(err)
	}
	var ib upgrade.InfoBinario
	if err := json.Unmarshal([]byte(out), &ib); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if ib.Kling != Version || ib.Esquemas != machine.EsquemasSoportados() || ib.API == 0 {
		t.Errorf("%+v", ib)
	}
}

func TestUpgradeRechazaArgumentos(t *testing.T) {
	for _, args := range [][]string{{"v1.2.3"}, {"-rollback", "-tag", "v1.2.3"}} {
		if _, err := salida(t, func() error { return cmdUpgrade(args) }); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestProgramaLaunchd(t *testing.T) {
	out := []byte("gui/501/dev.kindling.daemon = {\n\tactive count = 1\n\tpath = /Users/j/Library/LaunchAgents/dev.kindling.daemon.plist\n\tprogram = /Users/j/.local/bin/kling\n\targuments = {\n\t\t/Users/j/.local/bin/kling\n\t\tdaemon\n\t}\n")
	if got := programaLaunchd(out); got != "/Users/j/.local/bin/kling" {
		t.Errorf("got %q", got)
	}
	if got := programaLaunchd([]byte("nothing")); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestArgsRemotos(t *testing.T) {
	if got := argsRemotos("v0.18.0", false, false, false, "kling", ""); got != " -tag v0.18.0" {
		t.Errorf("%q", got)
	}
	if got := argsRemotos("", true, false, false, "kling", ""); got != " -rollback" {
		t.Errorf("%q", got)
	}
	// Lo que cambia lo que hace no se pierde: un -dry-run olvidado es una
	// actualización de verdad al pegar la orden.
	if got := argsRemotos("v0.18.0", false, true, true, "kt", "/srv/kt"); got != " -tag v0.18.0 -dry-run -force -unit kt -root /srv/kt" {
		t.Errorf("%q", got)
	}
}

// El daemon del socket tiene que ser el del servicio: con KLING_HOST en un
// daemon privado, upgrade no puede reiniciar el de producción.
func TestMismoProceso(t *testing.T) {
	if err := mismoProceso("/run/kling.sock", "kling.service", 42, 42); err != nil {
		t.Error(err)
	}
	if err := mismoProceso("/run/kt/kling.sock", "kling.service", 42, 77); err == nil || !strings.Contains(err.Error(), "pid 77") {
		t.Errorf("another daemon accepted: %v", err)
	}
	if err := mismoProceso("/run/kt/kling.sock", "kling.service", 0, 77); err == nil {
		t.Error("a unit that is not running accepted")
	}
}

// pidDelPar lo dice el kernel: un socket que escucha este proceso da su PID.
func TestPidDelPar(t *testing.T) {
	dir, err := os.MkdirTemp("", "kp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			c.Read(make([]byte, 1))
		}
	}()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pid, err := pidDelPar(c.(*net.UnixConn))
	if err != nil || pid != os.Getpid() {
		t.Fatalf("pid %d, %v; want %d", pid, err, os.Getpid())
	}
}

func TestPidLaunchd(t *testing.T) {
	if got := pidLaunchd([]byte("gui/501/dev.kindling.daemon = {\n\tstate = running\n\tpid = 4242\n")); got != 4242 {
		t.Errorf("got %d", got)
	}
	if got := pidLaunchd([]byte("state = not running\n")); got != 0 {
		t.Errorf("got %d", got)
	}
}

// extFalsa es una extensión de mentira: un sh que imprime su manifiesto.
func extFalsa(version string) []byte {
	return []byte("#!/bin/sh\nif [ \"$1\" = --kling-manifest ]; then echo '" +
		`{"manifest_version":1,"name":"demo","version":"` + version + `","commands":[{"name":"demo"}],"companions":["kling-demo-helper"]}` +
		"'; exit 0; fi\n")
}

// Tras actualizar el núcleo a una release (que se llama vX.Y.Z aunque el
// binario diga X.Y.Z, como lo construye release.yml), las extensiones
// instaladas pasan a esa release con sus compañeros, y lo de antes queda en
// la copia: -rollback lo devuelve también.
func TestActualizarExtensiones(t *testing.T) {
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	asset := func(n string) string { return n + "-" + runtime.GOOS + "-" + runtime.GOARCH }
	nueva, ayudaNueva := extFalsa("1.1.0"), []byte("#!/bin/sh\necho helper 1.1.0\n")
	rel := map[string][]byte{asset("kling-demo"): nueva, asset("kling-demo-helper"): ayudaNueva}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, ok := strings.CutPrefix(r.URL.Path, "/v1.1.0/")
		switch {
		case !ok:
			http.NotFound(w, r)
		case n == "SHA256SUMS":
			for a, b := range rel {
				fmt.Fprintf(w, "%s  %s\n", sum(b), a)
			}
		case rel[n] != nil:
			w.Write(rel[n])
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// El núcleo: un kling 1.0.0 que se actualiza a 1.1.0 desde un directorio.
	base := t.TempDir()
	bin, nuevos := filepath.Join(base, "kling"), filepath.Join(base, "nuevo")
	os.Mkdir(nuevos, 0o755)
	klingSh := func(v string) []byte {
		return []byte("#!/bin/sh\n[ \"$1\" = version ] && echo 'kling " + v + "' && exit 0\nexit 2\n")
	}
	os.WriteFile(bin, klingSh("1.0.0"), 0o755)
	os.WriteFile(filepath.Join(nuevos, "kling"), klingSh("1.1.0"), 0o755)
	o := upgrade.Opciones{Dir: filepath.Join(base, "upgrade"), Fuente: &upgrade.Fuente{Dir: nuevos}, Actual: "1.0.0",
		Piezas: []upgrade.Pieza{{Asset: assetDe("kling", runtime.GOOS, runtime.GOARCH), Destino: bin}}}
	res, err := upgrade.Actualizar(context.Background(), o)
	if err != nil || res.Hacia != "1.1.0" {
		t.Fatalf("%+v %v", res, err)
	}

	// La extensión instalada, en 1.0.0.
	dir := filepath.Join(base, "plugins")
	os.Mkdir(dir, 0o755)
	vieja, ayudaVieja := extFalsa("1.0.0"), []byte("#!/bin/sh\necho helper 1.0.0\n")
	ext := filepath.Join(dir, "kling-demo")
	os.WriteFile(ext, vieja, 0o755)
	os.WriteFile(filepath.Join(dir, "kling-demo-helper"), ayudaVieja, 0o755)
	scViejo := `{"name":"demo","version":"1.0.0","companions":["kling-demo-helper"]}`
	os.WriteFile(plugin.SidecarPath(ext), []byte(scViejo), 0o644)

	var hechas []string
	out, _ := salida(t, func() error {
		hechas = actualizarExtensionesEn(dir, o.Dir, res.Hacia, "", plugin.InstallOptions{Client: srv.Client(), ReleaseURL: srv.URL})
		return nil
	})
	if strings.Join(hechas, ",") != "demo" {
		t.Fatalf("upgraded %v\n%s", hechas, out)
	}
	if b, _ := os.ReadFile(ext); string(b) != string(nueva) {
		t.Error("the extension was not replaced")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "kling-demo-helper")); string(b) != string(ayudaNueva) {
		t.Error("its companion was not replaced")
	}
	if sc, err := plugin.ReadSidecar(ext); err != nil || sc.Version != "1.1.0" {
		t.Errorf("sidecar %+v %v", sc, err)
	}

	if _, err := upgrade.VolverAtras(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{ext: string(vieja), filepath.Join(dir, "kling-demo-helper"): string(ayudaVieja), plugin.SidecarPath(ext): scViejo} {
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Errorf("%s not rolled back: %s", filepath.Base(p), b)
		}
	}
}

// Sin daemon que conteste, -rollback saca la raíz y el socket de cómo lo
// arranca su unidad, igual que cmdDaemon: -root/-socket, si no el entorno
// (el fichero manda sobre Environment=), si no los de siempre.
func TestRaizDeLaUnidad(t *testing.T) {
	env := filepath.Join(t.TempDir(), "kling")
	os.WriteFile(env, []byte("KLING_TOKEN=secreto\nexport KLING_SOCKET=\"/run/f/kling.sock\"\n"), 0o600)
	out := []byte("ExecStart={ path=/srv/kt/bin/kling ; argv[]=/srv/kt/bin/kling daemon -root /srv/kt/root -socket=/run/kt/kling.sock ; ignore_errors=no ; start_time=[n/a] }\n" +
		"Environment=KLING_LIB_DIR=/srv/kt/lib KLING_ROOT=/otra\nEnvironmentFiles=" + env + " (ignore_errors=yes)\n")
	argv, entorno, ficheros := leerUnidadSystemd(out)
	if strings.Join(argv, " ") != "/srv/kt/bin/kling daemon -root /srv/kt/root -socket=/run/kt/kling.sock" || entorno["KLING_ROOT"] != "/otra" || len(ficheros) != 1 || ficheros[0] != env {
		t.Fatalf("%q %v %v", argv, entorno, ficheros)
	}
	if r, s := raizYSocket(argv, entorno, "/var/lib/kindling", "/run/kling.sock"); r != "/srv/kt/root" || s != "/run/kt/kling.sock" {
		t.Errorf("root %s socket %s", r, s)
	}
	f := claveDeEntorno(env)
	if f["KLING_SOCKET"] != "/run/f/kling.sock" || f["KLING_TOKEN"] != "" {
		t.Errorf("from the env file: %v", f)
	}
	if r, s := raizYSocket([]string{"/usr/local/bin/kling", "daemon"}, map[string]string{"KLING_ROOT": "/otra"}, "/var/lib/kindling", "/run/kling.sock"); r != "/otra" || s != "/run/kling.sock" {
		t.Errorf("root %s socket %s", r, s)
	}
	argv, entorno, err := leerPlist([]byte(`{"ProgramArguments":["/Users/j/.local/bin/kling","daemon","--root","/Users/j/kr"],"EnvironmentVariables":{"KLING_SOCKET":"/tmp/k.sock"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r, s := raizYSocket(argv, entorno, "/def", "/def.sock"); r != "/Users/j/kr" || s != "/tmp/k.sock" {
		t.Errorf("plist: root %s socket %s", r, s)
	}
	if rutaSocket("unix:///run/kt/kling.sock") != rutaSocket("/run/kt//kling.sock") {
		t.Error("the same socket compares different")
	}
}

// Antes de volver atrás en el Mac: un snapshot que el kling-vz de la copia no
// lee se dice; lo que lee pasa. Un kling-vz sin -snapshot-formats (v0.17) lee
// solo el 1.
func TestVolcadoLegibleParaElKlingVZViejo(t *testing.T) {
	dir := t.TempDir()
	snap := func(v int) string {
		p := filepath.Join(t.TempDir(), "snap.file")
		os.WriteFile(p, []byte(fmt.Sprintf(`{"kling_vz": %d, "machine_identifier": "x"}`, v)), 0o644)
		return p
	}
	viejo := filepath.Join(dir, "kling-vz-v017")
	os.WriteFile(viejo, []byte("#!/bin/sh\necho 'flag provided but not defined' >&2; exit 2\n"), 0o755)
	nuevo := filepath.Join(dir, "kling-vz-v018")
	os.WriteFile(nuevo, []byte("#!/bin/sh\n[ \"$1\" = -snapshot-formats ] && echo '1 2'\n"), 0o755)
	if m := formatoMaxVZ(viejo); m != 1 {
		t.Fatalf("v0.17 reads up to %d, want 1", m)
	}
	if m := formatoMaxVZ(nuevo); m != 2 {
		t.Fatalf("v0.18 reads up to %d, want 2", m)
	}
	if m := formatoMaxVZ(filepath.Join(dir, "no-existe")); m != 1 {
		t.Fatalf("a missing binary reads up to %d, want 1", m)
	}
	if err := volcadoLegible(snap(1), 1); err != nil {
		t.Fatal(err)
	}
	if err := volcadoLegible(snap(2), 1); err == nil || !strings.Contains(err.Error(), "kling_vz 2") {
		t.Fatalf("kling_vz 2 for a kling-vz that reads 1: %v", err)
	}
	if err := volcadoLegible(snap(2), 2); err != nil {
		t.Fatal(err)
	}
	if err := volcadoLegible(filepath.Join(dir, "no-existe"), 2); err == nil {
		t.Fatal("a missing snapshot must not pass")
	}
}

// upgrade solo cambia el kling-guest de KLING_LIB_DIR: avisa de los agentes
// que el daemon toma de otro sitio (con su clave, que es una ruta), y de
// ninguno más.
func TestAgentesFueraDeLib(t *testing.T) {
	lib := "/usr/local/lib/kindling"
	env := []byte("PATH=/usr/bin\x00KLING_GUEST_AGENT=/usr/local/lib/kindling/./kling-guest\x00" +
		"KLING_GUEST_AGENT_arm64=/opt/arm/kling-guest\x00KLING_TOKEN=secreto\x00KLING_GUEST_AGENT_amd64=\x00")
	got := agentesFueraDeLib(env, lib)
	if len(got) != 1 || got[0] != "KLING_GUEST_AGENT_arm64=/opt/arm/kling-guest" {
		t.Fatalf("%q", got)
	}
	if got := agentesFueraDeLib([]byte("KLING_GUEST_AGENT=/srv/kg\x00"), lib); len(got) != 1 || got[0] != "KLING_GUEST_AGENT=/srv/kg" {
		t.Fatalf("%q", got)
	}
	if got := agentesFueraDeLib(nil, lib); got != nil {
		t.Fatalf("%q", got)
	}
}

// El agente del invitado de Linux se busca en /usr/local/lib/kindling en
// Linux y en lib/ de la raíz de datos en macOS (lo pone make install, sin
// sudo); la pista del error dice lo que vale en cada uno.
func TestLibPorDefecto(t *testing.T) {
	got := libPorDefecto("/datos")
	if runtime.GOOS == "darwin" {
		if got != "/datos/lib" || !strings.Contains(pistaAgente(), "make install") {
			t.Fatalf("macOS: lib %q, pista %q", got, pistaAgente())
		}
		return
	}
	if got != "/usr/local/lib/kindling" || !strings.Contains(pistaAgente(), "make deploy") {
		t.Fatalf("lib %q, pista %q", got, pistaAgente())
	}
}
