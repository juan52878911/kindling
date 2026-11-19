package e2b

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// procFalso es un proceso de fondo del gateway falso.
type procFalso struct {
	req    map[string]any
	estado string
	senal  string
	salida int
	fin    chan struct{}
}

// fondoFalso imita /v1/sandboxes/sbx1/processes del gateway.
type fondoFalso struct {
	mu        sync.Mutex
	procs     map[int]*procFalso
	senales   []string
	borrados  []int
	terminaYa bool // el proceso acaba solo nada más arrancar
}

func (f *fondoFalso) servir(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.procs == nil {
		f.procs = map[int]*procFalso{}
	}
	f.mu.Unlock()
	resto := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/sbx1/processes"), "/")
	j := func(st int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		json.NewEncoder(w).Encode(v)
	}
	info := func(pid int, p *procFalso) map[string]any {
		cmd, _ := p.req["cmd"].([]any)
		m := map[string]any{"pid": pid, "cmd": cmd, "state": p.estado}
		if d, ok := p.req["dir"]; ok {
			m["dir"] = d
		}
		if p.estado == "exited" {
			m["exit_code"] = p.salida
			if p.senal != "" {
				m["signal"] = p.senal
			}
		}
		return m
	}
	if resto == "" {
		switch r.Method {
		case "POST":
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			pid := len(f.procs) + 1
			p := &procFalso{req: req, estado: "running", fin: make(chan struct{})}
			if f.terminaYa {
				p.estado, p.salida = "exited", 0
				close(p.fin)
			}
			f.procs[pid] = p
			out := info(pid, p)
			f.mu.Unlock()
			j(201, out)
		case "GET":
			f.mu.Lock()
			defer f.mu.Unlock()
			out := []any{}
			for pid, p := range f.procs {
				out = append(out, info(pid, p))
			}
			j(200, out)
		}
		return
	}
	partes := strings.Split(resto, "/")
	pid, _ := strconv.Atoi(partes[0])
	f.mu.Lock()
	p := f.procs[pid]
	f.mu.Unlock()
	if p == nil {
		j(404, map[string]any{"message": "no such process"})
		return
	}
	switch {
	case len(partes) == 1 && r.Method == "GET":
		f.mu.Lock()
		defer f.mu.Unlock()
		j(200, info(pid, p))
	case len(partes) == 1 && r.Method == "DELETE":
		f.mu.Lock()
		f.borrados = append(f.borrados, pid)
		delete(f.procs, pid)
		f.mu.Unlock()
		w.WriteHeader(204)
	case partes[1] == "signal":
		var req struct{ Signal string }
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.senales = append(f.senales, req.Signal)
		if p.estado == "running" {
			p.estado, p.senal, p.salida = "exited", req.Signal, 137
			close(p.fin)
		}
		out := info(pid, p)
		f.mu.Unlock()
		j(200, out)
	case partes[1] == "output":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, `{"stream":"stdout","data":%q,"offset":5}`+"\n", base64.StdEncoding.EncodeToString([]byte("hola\n")))
		fl.Flush()
		select {
		case <-p.fin:
		case <-r.Context().Done():
			return
		}
		f.mu.Lock()
		c := p.salida
		f.mu.Unlock()
		fmt.Fprintf(w, `{"offset":5,"exit":%d}`+"\n", c)
	}
}

func (f *fondoFalso) vivos() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.procs {
		if p.estado == "running" {
			n++
		}
	}
	return n
}

// leerFrame lee un sobre Connect y devuelve flags y carga.
func leerFrame(t *testing.T, r io.Reader) (byte, string) {
	t.Helper()
	var c [5]byte
	if _, err := io.ReadFull(r, c[:]); err != nil {
		t.Fatalf("leyendo el sobre: %v", err)
	}
	carga := make([]byte, int(c[1])<<24|int(c[2])<<16|int(c[3])<<8|int(c[4]))
	if _, err := io.ReadFull(r, carga); err != nil {
		t.Fatalf("leyendo la carga: %v", err)
	}
	return c[0], string(carga)
}

func TestFondoStartDeProcesoQueTermina(t *testing.T) {
	b := nuevoBanco(t)
	b.gw.fondo = &fondoFalso{terminaYa: true}
	tok := (&Servidor{Clave: clave}).tokenEnvd("sbx1")
	cab := map[string]string{"X-Access-Token": tok, "Content-Type": "application/connect+json", "Connect-Timeout-Ms": "60000"}
	resp := b.peticion(t, "POST", "/process.Process/Start", "49983-onrhqmi.e2b.test", cab,
		strings.NewReader(string(envelope(`{"process":{"cmd":"/bin/bash","args":["-c","echo hola"],"envs":{"A":"1"},"cwd":"/work"}}`))))
	got := leer(t, resp)
	want := string(envelope(`{"event":{"start":{"pid":1}}}`)) +
		string(envelope(`{"event":{"data":{"stdout":"aG9sYQo="}}}`)) +
		string(envelope(`{"event":{"end":{"exitCode":0,"exited":true,"status":"exit status 0"}}}`))
	fin := envelope(`{}`)
	fin[0] = 0x02
	want += string(fin)
	if string(got) != want {
		t.Fatalf("frames\n got: %q\nwant: %q", got, want)
	}
	f := b.gw.fondo
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(b.gw.execs) != 0 {
		t.Errorf("no debía usar exec: %v", b.gw.execs)
	}
	if len(f.borrados) != 1 {
		t.Errorf("el proceso entregado debía borrarse: %v", f.borrados)
	}
}

func TestFondoSobreviveALaDesconexionYSeReconecta(t *testing.T) {
	b := nuevoBanco(t)
	f := &fondoFalso{}
	b.gw.fondo = f
	tok := (&Servidor{Clave: clave}).tokenEnvd("sbx1")
	host := "49983-onrhqmi.e2b.test"
	cab := map[string]string{"X-Access-Token": tok, "Content-Type": "application/connect+json"}
	cu := map[string]string{"X-Access-Token": tok, "Content-Type": "application/json"}

	resp := b.peticion(t, "POST", "/process.Process/Start", host, cab,
		strings.NewReader(string(envelope(`{"process":{"cmd":"sleep","args":["99"]},"tag":"dormir"}`))))
	if _, c := leerFrame(t, resp.Body); c != `{"event":{"start":{"pid":1}}}` {
		t.Fatalf("start: %s", c)
	}
	if _, c := leerFrame(t, resp.Body); !strings.Contains(c, `"stdout":"aG9sYQo="`) {
		t.Fatalf("data: %s", c)
	}
	// El cliente se va: el proceso tiene que seguir vivo y sin señales ni borrado.
	resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	req := f.procs[1].req
	nSenales, nBorrados := len(f.senales), len(f.borrados)
	f.mu.Unlock()
	if nSenales != 0 || nBorrados != 0 || f.vivos() != 1 {
		t.Fatalf("la desconexión tocó el proceso: senales=%d borrados=%d vivos=%d", nSenales, nBorrados, f.vivos())
	}
	if _, hay := req["timeout_seconds"]; hay || fmt.Sprint(req["cmd"]) != "[sleep 99]" {
		t.Errorf("petición al gateway: %v", req)
	}
	if len(b.gw.execs) != 0 {
		t.Errorf("no debía usar exec: %v", b.gw.execs)
	}

	// List lo ve, con su etiqueta.
	r := b.peticion(t, "POST", "/process.Process/List", host, cu, strings.NewReader(`{}`))
	if got := string(leer(t, r)); !strings.Contains(got, `"pid":1`) || !strings.Contains(got, `"tag":"dormir"`) || !strings.Contains(got, `"cmd":"sleep"`) {
		t.Fatalf("list: %s", got)
	}

	// Connect por etiqueta: start, y la salida desde el principio.
	con := b.peticion(t, "POST", "/process.Process/Connect", host, cab,
		strings.NewReader(string(envelope(`{"process":{"tag":"dormir"}}`))))
	defer con.Body.Close()
	if _, c := leerFrame(t, con.Body); c != `{"event":{"start":{"pid":1}}}` {
		t.Fatalf("connect start: %s", c)
	}
	if _, c := leerFrame(t, con.Body); !strings.Contains(c, `"stdout":"aG9sYQo="`) {
		t.Fatalf("connect data: %s", c)
	}

	// SendSignal por pid: el gateway recibe KILL y Connect acaba con la señal.
	r = b.peticion(t, "POST", "/process.Process/SendSignal", host, cu, strings.NewReader(`{"process":{"pid":1},"signal":"SIGNAL_SIGKILL"}`))
	if got := leer(t, r); r.StatusCode != 200 {
		t.Fatalf("signal: %d %s", r.StatusCode, got)
	}
	resto, _ := io.ReadAll(con.Body)
	if !strings.Contains(string(resto), `"exitCode":137`) || !strings.Contains(string(resto), `"status":"signal: killed"`) {
		t.Fatalf("fin tras señal: %q", resto)
	}
	f.mu.Lock()
	if len(f.senales) != 1 || f.senales[0] != "KILL" {
		t.Errorf("señales al gateway: %v", f.senales)
	}
	f.mu.Unlock()

	// Ya entregado y borrado: Connect y SendSignal dan not_found.
	r = b.peticion(t, "POST", "/process.Process/SendSignal", host, cu, strings.NewReader(`{"process":{"pid":1},"signal":"SIGNAL_SIGTERM"}`))
	if got := leer(t, r); r.StatusCode != 404 {
		t.Fatalf("señal a un proceso borrado: %d %s", r.StatusCode, got)
	}
	con2 := b.peticion(t, "POST", "/process.Process/Connect", host, cab,
		strings.NewReader(string(envelope(`{"process":{"pid":7}}`))))
	if _, c := leerFrame(t, con2.Body); !strings.Contains(c, `"not_found"`) {
		t.Fatalf("connect a un pid inexistente: %s", c)
	}
	con2.Body.Close()
}

// El plazo de la conexión (Connect-Timeout-Ms) cierra el stream pero no mata el comando.
func TestFondoPlazoDeConexionNoMataElProceso(t *testing.T) {
	b := nuevoBanco(t)
	f := &fondoFalso{}
	b.gw.fondo = f
	cab := map[string]string{"X-Access-Token": (&Servidor{Clave: clave}).tokenEnvd("sbx1"),
		"Content-Type": "application/connect+json", "Connect-Timeout-Ms": "200"}
	resp := b.peticion(t, "POST", "/process.Process/Start", "49983-onrhqmi.e2b.test", cab,
		strings.NewReader(string(envelope(`{"process":{"cmd":"sleep","args":["99"]}}`))))
	got := string(leer(t, resp))
	if !strings.Contains(got, `"deadline_exceeded"`) {
		t.Fatalf("esperaba deadline_exceeded: %q", got)
	}
	f.mu.Lock()
	n := len(f.senales)
	_, hayTimeout := f.procs[1].req["timeout_seconds"]
	f.mu.Unlock()
	if n != 0 || f.vivos() != 1 || hayTimeout {
		t.Errorf("el plazo tocó el proceso: senales=%d vivos=%d timeout=%v", n, f.vivos(), hayTimeout)
	}
}
