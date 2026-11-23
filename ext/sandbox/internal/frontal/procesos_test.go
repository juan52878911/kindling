package frontal

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// procsFalsos es el daemon falso de procesos y sesiones: guarda lo que le
// mandan y contesta con formas reales de pkg/api.
type procsFalsos struct {
	mu       sync.Mutex
	procs    []api.ProcessInfo
	sesiones map[string]*api.Session
	ultimaSe api.SessionRequest
	// ultimoProc es el último POST processes recibido.
	ultimoProc api.ProcessStartRequest
	senales  []string
}

func (f *falso) rutasProcesos(m *http.ServeMux) {
	pf := &f.procs
	m.HandleFunc("POST /machines/{ref}/processes", func(w http.ResponseWriter, r *http.Request) {
		var req api.ProcessStartRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		pf.mu.Lock()
		defer pf.mu.Unlock()
		pf.ultimoProc = req
		p := api.ProcessInfo{PID: len(pf.procs) + 1, Cmd: req.Cmd, State: api.ProcessRunning}
		pf.procs = append(pf.procs, p)
		writeJSON(w, 201, p)
	})
	m.HandleFunc("GET /machines/{ref}/processes", func(w http.ResponseWriter, r *http.Request) {
		pf.mu.Lock()
		defer pf.mu.Unlock()
		writeJSON(w, 200, pf.procs)
	})
	m.HandleFunc("GET /machines/{ref}/processes/{pid}", func(w http.ResponseWriter, r *http.Request) {
		pf.mu.Lock()
		defer pf.mu.Unlock()
		for _, p := range pf.procs {
			if r.PathValue("pid") == itoa(p.PID) {
				writeJSON(w, 200, p)
				return
			}
		}
		failDaemon(w, 404, errSin("no such process"))
	})
	m.HandleFunc("POST /machines/{ref}/processes/{pid}/signal", func(w http.ResponseWriter, r *http.Request) {
		var req api.ProcessSignalRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		pf.mu.Lock()
		defer pf.mu.Unlock()
		pf.senales = append(pf.senales, req.Signal)
		for i := range pf.procs {
			if r.PathValue("pid") == itoa(pf.procs[i].PID) {
				c := 143
				pf.procs[i].State, pf.procs[i].ExitCode = api.ProcessExited, &c
				writeJSON(w, 200, pf.procs[i])
				return
			}
		}
		failDaemon(w, 404, errSin("no such process"))
	})
	m.HandleFunc("DELETE /machines/{ref}/processes/{pid}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	m.HandleFunc("GET /machines/{ref}/processes/{pid}/output", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		off := int64(0)
		if r.URL.Query().Get("offset") == "3" {
			off = 3
		}
		_ = enc.Encode(api.ProcessEvent{Stream: "stdout", Data: []byte("hola\n")[off:], Offset: 5})
		if r.URL.Query().Get("follow") == "1" {
			c := 0
			_ = enc.Encode(api.ProcessEvent{Offset: 5, Exit: &c})
		}
	})
	m.HandleFunc("POST /machines/{ref}/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req api.SessionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		pf.mu.Lock()
		defer pf.mu.Unlock()
		if pf.sesiones == nil {
			pf.sesiones = map[string]*api.Session{}
		}
		pf.ultimaSe = req
		id := req.ID
		if id == "" {
			id = "s" + itoa(len(pf.sesiones)+1)
		}
		se := &api.Session{ID: id, Dir: req.Dir, Env: req.Env}
		pf.sesiones[id] = se
		writeJSON(w, 201, se)
	})
	m.HandleFunc("GET /machines/{ref}/sessions", func(w http.ResponseWriter, r *http.Request) {
		pf.mu.Lock()
		defer pf.mu.Unlock()
		out := []api.Session{}
		for _, se := range pf.sesiones {
			out = append(out, *se)
		}
		writeJSON(w, 200, out)
	})
	m.HandleFunc("GET /machines/{ref}/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		pf.mu.Lock()
		defer pf.mu.Unlock()
		if se, ok := pf.sesiones[r.PathValue("id")]; ok {
			writeJSON(w, 200, se)
			return
		}
		failDaemon(w, 404, errSin("no such session"))
	})
	m.HandleFunc("DELETE /machines/{ref}/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		pf.mu.Lock()
		defer pf.mu.Unlock()
		delete(pf.sesiones, r.PathValue("id"))
		w.WriteHeader(204)
	})
}

type errSin string

func (e errSin) Error() string { return string(e) }

func itoa(n int) string { return jsonNum(n) }
func jsonNum(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// bancoProcs monta un daemon falso, un sandbox de alice y el frontal.
func bancoProcs(t *testing.T, tenants ...Tenant) (*httptest.Server, *falso, string) {
	t.Helper()
	f := nuevoFalso(t)
	sb := f.sembrar(&api.Machine{Labels: map[string]string{LabelTenant: "alice"}})
	if tenants == nil {
		tenants = []Tenant{{Nombre: "alice", Token: tokenAlice}, {Nombre: "bob", Token: tokenBob}}
	}
	s, err := Nuevo(Opciones{Hosts: hosts.Nuevo(map[string]string{"a": f.endpoint}), Tenants: tenants, Log: testLog(t)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, f, "/v1/sandboxes/a/" + sb.ID
}

func TestProcesosCicloDeVida(t *testing.T) {
	srv, f, base := bancoProcs(t)

	resp := pide(t, srv, tokenAlice, "POST", base+"/processes", api.ProcessStartRequest{Cmd: []string{"sleep", "9"}})
	var p api.ProcessInfo
	json.NewDecoder(resp.Body).Decode(&p)
	resp.Body.Close()
	if resp.StatusCode != 201 || p.PID != 1 || p.State != api.ProcessRunning {
		t.Fatalf("start: %d %+v", resp.StatusCode, p)
	}
	resp = pide(t, srv, tokenAlice, "POST", base+"/processes", api.ProcessStartRequest{})
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("sin cmd: %d", resp.StatusCode)
	}

	resp = pide(t, srv, tokenAlice, "GET", base+"/processes", nil)
	var lista []api.ProcessInfo
	json.NewDecoder(resp.Body).Decode(&lista)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(lista) != 1 {
		t.Fatalf("list: %d %+v", resp.StatusCode, lista)
	}
	resp = pide(t, srv, tokenAlice, "GET", base+"/processes/1?wait=2", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("get: %d", resp.StatusCode)
	}
	resp = pide(t, srv, tokenAlice, "GET", base+"/processes/7", nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("pid inexistente: %d", resp.StatusCode)
	}
	resp = pide(t, srv, tokenAlice, "GET", base+"/processes/x", nil)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("pid inválido: %d", resp.StatusCode)
	}
	resp = pide(t, srv, tokenAlice, "POST", base+"/processes/1/signal", api.ProcessSignalRequest{Signal: "KILL"})
	var fin api.ProcessInfo
	json.NewDecoder(resp.Body).Decode(&fin)
	resp.Body.Close()
	if resp.StatusCode != 200 || fin.State != api.ProcessExited || f.procs.senales[0] != "KILL" {
		t.Errorf("signal: %d %+v %v", resp.StatusCode, fin, f.procs.senales)
	}
	resp = pide(t, srv, tokenAlice, "DELETE", base+"/processes/1", nil)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
}

func TestProcesosSalidaNDJSON(t *testing.T) {
	srv, _, base := bancoProcs(t)
	resp := pide(t, srv, tokenAlice, "GET", base+"/processes/1/output?offset=3&follow=1", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "ndjson") {
		t.Fatalf("output: %d %v", resp.StatusCode, resp.Header)
	}
	var evs []api.ProcessEvent
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev api.ProcessEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	if len(evs) < 2 || string(evs[0].Data) != "a\n" || evs[len(evs)-1].Exit == nil || evs[len(evs)-1].Offset != 5 {
		t.Errorf("eventos: %+v", evs)
	}
	resp2 := pide(t, srv, tokenAlice, "GET", base+"/processes/1/output?offset=-1", nil)
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Errorf("offset negativo: %d", resp2.StatusCode)
	}
}

func TestProcesosYSesionesSoloDelDueno(t *testing.T) {
	srv, _, base := bancoProcs(t)
	casos := []struct{ m, p string }{
		{"POST", "/processes"}, {"GET", "/processes"}, {"GET", "/processes/1"}, {"DELETE", "/processes/1"},
		{"POST", "/processes/1/signal"}, {"GET", "/processes/1/output"},
		{"POST", "/sessions"}, {"GET", "/sessions"}, {"GET", "/sessions/s1"}, {"DELETE", "/sessions/s1"},
	}
	for _, c := range casos {
		var body any
		switch {
		case c.m == "POST" && strings.HasSuffix(c.p, "/signal"):
			body = api.ProcessSignalRequest{}
		case c.m == "POST" && c.p == "/sessions":
			body = api.SessionRequest{}
		case c.m == "POST":
			body = api.ProcessStartRequest{Cmd: []string{"x"}}
		}
		resp := pide(t, srv, tokenBob, c.m, base+c.p, body)
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("bob %s %s: %d, quiero 404", c.m, c.p, resp.StatusCode)
		}
		resp = pide(t, srv, "", c.m, base+c.p, body)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("sin token %s %s: %d, quiero 401", c.m, c.p, resp.StatusCode)
		}
	}
}

func TestProcesosTopes(t *testing.T) {
	srv, _, base := bancoProcs(t, Tenant{Nombre: "alice", Token: tokenAlice, MaxProcesos: 2, MaxSesiones: 1})
	for i := 0; i < 2; i++ {
		resp := pide(t, srv, tokenAlice, "POST", base+"/processes", api.ProcessStartRequest{Cmd: []string{"x"}})
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("proceso %d: %d", i, resp.StatusCode)
		}
	}
	resp := pide(t, srv, tokenAlice, "POST", base+"/processes", api.ProcessStartRequest{Cmd: []string{"x"}})
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Errorf("tercer proceso: %d, quiero 429", resp.StatusCode)
	}
	resp = pide(t, srv, tokenAlice, "POST", base+"/sessions", api.SessionRequest{})
	resp.Body.Close()
	resp2 := pide(t, srv, tokenAlice, "POST", base+"/sessions", api.SessionRequest{})
	resp2.Body.Close()
	if resp.StatusCode != 201 || resp2.StatusCode != 429 {
		t.Errorf("sesiones: %d y %d, quiero 201 y 429", resp.StatusCode, resp2.StatusCode)
	}
}

func TestSesionesCicloDeVida(t *testing.T) {
	srv, f, base := bancoProcs(t)
	resp := pide(t, srv, tokenAlice, "POST", base+"/sessions", api.SessionRequest{ID: "dev", Dir: "/work", Env: []string{"A=1"}})
	var se api.Session
	json.NewDecoder(resp.Body).Decode(&se)
	resp.Body.Close()
	if resp.StatusCode != 201 || se.ID != "dev" || f.procs.ultimaSe.Dir != "/work" {
		t.Fatalf("create: %d %+v", resp.StatusCode, se)
	}
	resp = pide(t, srv, tokenAlice, "GET", base+"/sessions", nil)
	var l []api.Session
	json.NewDecoder(resp.Body).Decode(&l)
	resp.Body.Close()
	if len(l) != 1 {
		t.Errorf("list: %+v", l)
	}
	resp = pide(t, srv, tokenAlice, "GET", base+"/sessions/dev", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("get: %d", resp.StatusCode)
	}
	// exec con sesión: el frontal deja pasar el campo.
	resp = pide(t, srv, tokenAlice, "POST", base+"/exec?wait=1", api.ExecRequest{Cmd: []string{"pwd"}, Session: "dev"})
	resp.Body.Close()
	if f.ultimoExec.Session != "dev" {
		t.Errorf("exec no llevó la sesión: %+v", f.ultimoExec)
	}
	resp = pide(t, srv, tokenAlice, "DELETE", base+"/sessions/dev", nil)
	resp.Body.Close()
	resp2 := pide(t, srv, tokenAlice, "GET", base+"/sessions/dev", nil)
	resp2.Body.Close()
	if resp.StatusCode != 204 || resp2.StatusCode != 404 {
		t.Errorf("delete: %d, luego %d", resp.StatusCode, resp2.StatusCode)
	}
}
