package frontal

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// ponerSesiones pone delante del daemon (falso o real) lo único que le falta
// a create(env=): POST /machines/{ref}/sessions.
func ponerSesiones(t *testing.T, f *falso) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ss")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	detras := f.endpoint
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) { r.URL.Scheme, r.URL.Host = "http", "daemon" },
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", detras)
		}},
	}
	m := http.NewServeMux()
	m.HandleFunc("POST /machines/{ref}/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req api.SessionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeJSON(w, 201, &api.Session{ID: req.ID, Dir: req.Dir, Env: req.Env})
	})
	m.Handle("/", proxy)
	srv := httptest.NewUnstartedServer(m)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	f.endpoint = sock
}

// raizSDKs es el directorio sdks/ del repositorio.
func raizSDKs(t *testing.T) string {
	t.Helper()
	_, este, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(este), "..", "..", "..", "..", "sdks")
}

// bancoContrato monta el frontal REAL (con entrada a puertos) delante de un
// daemon que une el falso, pausa, sesiones y un invitado de verdad (exec, ficheros
// y conductores de código en esta máquina). Sin VM.
func bancoContrato(t *testing.T) *httptest.Server {
	t.Helper()
	f := nuevoFalso(t)
	ponerPausa(t, f)
	ponerSesiones(t, f)
	ponerReal(t, f)
	s, err := Nuevo(Opciones{
		Hosts:   hosts.Nuevo(map[string]string{"a": f.endpoint}),
		Tenants: []Tenant{{Nombre: "alice", Token: tokenAlice}, {Nombre: "bob", Token: tokenBob}},
		Log:     testLog(t),
		Ingreso: IngresoOpciones{Dominio: dominioTest, Esquema: "http", Secreto: []byte("secreto-de-prueba-123")},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

// TestContratoSDKs recorre cada SDK (Python y TypeScript) contra el frontal real
// y su daemon falso: lo que la spec promete y los SDK usan, de punta a punta.
// Los scripts (sdks/python/tests/contrato.py, sdks/typescript/test/contrato.mjs)
// fallan con código distinto de 0 y cuentan qué paso rompió.
func TestContratoSDKs(t *testing.T) {
	raiz := raizSDKs(t)
	casos := []struct {
		nombre, herramienta string
		args                []string
	}{
		{"python", "python3", []string{filepath.Join(raiz, "python", "tests", "contrato.py")}},
		{"typescript", "node", []string{filepath.Join(raiz, "typescript", "test", "contrato.mjs")}},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			bin, err := exec.LookPath(c.herramienta)
			if err != nil {
				t.Skipf("no %s in this runner", c.herramienta)
			}
			if _, err := os.Stat(c.args[0]); err != nil {
				t.Fatalf("falta el script del contrato: %v", err)
			}
			srv := bancoContrato(t)
			base := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, append(c.args, srv.URL, tokenAlice, base, dominioTest)...)
			cmd.Env = append(os.Environ(), "KLING_SANDBOX_URL=", "KLING_SANDBOX_TOKEN=")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", c.nombre, err, out)
			}
			if !strings.Contains(string(out), "CONTRATO OK") {
				t.Fatalf("%s no llegó al final:\n%s", c.nombre, out)
			}
			t.Logf("%s", strings.TrimSpace(string(out)))
		})
	}
}
