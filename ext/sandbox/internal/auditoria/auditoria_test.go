package auditoria

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/telemetria"
)

const (
	tokAna   = "tok-ana-secreto-1234567890"
	tokBob   = "tok-bob-secreto-1234567890"
	tokAdmin = "tok-admin-secreto-123456"
)

func identificar(a string) (telemetria.Identidad, bool) {
	switch strings.TrimPrefix(a, "Bearer ") {
	case tokAna:
		return telemetria.Identidad{Nombre: "ana"}, true
	case tokBob:
		return telemetria.Identidad{Nombre: "bob"}, true
	case tokAdmin:
		return telemetria.Identidad{Nombre: "root", Admin: true}, true
	}
	return telemetria.Identidad{}, false
}

// montar levanta el envoltorio sobre un handler que contesta lo que se le dice.
func montar(t *testing.T, o Opciones, estado int, cuerpo string) (*httptest.Server, *Registro) {
	t.Helper()
	if o.Dir == "" {
		o.Dir = filepath.Join(t.TempDir(), "audit")
	}
	reg, err := Abrir(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Cerrar)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // como un handler real, lee lo que le mandan
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(estado)
		io.WriteString(w, cuerpo)
	})
	h := telemetria.Medio(inner, telemetria.Config{
		Identificar: identificar, Observadores: []func(*telemetria.Evento){Observador(reg)}, Propios: Servir(reg),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, reg
}

func pedir(t *testing.T, srv *httptest.Server, tok, metodo, ruta, cuerpo string) *http.Response {
	t.Helper()
	rq, _ := http.NewRequest(metodo, srv.URL+ruta, strings.NewReader(cuerpo))
	if tok != "" {
		rq.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func todas(t *testing.T, reg *Registro) []Linea {
	t.Helper()
	ls, _, err := reg.Leer(time.Time{}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

func TestUnaLineaPorCadaTipoDeAccion(t *testing.T) {
	srv, reg := montar(t, Opciones{}, 200, `{"id":"lab/m1"}`)
	casos := []struct {
		m, ruta, cuerpo, accion string
		det                     map[string]any
	}{
		{"POST", "/v1/sandboxes", `{"template":"py","ttl_seconds":60,"labels":{"k":"v"}}`, "sandbox.create", map[string]any{"template": "py"}},
		{"DELETE", "/v1/sandboxes/lab/m1", "", "sandbox.delete", nil},
		{"POST", "/v1/sandboxes/lab/m1/renew", `{"ttl_seconds":30}`, "sandbox.renew", nil},
		{"POST", "/v1/sandboxes/lab/m1/exec", `{"cmd":["ls","-la","/tmp"],"env":["A=1"],"dir":"/work"}`, "exec", map[string]any{"dir": "/work"}},
		{"POST", "/v1/sandboxes/lab/m1/shell", "", "shell.open", nil},
		{"POST", "/v1/sandboxes/lab/m1/endpoints", `{"port":8080,"public":true}`, "endpoint.create", map[string]any{"port": float64(8080), "public": true}},
		{"PUT", "/v1/sandboxes/lab/m1/files?path=/work/a.txt", "hola mundo", "file.write", map[string]any{"path": "/work/a.txt", "bytes": float64(10)}},
		{"DELETE", "/v1/sandboxes/lab/m1/files?path=/work/a.txt", "", "file.delete", map[string]any{"path": "/work/a.txt"}},
		{"POST", "/v1/sandboxes/lab/m1/processes", `{"cmd":["sleep","99"]}`, "process.start", nil},
		{"POST", "/v1/sandboxes/lab/m1/processes/42/signal", `{"signal":"TERM"}`, "process.signal", map[string]any{"pid": "42", "signal": "TERM"}},
		{"DELETE", "/v1/sandboxes/lab/m1/processes/42", "", "process.kill", map[string]any{"pid": "42"}},
		{"POST", "/v1/sandboxes/lab/m1/sessions", `{"id":"s1","dir":"/work"}`, "session.create", map[string]any{"session": "s1"}},
		{"DELETE", "/v1/sandboxes/lab/m1/sessions/s1", "", "session.delete", map[string]any{"session": "s1"}},
		{"POST", "/v1/tenants/bob/keys", `{"name":"ci"}`, "key.create", map[string]any{"target_tenant": "bob"}},
		{"POST", "/v1/tenants/bob/keys/k1/rotate", ``, "key.rotate", map[string]any{"key_id": "k1"}},
		{"DELETE", "/v1/tenants/bob/keys/k1", "", "key.revoke", map[string]any{"key_id": "k1"}},
		{"DELETE", "/v1/tenants/bob", "", "tenant.delete", nil},
	}
	for _, c := range casos {
		pedir(t, srv, tokAna, c.m, c.ruta, c.cuerpo).Body.Close()
	}
	// Lecturas y rutas sin efecto: sin línea.
	for _, r := range []string{"/v1/sandboxes", "/v1/sandboxes/lab/m1", "/v1/sandboxes/lab/m1/files?path=/x", "/v1/metrics", "/v1/health"} {
		pedir(t, srv, tokAna, "GET", r, "").Body.Close()
	}
	// Un token que no abre nada: no hay inquilino a quien atribuirlo.
	pedir(t, srv, "tok-falso-0000000000000", "POST", "/v1/sandboxes", `{}`).Body.Close()

	ls := todas(t, reg)
	if len(ls) != len(casos) {
		t.Fatalf("%d lines, want %d: %+v", len(ls), len(casos), ls)
	}
	for i, c := range casos {
		l := ls[i]
		if l.Accion != c.accion || l.Tenant != "ana" || l.Resultado != "ok" || l.Estado != 200 || len(l.Traza) != 32 || l.Ts.IsZero() {
			t.Errorf("%s: %+v", c.accion, l)
		}
		for k, v := range c.det {
			if l.Detalle[k] != v {
				t.Errorf("%s: detail[%s] = %v, want %v", c.accion, k, l.Detalle[k], v)
			}
		}
		if strings.Contains(c.ruta, "/m1") && l.Sandbox != "lab/m1" {
			t.Errorf("%s: sandbox = %q", c.accion, l.Sandbox)
		}
	}
	if a, _ := ls[3].Detalle["argv"].([]any); len(a) != 3 || a[0] != "ls" {
		t.Errorf("exec argv = %v", ls[3].Detalle["argv"])
	}
	if ls[0].Sandbox != "lab/m1" {
		t.Errorf("create: sandbox id from the response = %q", ls[0].Sandbox)
	}
}

func TestResultadoDeniedYError(t *testing.T) {
	for estado, want := range map[int]string{403: "denied", 429: "denied", 404: "error", 502: "error", 201: "ok"} {
		srv, reg := montar(t, Opciones{}, estado, `{}`)
		pedir(t, srv, tokAna, "POST", "/v1/sandboxes/lab/m1/exec", `{"cmd":["x"]}`).Body.Close()
		if ls := todas(t, reg); len(ls) != 1 || ls[0].Resultado != want || ls[0].Estado != estado {
			t.Errorf("%d: %+v", estado, ls)
		}
	}
}

func TestSinSecretosEnElRegistro(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a")
	srv, reg := montar(t, Opciones{Dir: dir}, 200, `{"id":"lab/m1","token":"RESP-SECRETO"}`)
	pedir(t, srv, tokAna, "POST", "/v1/sandboxes/lab/m1/exec",
		`{"cmd":["curl","-H","Authorization: Bearer ABCSECRETO","--password=hunter2","password: otro","x"],"env":["API_KEY=VALORENV","PATH=/bin"],"stdin":"U0VDUkVUT1NURElO"}`).Body.Close()
	pedir(t, srv, tokAna, "POST", "/v1/sandboxes/lab/m1/processes", `{"cmd":["run"],"env":["DB_PASS=CLAVEDB"]}`).Body.Close()
	pedir(t, srv, tokAna, "PUT", "/v1/sandboxes/lab/m1/files?path=/x", "CONTENIDOSECRETO").Body.Close()
	pedir(t, srv, tokAdmin, "POST", "/v1/tenants/bob/keys", `{"name":"ci","token":"CLAVENUEVA"}`).Body.Close()
	reg.Vaciar()
	b, _ := os.ReadFile(filepath.Join(dir, Fichero))
	s := string(b)
	for _, prohibido := range []string{tokAna, tokAdmin, "ABCSECRETO", "hunter2", "otro", "VALORENV", "CLAVEDB", "SECRETOSTDIN", "U0VDUkVU", "CONTENIDOSECRETO", "CLAVENUEVA", "RESP-SECRETO"} {
		if strings.Contains(s, prohibido) {
			t.Errorf("the audit log contains %q:\n%s", prohibido, s)
		}
	}
	for _, debe := range []string{`"API_KEY"`, `"PATH"`, `"DB_PASS"`, `"curl"`} {
		if !strings.Contains(s, debe) {
			t.Errorf("missing %s:\n%s", debe, s)
		}
	}
}

func TestArgvTruncado(t *testing.T) {
	cmd := make([]string, 50)
	for i := range cmd {
		cmd[i] = strings.Repeat("é", 200)
	}
	a := Argv(cmd)
	if len(a) != maxArgs+1 || !strings.Contains(a[maxArgs], "18 more") {
		t.Errorf("argv len %d, last %q", len(a), a[len(a)-1])
	}
	if len(a[0]) > maxArgBytes+len("…") || !strings.HasSuffix(a[0], "…") {
		t.Errorf("arg not truncated: %d bytes", len(a[0]))
	}
}

func TestFicheroConPermisos0600YRotacion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a")
	// Un fichero previo con permisos laxos se endurece.
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, Fichero), nil, 0o644)
	srv, reg := montar(t, Opciones{Dir: dir, MaxBytes: 2000, Generaciones: 2}, 200, `{}`)
	for i := 0; i < 60; i++ {
		pedir(t, srv, tokAna, "DELETE", fmt.Sprintf("/v1/sandboxes/lab/m%d", i), "").Body.Close()
	}
	reg.Vaciar()
	for _, n := range []string{Fichero, Fichero + ".1", Fichero + ".2"} {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", n, st.Mode().Perm())
		}
		if st.Size() > 2000 {
			t.Errorf("%s is %d bytes, over the limit", n, st.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, Fichero+".3")); err == nil {
		t.Error("a third generation exists")
	}
	// Lo que cayó de la última generación se perdió (rotación), pero lo más reciente sigue.
	ls := todas(t, reg)
	if len(ls) == 0 || ls[len(ls)-1].Sandbox != "lab/m59" {
		t.Errorf("the latest line is missing: %d lines", len(ls))
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o755 { // existía antes: no se toca
		t.Logf("dir mode %v", st.Mode().Perm())
	}
}

func TestDirectorioNuevoEs0700YEnlaceNoSeSigue(t *testing.T) {
	base := t.TempDir()
	reg, err := Abrir(Opciones{Dir: filepath.Join(base, "n")})
	if err != nil {
		t.Fatal(err)
	}
	reg.Cerrar()
	if st, _ := os.Stat(filepath.Join(base, "n")); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	d := filepath.Join(base, "e")
	os.MkdirAll(d, 0o700)
	os.WriteFile(filepath.Join(base, "otro"), nil, 0o600)
	os.Symlink(filepath.Join(base, "otro"), filepath.Join(d, Fichero))
	if _, err := Abrir(Opciones{Dir: d}); err == nil {
		t.Error("followed a symlink")
	}
}

func TestEndpointAuditFiltroPorInquilino(t *testing.T) {
	srv, reg := montar(t, Opciones{}, 200, `{}`)
	pedir(t, srv, tokAna, "DELETE", "/v1/sandboxes/lab/a1", "").Body.Close()
	pedir(t, srv, tokBob, "DELETE", "/v1/sandboxes/lab/b1", "").Body.Close()
	pedir(t, srv, tokBob, "DELETE", "/v1/sandboxes/lab/b2", "").Body.Close()
	reg.Vaciar()

	leer := func(tok, query string) (int, []Linea, bool) {
		resp := pedir(t, srv, tok, "GET", "/v1/audit"+query, "")
		defer resp.Body.Close()
		var out struct {
			Events    []Linea
			Truncated bool
		}
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Events, out.Truncated
	}
	if c, _, _ := leer("", ""); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c, _, _ := leer("tok-falso-0000000000000", ""); c != 401 {
		t.Errorf("bad token: %d", c)
	}
	c, ls, _ := leer(tokAna, "")
	if c != 200 || len(ls) != 1 || ls[0].Tenant != "ana" {
		t.Errorf("ana: %d %+v", c, ls)
	}
	// Pedir lo de otro no da nada de otro.
	if _, ls, _ := leer(tokAna, "?tenant=bob"); len(ls) != 1 || ls[0].Tenant != "ana" {
		t.Errorf("ana with tenant=bob: %+v", ls)
	}
	if _, ls, _ := leer(tokBob, ""); len(ls) != 2 {
		t.Errorf("bob: %+v", ls)
	}
	if _, ls, _ := leer(tokAdmin, ""); len(ls) != 3 {
		t.Errorf("admin: %+v", ls)
	}
	if _, ls, _ := leer(tokAdmin, "?tenant=bob"); len(ls) != 2 {
		t.Errorf("admin tenant=bob: %+v", ls)
	}
	if _, ls, tr := leer(tokAdmin, "?limit=2"); len(ls) != 2 || !tr {
		t.Errorf("limit: %d truncated=%v", len(ls), tr)
	}
	// since: en el futuro no hay nada; duración y RFC 3339 valen; basura es 400.
	if _, ls, _ := leer(tokAdmin, "?since="+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); len(ls) != 0 {
		t.Errorf("future since: %+v", ls)
	}
	if _, ls, _ := leer(tokAdmin, "?since=1h"); len(ls) != 3 {
		t.Errorf("since=1h: %+v", ls)
	}
	if c, _, _ := leer(tokAdmin, "?since=ayer"); c != 400 {
		t.Errorf("bad since: %d", c)
	}
	if resp := pedir(t, srv, tokAdmin, "POST", "/v1/audit", ""); resp.StatusCode != 405 {
		t.Errorf("POST: %d", resp.StatusCode)
	}
}

func TestColaLlenaDescartaYLoCuenta(t *testing.T) {
	reg, err := Abrir(Opciones{Dir: t.TempDir(), Cola: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Cerrar()
	// Bloquea el escritor con el cerrojo de rotación para llenar la cola.
	reg.mu.Lock()
	for i := 0; i < 50; i++ {
		reg.Escribir(Linea{Ts: time.Now(), Tenant: "ana", Accion: "exec", Resultado: "ok"})
	}
	reg.mu.Unlock()
	if reg.Descartados() == 0 {
		t.Fatal("nothing dropped with a full queue")
	}
	reg.Escribir(Linea{Ts: time.Now(), Tenant: "ana", Accion: "exec", Resultado: "ok"})
	reg.Vaciar()
	var suma int64
	for _, l := range todas(t, reg) {
		suma += l.Descartados
	}
	if suma == 0 || suma > reg.Descartados() {
		t.Errorf("dropped recorded in lines = %d, total %d", suma, reg.Descartados())
	}
}
