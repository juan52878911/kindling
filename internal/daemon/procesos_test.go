package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
)

// clienteUnix sirve h por un socket unix y devuelve un cliente de pkg/api.
func clienteUnix(t *testing.T, h http.Handler) *api.Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", dir+"/s")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return api.NewClient("unix://" + dir + "/s")
}

// Las rutas existen y validan antes de buscar la máquina.
func TestRutasDeProcesosYSesionesSinMaquina(t *testing.T) {
	_, h := testServer(t)
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/machines/nada/processes", `{"cmd":["true"]}`, 404},
		{"GET", "/machines/nada/processes", "", 404},
		{"GET", "/machines/nada/processes/abc", "", 400},
		{"GET", "/machines/nada/processes/-3/output", "", 400},
		{"POST", "/machines/nada/processes/5/signal", `{}`, 404},
		{"DELETE", "/machines/nada/processes/5", "", 404},
		{"POST", "/machines/nada/sessions", `{}`, 404},
		{"GET", "/machines/nada/sessions/s1", "", 404},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
		if rr.Code != c.want {
			t.Errorf("%s %s: %d (%s), want %d", c.method, c.path, rr.Code, strings.TrimSpace(rr.Body.String()), c.want)
		}
	}
}

// El daemon (reenviar) y el agente de verdad, sin microVM: el agente en
// httptest y el mismo camino de reenvío que usa el daemon.
func TestProcesosYSesionesDeExtremoAExtremo(t *testing.T) {
	s, _ := testServer(t)
	gmux := http.NewServeMux()
	ph := guest.ProcessesHandlerFunc(func() []string { return os.Environ() })
	gmux.HandleFunc(api.GuestProcessesPath, ph)
	gmux.HandleFunc(api.GuestProcessesPath+"/", ph)
	sh := guest.SessionsHandler()
	gmux.HandleFunc(api.GuestSessionsPath, sh)
	gmux.HandleFunc(api.GuestSessionsPath+"/", sh)
	gmux.HandleFunc("/exec/stream", guest.StreamExecHandler(os.Environ()))
	g := httptest.NewServer(gmux)
	defer g.Close()

	dmux := http.NewServeMux()
	for _, ruta := range []string{
		"POST /machines/{ref}/processes", "GET /machines/{ref}/processes",
		"GET /machines/{ref}/processes/{pid}", "DELETE /machines/{ref}/processes/{pid}",
		"GET /machines/{ref}/processes/{pid}/output", "POST /machines/{ref}/processes/{pid}/signal",
		"POST /machines/{ref}/sessions", "GET /machines/{ref}/sessions",
		"GET /machines/{ref}/sessions/{id}", "DELETE /machines/{ref}/sessions/{id}",
	} {
		dmux.HandleFunc(ruta, func(w http.ResponseWriter, r *http.Request) {
			s.reenviar(w, r, g.URL, "m", &api.Machine{})
		})
	}
	cl := clienteUnix(t, dmux)
	ctx := context.Background()

	// Proceso de fondo: pid, espera, salida desde offset y señal.
	p, err := cl.StartProcess(ctx, "m", api.ProcessStartRequest{Cmd: []string{"sh", "-c", "echo uno; sleep 0.2; echo dos; exit 3"}})
	if err != nil || p.PID <= 0 {
		t.Fatalf("start: %+v %v", p, err)
	}
	fin, err := cl.Process(ctx, "m", p.PID, 10*time.Second)
	if err != nil || fin.State != api.ProcessExited || fin.ExitCode == nil || *fin.ExitCode != 3 {
		t.Fatalf("wait: %+v %v", fin, err)
	}
	var out strings.Builder
	exit, next, err := cl.ProcessOutput(ctx, "m", p.PID, 4, true, func(ev api.ProcessEvent) error { out.Write(ev.Data); return nil })
	if err != nil || out.String() != "dos\n" || exit == nil || *exit != 3 || next != 8 {
		t.Fatalf("output: %q exit=%v next=%d err=%v", out.String(), exit, next, err)
	}

	dormilon, _ := cl.StartProcess(ctx, "m", api.ProcessStartRequest{Cmd: []string{"sleep", "30"}})
	if _, err := cl.SignalProcess(ctx, "m", dormilon.PID, "KILL"); err != nil {
		t.Fatal(err)
	}
	fin, _ = cl.Process(ctx, "m", dormilon.PID, 10*time.Second)
	if fin.Signal != "KILL" {
		t.Fatalf("tras KILL: %+v", fin)
	}
	lista, err := cl.Processes(ctx, "m")
	// La tabla del agente es global del proceso de test: se buscan los nuestros.
	vistos := 0
	for _, x := range lista {
		if x.PID == p.PID || x.PID == dormilon.PID {
			vistos++
		}
	}
	if err != nil || vistos != 2 {
		t.Fatalf("lista %+v %v", lista, err)
	}
	if err := cl.RemoveProcess(ctx, "m", p.PID); err != nil {
		t.Fatal(err)
	}
	// Un pid que el agente no conoce llega como 404 con su mensaje.
	if _, err := cl.Process(ctx, "m", p.PID, 0); err == nil || !strings.Contains(err.Error(), "no background process") {
		t.Fatalf("pid borrado: %v", err)
	}

	// Sesión: se crea por el daemon y el exec con session (openGuestExec, lo
	// que hace handleExec) ve el cwd y el entorno del anterior.
	dir := t.TempDir()
	if _, err := cl.CreateSession(ctx, "m", api.SessionRequest{ID: "dev"}); err != nil {
		t.Fatal(err)
	}
	run := func(script string) string {
		body, code, err := openGuestExec(ctx, g.URL, api.ExecRequest{Cmd: []string{"sh", "-c", script}, Session: "dev"})
		if err != nil {
			t.Fatalf("%d %v", code, err)
		}
		defer body.Close()
		res, err := aggregateExec(readExecEvents(body, 1<<20))
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%+v %v", res, err)
		}
		return string(res.Stdout)
	}
	run("cd " + dir + "; export HOLA=mundo")
	if got := run(`echo "$PWD $HOLA"`); got != dir+" mundo\n" {
		t.Fatalf("la sesión no persistió: %q", got)
	}
	ss, err := cl.GetSession(ctx, "m", "dev")
	if err != nil || ss.Dir != dir || ss.Execs != 2 {
		t.Fatalf("sesión %+v %v", ss, err)
	}
	if err := cl.RemoveSession(ctx, "m", "dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.GetSession(ctx, "m", "dev"); err == nil {
		t.Fatal("la sesión borrada sigue")
	}
}

// Seguir la salida llega en vivo por el daemon: el primer trozo no espera al
// final del proceso.
func TestProcesoSeguirSalidaLlegaEnVivoPorElDaemon(t *testing.T) {
	s, _ := testServer(t)
	gmux := http.NewServeMux()
	ph := guest.ProcessesHandlerFunc(func() []string { return os.Environ() })
	gmux.HandleFunc(api.GuestProcessesPath, ph)
	gmux.HandleFunc(api.GuestProcessesPath+"/", ph)
	g := httptest.NewServer(gmux)
	defer g.Close()
	dmux := http.NewServeMux()
	dmux.HandleFunc("GET /machines/{ref}/processes/{pid}/output", func(w http.ResponseWriter, r *http.Request) {
		s.reenviar(w, r, g.URL, "m", &api.Machine{})
	})
	d := httptest.NewServer(dmux)
	defer d.Close()

	b, _ := json.Marshal(api.ProcessStartRequest{Cmd: []string{"sh", "-c", "echo ya; sleep 30"}})
	resp, err := http.Post(g.URL+"/processes", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var p api.ProcessInfo
	_ = json.NewDecoder(resp.Body).Decode(&p)
	resp.Body.Close()
	defer func() {
		req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/processes/%d", g.URL, p.PID), nil)
		if r, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/machines/m/processes/%d/output?follow=1", d.URL, p.PID), nil)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	sc := bufio.NewScanner(r2.Body)
	if !sc.Scan() {
		t.Fatalf("sin evento en vivo: %v", sc.Err())
	}
	var ev api.ProcessEvent
	_ = json.Unmarshal(sc.Bytes(), &ev)
	if string(ev.Data) != "ya\n" {
		t.Fatalf("evento %+v", ev)
	}
}

// Un agente anterior (sin las rutas) da 501 con la explicación, no un 404.
func TestProcesosAgenteAntiguoEs501(t *testing.T) {
	s, _ := testServer(t)
	g := httptest.NewServer(http.NewServeMux())
	defer g.Close()
	rr := httptest.NewRecorder()
	s.reenviar(rr, httptest.NewRequest("GET", "/machines/m/processes", nil), g.URL, "m", &api.Machine{Image: "viejo"})
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}
