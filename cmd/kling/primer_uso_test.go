package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/ext4"
	"github.com/juan52878911/kindling/internal/transport"
	"github.com/juan52878911/kindling/internal/upgrade"
)

// Primer uso: lo que pasa entre `curl … | sh` y el primer `kling try`.

// socketDaemon es fakeDaemon pero devolviendo la ruta del socket, para pasarla
// a un comando con -H.
func socketDaemon(t *testing.T, mux http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kt")
	if err != nil {
		t.Skipf("no /tmp for a unix socket: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot listen on a unix socket: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

// sinJSON restablece el estado global que parseFlags deja tras un -json.
func sinJSON(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { salidaJSON.Store(false) })
}

func TestTrySinImagenUsaMin(t *testing.T) {
	d := &tryDaemon{}
	m := d.mux()
	m.Handle("GET /images", imagesMux("min"))
	sock := socketDaemon(t, m)
	code, err := cmdTry([]string{"-H", sock, "--", "true"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(d.created) != 1 || d.created[0].Image != "min" {
		t.Fatalf("created = %+v, want one sandbox from min", d.created)
	}
}

func TestTryPrefiereToolchain(t *testing.T) {
	d := &tryDaemon{}
	m := d.mux()
	m.Handle("GET /images", imagesMux("min", "toolchain"))
	sock := socketDaemon(t, m)
	if code, err := cmdTry([]string{"-H", sock, "--", "true"}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(d.created) != 1 || d.created[0].Image != tryDefaultImage {
		t.Fatalf("created = %+v", d.created)
	}
}

func TestTrySinImagenLasBajaYSigue(t *testing.T) {
	d := &tryDaemon{}
	m := d.mux()
	var mu sync.Mutex
	bajadas := 0
	m.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hay := bajadas > 0
		mu.Unlock()
		if hay {
			imagesMux("min").ServeHTTP(w, r)
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	sock := socketDaemon(t, m)
	old := bajarImagenesTry
	bajarImagenesTry = func(ctx context.Context, endpoint string) error {
		mu.Lock()
		bajadas++
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { bajarImagenesTry = old })
	code, err := cmdTry([]string{"-H", sock, "--", "true"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if bajadas != 1 || len(d.created) != 1 || d.created[0].Image != "min" {
		t.Fatalf("bajadas=%d created=%+v", bajadas, d.created)
	}
}

func TestTrySinImagenYSinRedDaUnaOrden(t *testing.T) {
	d := &tryDaemon{}
	m := d.mux()
	m.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("[]")) })
	sock := socketDaemon(t, m)
	old := bajarImagenesTry
	bajarImagenesTry = func(context.Context, string) error { return errors.New("no network") }
	t.Cleanup(func() { bajarImagenesTry = old })
	_, err := cmdTry([]string{"-H", sock, "--", "true"})
	if err == nil {
		t.Fatal("want an error")
	}
	if h := hintFor(err); !strings.Contains(h, "kling up") || strings.Count(h, "\n") != 0 {
		t.Fatalf("hint = %q, want a single command with kling up", h)
	}
	if len(d.created) != 0 {
		t.Fatal("it must not create a sandbox without an image")
	}
}

func TestTryArrancaElDaemonSiNoCorre(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "no.sock")
	if len(sock) > 100 {
		sock = "/tmp/kling-no-" + filepath.Base(filepath.Dir(sock)) + ".sock"
	}
	old := arrancarDaemonTry
	llamadas := 0
	arrancarDaemonTry = func(ctx context.Context, endpoint string) error {
		llamadas++
		return errors.New("could not start the daemon")
	}
	t.Cleanup(func() { arrancarDaemonTry = old })
	code, err := cmdTry([]string{"-H", sock, "--", "true"})
	if llamadas != 1 || err == nil || code == 0 {
		t.Fatalf("llamadas=%d code=%d err=%v", llamadas, code, err)
	}
}

func TestTryJSONErrorEsJSON(t *testing.T) {
	sinJSON(t)
	sock := filepath.Join("/tmp", "kling-ausente-"+filepath.Base(t.TempDir())+".sock")
	old := arrancarDaemonTry
	arrancarDaemonTry = func(context.Context, string) error { return errors.New("down") }
	t.Cleanup(func() { arrancarDaemonTry = old })
	code, err := cmdTry([]string{"-H", sock, "-json", "--", "true"})
	if err == nil || code == 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !salidaJSON.Load() {
		t.Fatal("-json must make the error come out as JSON (parseFlags)")
	}
}

func TestTryJSONFlagMalo(t *testing.T) {
	sinJSON(t)
	code, err := cmdTry([]string{"-json", "-nope", "--", "true"})
	if err == nil || code != salidaUso {
		t.Fatalf("code=%d err=%v, want usage error (2) and not os.Exit", code, err)
	}
	if !salidaJSON.Load() {
		t.Fatal("a bad flag with -json must still answer in JSON")
	}
}

// Los códigos 3, 4, 5 y 130 del contrato llegan a la salida de try.
func TestTryConservaCodigosDeContrato(t *testing.T) {
	casos := []struct {
		nombre string
		status int
		cuerpo string
		code   int
	}{
		{"dorado obsoleto", http.StatusConflict, `{"message":"stale golden snapshot: rebuild it"}`, salidaDoradoObsoleto},
	}
	for _, c := range casos {
		for _, conJSON := range []bool{false, true} {
			t.Run(c.nombre, func(t *testing.T) {
				sinJSON(t)
				m := http.NewServeMux()
				m.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(c.status)
					_, _ = w.Write([]byte(c.cuerpo))
				})
				sock := socketDaemon(t, m)
				args := []string{"-H", sock, "-image", "min"}
				if conJSON {
					args = append(args, "-json")
				}
				code, err := cmdTry(append(args, "--", "true"))
				if err == nil || code != c.code {
					t.Fatalf("json=%v code=%d err=%v, want %d", conJSON, code, err, c.code)
				}
			})
		}
	}
}

func TestCodigoDeTryInterrumpidoYViejo(t *testing.T) {
	if got := codigoTry(errors.New("x"), 1); got != 1 {
		t.Fatalf("plain error: %d", got)
	}
	interrumpido.Store(true)
	t.Cleanup(func() { interrumpido.Store(false) })
	if got := codigoTry(errors.New("interrupted"), 1); got != salidaInterrumpido {
		t.Fatalf("interrupted: %d", got)
	}
}

// ── kling up en Mac ───────────────────────────────────────────────────────────

func TestStartMacDaemonSinAgenteFallaSiNoHayQuienConfirme(t *testing.T) {
	a := agenteDaemon{label: "dev.kindling.test", plist: filepath.Join(t.TempDir(), "x.plist"), log: filepath.Join(t.TempDir(), "x.log")}
	old := confirmarAgente
	confirmarAgente = func(agenteDaemon) bool { return false }
	t.Cleanup(func() { confirmarAgente = old })
	err := startMacDaemon(io.Discard, a, "/tmp/kling-no-such.sock", t.TempDir())
	if err == nil {
		t.Fatal("kling up without a daemon and without an agent must fail, not say 'Next step'")
	}
	if h := hintFor(err); !strings.Contains(err.Error()+h, "kling up -install") {
		t.Fatalf("it should name the fix: %v / %q", err, h)
	}
}

func TestStartMacDaemonInstalaElAgenteSiConfirman(t *testing.T) {
	dir := t.TempDir()
	a := agenteDaemon{label: "dev.kindling.test", plist: filepath.Join(dir, "x.plist"), log: filepath.Join(dir, "x.log")}
	oldC, oldL, oldE := confirmarAgente, lanzarctl, esperaDaemonMac
	esperaDaemonMac = 200 * time.Millisecond
	confirmarAgente = func(agenteDaemon) bool { return true }
	var cmds []string
	lanzarctl = func(args ...string) ([]byte, error) { cmds = append(cmds, strings.Join(args, " ")); return nil, nil }
	t.Cleanup(func() { confirmarAgente, lanzarctl, esperaDaemonMac = oldC, oldL, oldE })
	// launchctl "funciona" pero el socket nunca contesta: error, no éxito.
	err := startMacDaemon(io.Discard, a, "/tmp/kling-no-such.sock", dir)
	if _, serr := os.Stat(a.plist); serr != nil {
		t.Fatalf("the agent was not written: %v", serr)
	}
	if err == nil {
		t.Fatal("a daemon that never answers must be an error")
	}
	if len(cmds) == 0 || !strings.HasPrefix(cmds[0], "launchctl bootstrap") {
		t.Fatalf("launchctl calls: %v", cmds)
	}
}

func TestPlistAdaptativoYConFreno(t *testing.T) {
	p := plistDaemon(agenteDaemon{label: "dev.kindling.test", log: "/tmp/l"}, "/usr/local/bin/kling", nil)
	for _, want := range []string{"<key>ProcessType</key>\n  <string>Adaptive</string>", "<key>ThrottleInterval</key>\n  <integer>30</integer>"} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q:\n%s", want, p)
		}
	}
}

func TestRotarLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kindling.log")
	if err := os.WriteFile(p, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rotarLog(p, 4096); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".1"); err == nil {
		t.Fatal("below the limit it must not rotate")
	}
	if err := rotarLog(p, 1024); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p + ".1"); err != nil || fi.Size() != 2048 {
		t.Fatalf("rotated file: %v %v", fi, err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the live log should be gone (launchd recreates it)")
	}
	if err := rotarLog(filepath.Join(t.TempDir(), "none"), 1); err != nil {
		t.Fatalf("a missing log is not an error: %v", err)
	}
}

func TestDesinstalarAgente(t *testing.T) {
	dir := t.TempDir()
	a := agenteDaemon{label: "dev.kindling.test", plist: filepath.Join(dir, "x.plist"), log: filepath.Join(dir, "x.log")}
	if err := os.WriteFile(a.plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := lanzarctl
	var cmds []string
	lanzarctl = func(args ...string) ([]byte, error) { cmds = append(cmds, strings.Join(args, " ")); return nil, nil }
	t.Cleanup(func() { lanzarctl = old })
	if err := desinstalarAgente(a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.plist); err == nil {
		t.Fatal("plist still there")
	}
	if len(cmds) != 1 || !strings.HasPrefix(cmds[0], "launchctl bootout ") {
		t.Fatalf("launchctl calls: %v", cmds)
	}
	// Sin plist: nada que hacer y sin error.
	if err := desinstalarAgente(a); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
}

// ── plantilla de overlay sin e2fsprogs ────────────────────────────────────────

func TestPlantillaOverlayConExt4EnGo(t *testing.T) {
	root := t.TempDir()
	if err := escribirPlantillaOverlay(root, 8); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "images", "overlay-template.ext4")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 8<<20 {
		t.Fatalf("size = %d, want %d", fi.Size(), 8<<20)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n, err := ext4.Read(f)
	if err != nil {
		t.Fatalf("not a readable ext4: %v", err)
	}
	if n.Lookup("/lost+found") == nil {
		t.Fatal("no lost+found")
	}
	// Una segunda vez no la pisa (existir implica estar completa).
	if err := os.Chtimes(p, fi.ModTime().Add(-1e9*3600), fi.ModTime().Add(-1e9*3600)); err != nil {
		t.Fatal(err)
	}
	if err := escribirPlantillaOverlay(root, 8); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(p)
	if fi2.ModTime().After(fi.ModTime().Add(-1e9 * 1800)) {
		t.Fatal("an existing template must be left alone")
	}
}

func TestMkfsYaNoEsFatalEnMac(t *testing.T) {
	for _, c := range checksMac(probeMac{arch: "arm64", macOS: "15.5", vmm: "/x/kling-vz", vmmVersion: "1", entitlement: true}) {
		if c.label == "mkfs.ext4" && c.fatal {
			t.Fatal("mkfs.ext4 only matters for -disk other than 512: it must be a warning")
		}
	}
}

// ── up -verify-sums (install.sh sin minisign) ─────────────────────────────────

func TestVerificarSumas(t *testing.T) {
	k, err := upgrade.GenerarClave(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sums := filepath.Join(dir, "SHA256SUMS")
	sigf := filepath.Join(dir, "SHA256SUMS.minisig")
	datos := []byte("abc  kling-linux-amd64\n")
	if err := os.WriteFile(sums, datos, 0o644); err != nil {
		t.Fatal(err)
	}
	sig, err := upgrade.Firmar(k, datos, "timestamp:1\tfile:SHA256SUMS\ttag:v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sigf, sig, 0o644); err != nil {
		t.Fatal(err)
	}
	clave := k.Publica().String()
	if err := verificarSumas(sums, sigf, "v1.0.0", clave); err != nil {
		t.Fatalf("a valid signature: %v", err)
	}
	if err := verificarSumas(sums, sigf, "v0.9.0", clave); err == nil {
		t.Fatal("a signature for another release must be refused")
	}
	otra, _ := upgrade.GenerarClave(rand.Reader)
	if err := verificarSumas(sums, sigf, "v1.0.0", otra.Publica().String()); err == nil {
		t.Fatal("a signature from an unknown key must be refused")
	}
	if err := os.WriteFile(sums, append(datos, 'x'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verificarSumas(sums, sigf, "v1.0.0", clave); err == nil {
		t.Fatal("altered SHA256SUMS must be refused")
	}
}

// La unidad que escribe `kling up -install` es la de packaging/kling.service
// (la de make deploy) sin comentarios.
func TestUnidadSystemdIgualQuePackaging(t *testing.T) {
	b, err := os.ReadFile("../../packaging/kling.service")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "Description=") {
			want = append(want, l)
		}
	}
	var got []string
	for _, l := range strings.Split(textoUnidad("/usr/local/bin/kling"), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "Description=") {
			got = append(got, l)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("units differ:\n--- generated\n%s\n--- packaging\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// El kernel, min y el agente de invitado salen de la misma release.
func TestImagenesMacIncluyeElAgenteDeInvitado(t *testing.T) {
	var vistos []string
	for _, im := range imagenesMac("arm64") {
		vistos = append(vistos, im.asset+" -> "+filepath.Join(im.sub, im.destino))
	}
	for _, want := range []string{"vmlinux-arm64.zst -> vmlinux", "min-arm64.ext4.zst -> min.ext4", "kling-guest-linux-arm64 -> lib/kling-guest"} {
		if !strings.Contains(strings.Join(vistos, "\n"), want) {
			t.Fatalf("missing %q in %v", want, vistos)
		}
	}
}

// ── revisión de primer uso (R20-R28) ─────────────────────────────────────────

// R23: `kling try` que arranca el daemon no escribe nada propio en stdout: es
// de lo que se capture con out=$(kling try -- cmd) o -json.
func TestTryArrancandoNoEnsuciaStdout(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KLING_LAUNCH_AGENT_PLIST", filepath.Join(dir, "x.plist"))
	t.Setenv("KLING_LAUNCH_AGENT_LABEL", "dev.kindling.revtest")
	t.Setenv("KLING_ROOT", filepath.Join(dir, "root"))
	t.Setenv("HOME", dir)
	oldC, oldL, oldE := confirmarAgente, lanzarctl, esperaDaemonMac
	esperaDaemonMac = 100 * time.Millisecond
	confirmarAgente = func(agenteDaemon) bool { return true }
	lanzarctl = func(args ...string) ([]byte, error) { return nil, nil }
	t.Cleanup(func() { confirmarAgente, lanzarctl, esperaDaemonMac = oldC, oldL, oldE })
	if runtime.GOOS != "darwin" {
		t.Skip("el agente de launchd solo existe en macOS")
	}
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	_ = arrancarDaemonLocal(context.Background(), transport.DefaultSocketPath())
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	if len(b) != 0 {
		t.Errorf("kling try wrote %d bytes of its own to stdout: %q", len(b), b)
	}
}

// R24: varios `kling up` a la vez sobre la misma raíz no se pisan el temporal.
func TestPlantillaOverlayVariasAlaVez(t *testing.T) {
	for i := 0; i < 5; i++ {
		root := t.TempDir()
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for j := range errs {
			wg.Add(1)
			go func() { defer wg.Done(); errs[j] = escribirPlantillaOverlay(root, 64) }()
		}
		wg.Wait()
		for _, e := range errs {
			if e != nil {
				t.Errorf("round %d: %v", i, e)
			}
		}
		if ents, _ := os.ReadDir(filepath.Join(root, "images")); len(ents) != 1 {
			t.Errorf("round %d: left %d files in images/, want just the template", i, len(ents))
		}
	}
}

// R22: la unidad de systemd la ejecuta root: el binario y cada directorio por
// encima tienen que ser de root y no escribibles por otros.
func TestUnidadSystemdRechazaBinarioEscribible(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "kling")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := comprobarBinarioDeRoot(exe)
	if err == nil {
		t.Fatal("a binary in a world-writable directory must be refused")
	}
	if h := hintFor(err); !strings.Contains(h, "/usr/local/bin/kling") {
		t.Errorf("the hint should say where to copy it: %q", h)
	}
	if runtime.GOOS != "windows" {
		if err := comprobarBinarioDeRoot("/bin/sh"); err != nil {
			t.Errorf("a system binary must pass: %v", err)
		}
	}
}

func TestTextoUnidadCitaLaRuta(t *testing.T) {
	if got := textoUnidad("/opt/with space/kling"); !strings.Contains(got, `ExecStart="/opt/with space/kling" daemon`) {
		t.Errorf("ExecStart must quote the path:\n%s", got)
	}
}

// R25: try solo arranca el agente si el destino es el socket por defecto.
func TestDestinoEsElLocal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	def := transport.DefaultSocketPath()
	for _, ok := range []string{"", def, "unix://" + def} {
		if !destinoEsElLocal(ok) {
			t.Errorf("%q is the local daemon", ok)
		}
	}
	if destinoEsElLocal("/tmp/otra-raiz/kling.sock") || destinoEsElLocal("unix:///tmp/otra-raiz/kling.sock") {
		t.Error("a socket under another root is not the local daemon")
	}
}

// R27: un plist que solo difiere en claves conocidas se migra; otro cambio no.
func TestEscribirPlistMigraClavesConocidas(t *testing.T) {
	dir := t.TempDir()
	a := agenteDaemon{label: "dev.kindling.test", plist: filepath.Join(dir, "x.plist"), log: filepath.Join(dir, "x.log")}
	nuevo := plistDaemon(a, "/usr/local/bin/kling", nil)
	viejo := strings.Replace(nuevo, "<integer>30</integer>", "<integer>10</integer>", 1)
	viejo = strings.Replace(viejo, "<key>ProcessType</key>\n  <string>Adaptive</string>\n", "", 1)
	if viejo == nuevo {
		t.Fatal("the test did not alter the plist")
	}
	if err := os.WriteFile(a.plist, []byte(viejo), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := escribirPlist(a, nuevo); err != nil || !ok {
		t.Fatalf("known keys must migrate: %v %v", ok, err)
	}
	if b, _ := os.ReadFile(a.plist); string(b) != nuevo {
		t.Error("not migrated")
	}
	ajustado := strings.Replace(nuevo, "<true/>", "<false/>", 1)
	if err := os.WriteFile(a.plist, []byte(ajustado), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := escribirPlist(a, nuevo); !errors.Is(err, errPlistDistinto) {
		t.Fatalf("a hand-edited plist must be left alone: %v", err)
	}
}
