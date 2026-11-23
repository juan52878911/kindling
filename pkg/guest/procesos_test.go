package guest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/juan52878911/kindling/pkg/api"
)

// servidorProcs monta los manejadores de procesos y sesiones en un servidor
// real (el seguimiento de salida necesita escritura concurrente con la lectura,
// que httptest.Recorder no permite).
func servidorProcs(t *testing.T) *httptest.Server {
	t.Helper()
	limpiarProcesos()
	mux := http.NewServeMux()
	h := ProcessesHandlerFunc(func() []string { return os.Environ() })
	mux.HandleFunc(api.GuestProcessesPath, h)
	mux.HandleFunc(api.GuestProcessesPath+"/", h)
	s := SessionsHandler()
	mux.HandleFunc(api.GuestSessionsPath, s)
	mux.HandleFunc(api.GuestSessionsPath+"/", s)
	mux.HandleFunc("/exec/stream", StreamExecHandler(os.Environ()))
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.Close(); limpiarProcesos() })
	return srv
}

func llamar(t *testing.T, srv *httptest.Server, method, path string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v (%s)", method, path, err, b)
		}
	}
	return resp.StatusCode
}

func lanzar(t *testing.T, srv *httptest.Server, req api.ProcessStartRequest) api.ProcessInfo {
	t.Helper()
	var p api.ProcessInfo
	if c := llamar(t, srv, http.MethodPost, "/processes", req, &p); c != http.StatusCreated {
		t.Fatalf("start: status %d", c)
	}
	if p.PID <= 0 {
		t.Fatalf("start: %+v", p)
	}
	return p
}

func esperar(t *testing.T, srv *httptest.Server, pid int) api.ProcessInfo {
	t.Helper()
	var p api.ProcessInfo
	llamar(t, srv, http.MethodGet, fmt.Sprintf("/processes/%d?wait=10", pid), nil, &p)
	if p.State != api.ProcessExited {
		t.Fatalf("el proceso %d no terminó: %+v", pid, p)
	}
	return p
}

func salida(t *testing.T, srv *httptest.Server, pid int, query string) (out string, evs []api.ProcessEvent) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/processes/%d/output?%s", srv.URL, pid, query))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev api.ProcessEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		sb.Write(ev.Data)
		evs = append(evs, ev)
	}
	return sb.String(), evs
}

func TestProcesoDeFondoSobreviveYGuardaSalidaYCodigo(t *testing.T) {
	srv := servidorProcs(t)
	p := lanzar(t, srv, api.ProcessStartRequest{Cmd: []string{"sh", "-c", "echo hola; sleep 0.1; echo err >&2; sleep 0.1; exit 7"}})
	fin := esperar(t, srv, p.PID)
	if fin.ExitCode == nil || *fin.ExitCode != 7 || fin.EndedAt == nil || fin.Signal != "" {
		t.Fatalf("estado final %+v", fin)
	}
	out, evs := salida(t, srv, p.PID, "")
	if out != "hola\nerr\n" {
		t.Fatalf("salida %q", out)
	}
	if last := evs[len(evs)-1]; last.Exit == nil || *last.Exit != 7 || last.Offset != 9 {
		t.Fatalf("último evento %+v", last)
	}
	// Desde un offset: solo lo que falta.
	out, _ = salida(t, srv, p.PID, "offset=5")
	if out != "err\n" {
		t.Fatalf("desde offset 5: %q", out)
	}
	// Lista y borrado.
	var lista []api.ProcessInfo
	llamar(t, srv, http.MethodGet, "/processes", nil, &lista)
	if len(lista) != 1 || lista[0].PID != p.PID {
		t.Fatalf("lista %+v", lista)
	}
	if c := llamar(t, srv, http.MethodDelete, fmt.Sprintf("/processes/%d", p.PID), nil, nil); c != http.StatusNoContent {
		t.Fatalf("delete %d", c)
	}
	if c := llamar(t, srv, http.MethodGet, fmt.Sprintf("/processes/%d", p.PID), nil, nil); c != http.StatusNotFound {
		t.Fatalf("tras borrar: %d", c)
	}
}

func TestProcesoSeguirSalidaEnVivoYSenal(t *testing.T) {
	srv := servidorProcs(t)
	p := lanzar(t, srv, api.ProcessStartRequest{Cmd: []string{"sh", "-c", "echo uno; sleep 30; echo nunca"}})

	resp, err := http.Get(fmt.Sprintf("%s/processes/%d/output?follow=1", srv.URL, p.PID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() {
		t.Fatal("sin primer evento")
	}
	var ev api.ProcessEvent
	_ = json.Unmarshal(sc.Bytes(), &ev)
	if string(ev.Data) != "uno\n" || ev.Offset != 4 {
		t.Fatalf("primer evento %+v", ev)
	}
	// Se señala mientras alguien sigue la salida: el flujo acaba con Exit.
	var info api.ProcessInfo
	if c := llamar(t, srv, http.MethodPost, fmt.Sprintf("/processes/%d/signal", p.PID), api.ProcessSignalRequest{Signal: "SIGTERM"}, &info); c != http.StatusOK {
		t.Fatalf("signal %d", c)
	}
	var last api.ProcessEvent
	for sc.Scan() {
		_ = json.Unmarshal(sc.Bytes(), &last)
	}
	if last.Exit == nil || *last.Exit != 143 || last.Signal != "TERM" {
		t.Fatalf("evento final %+v", last)
	}
	// Señalar a un proceso ya terminado es un conflicto, no un éxito mudo.
	if c := llamar(t, srv, http.MethodPost, fmt.Sprintf("/processes/%d/signal", p.PID), api.ProcessSignalRequest{}, nil); c != http.StatusConflict {
		t.Fatalf("señal a un muerto: %d", c)
	}
	if c := llamar(t, srv, http.MethodPost, fmt.Sprintf("/processes/%d/signal", p.PID), api.ProcessSignalRequest{Signal: "NOPE"}, nil); c != http.StatusBadRequest {
		t.Fatalf("señal desconocida: %d", c)
	}
}

func TestProcesoEsperarConPlazoDevuelveRunning(t *testing.T) {
	srv := servidorProcs(t)
	p := lanzar(t, srv, api.ProcessStartRequest{Cmd: []string{"sleep", "30"}})
	t0 := time.Now()
	var got api.ProcessInfo
	llamar(t, srv, http.MethodGet, fmt.Sprintf("/processes/%d?wait=1", p.PID), nil, &got)
	if got.State != api.ProcessRunning || time.Since(t0) < 900*time.Millisecond {
		t.Fatalf("wait=1 devolvió %+v tras %s", got, time.Since(t0))
	}
	// DELETE de uno vivo lo mata y lo olvida.
	llamar(t, srv, http.MethodDelete, fmt.Sprintf("/processes/%d", p.PID), nil, nil)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscallKill0(p.PID); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("el proceso sigue vivo tras DELETE")
}

func TestProcesoPlazoLoMata(t *testing.T) {
	srv := servidorProcs(t)
	p := lanzar(t, srv, api.ProcessStartRequest{Cmd: []string{"sleep", "30"}, TimeoutSeconds: 1})
	fin := esperar(t, srv, p.PID)
	if !fin.TimedOut || fin.Signal != "KILL" || fin.ExitCode == nil || *fin.ExitCode != 137 {
		t.Fatalf("%+v", fin)
	}
}

func TestProcesoAnilloDescartaLoViejoYLoDice(t *testing.T) {
	srv := servidorProcs(t)
	// 100 KiB con un anillo de 4 KiB: queda el final.
	p := lanzar(t, srv, api.ProcessStartRequest{
		Cmd:            []string{"sh", "-c", "head -c 102400 /dev/zero | tr '\\0' a; printf FIN"},
		MaxOutputBytes: 4096,
	})
	fin := esperar(t, srv, p.PID)
	if fin.OutputBytes != 102403 || fin.OutputStart != 102403-4096 {
		t.Fatalf("info %+v", fin)
	}
	out, evs := salida(t, srv, p.PID, "offset=0")
	if !strings.HasSuffix(out, "aaaFIN") || int64(len(out)) != fin.OutputBytes-fin.OutputStart {
		t.Fatalf("salida de %d bytes, termina en %q", len(out), out[max(0, len(out)-6):])
	}
	if evs[0].Dropped != fin.OutputStart {
		t.Fatalf("dropped = %d, want %d", evs[0].Dropped, fin.OutputStart)
	}
}

func TestProcesosTopeDeVivosYEntradas(t *testing.T) {
	srv := servidorProcs(t)
	for i := 0; i < api.ProcessMaxRunning; i++ {
		lanzar(t, srv, api.ProcessStartRequest{Cmd: []string{"sleep", "30"}})
	}
	if c := llamar(t, srv, http.MethodPost, "/processes", api.ProcessStartRequest{Cmd: []string{"true"}}, nil); c != http.StatusTooManyRequests {
		t.Fatalf("con %d vivos: %d, want 429", api.ProcessMaxRunning, c)
	}
}

func TestProcesosOlvidaElTerminadoMasViejoAlLlenarse(t *testing.T) {
	srv := servidorProcs(t)
	var primero int
	for i := 0; i < api.ProcessMaxEntries+3; i++ {
		p := lanzar(t, srv, api.ProcessStartRequest{Cmd: []string{"true"}})
		if i == 0 {
			primero = p.PID
		}
		esperar(t, srv, p.PID)
	}
	var lista []api.ProcessInfo
	llamar(t, srv, http.MethodGet, "/processes", nil, &lista)
	if len(lista) != api.ProcessMaxEntries {
		t.Fatalf("la tabla tiene %d, want %d", len(lista), api.ProcessMaxEntries)
	}
	if c := llamar(t, srv, http.MethodGet, fmt.Sprintf("/processes/%d", primero), nil, nil); c != http.StatusNotFound {
		t.Fatalf("el más viejo sigue: %d", c)
	}
}

func TestProcesoRechazaLoMalformado(t *testing.T) {
	srv := servidorProcs(t)
	for nombre, c := range map[string]struct {
		m, path string
		body    any
		want    int
	}{
		"sin cmd":          {"POST", "/processes", api.ProcessStartRequest{}, 400},
		"binario inexiste": {"POST", "/processes", api.ProcessStartRequest{Cmd: []string{"no-existe-jamas-xyz"}}, 400},
		"pid raro":         {"GET", "/processes/abc", nil, 400},
		"pid ausente":      {"GET", "/processes/999999", nil, 404},
		"sesión ausente":   {"POST", "/processes", api.ProcessStartRequest{Cmd: []string{"true"}, Session: "nada"}, 404},
		"método":           {"PUT", "/processes", nil, 405},
		"anillo enorme":    {"POST", "/processes", api.ProcessStartRequest{Cmd: []string{"true"}, MaxOutputBytes: api.ProcessMaxOutput + 1}, 400},
	} {
		if got := llamar(t, srv, c.m, c.path, c.body, nil); got != c.want {
			t.Errorf("%s: %d, want %d", nombre, got, c.want)
		}
	}
}

func TestLimpiarProcesosMataLoVivoYBorraSesiones(t *testing.T) {
	srv := servidorProcs(t)
	p := lanzar(t, srv, api.ProcessStartRequest{Cmd: []string{"sleep", "30"}})
	llamar(t, srv, http.MethodPost, "/sessions", api.SessionRequest{ID: "x"}, nil)
	if n := limpiarProcesos(); n != 1 {
		t.Fatalf("limpiarProcesos = %d, want 1", n)
	}
	var lista []api.ProcessInfo
	llamar(t, srv, http.MethodGet, "/processes", nil, &lista)
	var ss []api.Session
	llamar(t, srv, http.MethodGet, "/sessions", nil, &ss)
	if len(lista) != 0 || len(ss) != 0 {
		t.Fatalf("quedó %v %v", lista, ss)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscallKill0(p.PID) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("sigue vivo")
}

func TestRutasDeProcesosYSesionesSonDeControl(t *testing.T) {
	for _, p := range []string{"/processes", "/processes/12/output", "/sessions", "/sessions/a"} {
		if !IsControlPath(p) {
			t.Errorf("%s no es ruta de control: un proxy de terceros la alcanzaría", p)
		}
	}
}

// R6-I2 (R2): la lista recorta Cmd a maxCmdLista por entrada.
func TestRecortarCmdDeLaLista(t *testing.T) {
	grande := []string{"sh", "-c", strings.Repeat("é", 1<<20)}
	r := recortarCmd(grande, maxCmdLista)
	n := 0
	for _, a := range r {
		n += len(a)
		if !utf8.ValidString(a) {
			t.Fatalf("argumento cortado a mitad de un carácter")
		}
	}
	if n > maxCmdLista || len(r) != 3 {
		t.Fatalf("recortado: %d bytes, %d argumentos", n, len(r))
	}
	if len(grande[2]) != 2<<20 {
		t.Fatal("el original se modificó")
	}
	if got := recortarCmd([]string{"ls", "-l"}, maxCmdLista); len(got) != 2 {
		t.Fatalf("un cmd corto no se toca: %v", got)
	}
	if got := recortarCmd([]string{"a", "b", "c"}, 2); len(got) != 2 || got[1] != "b" {
		t.Fatalf("omite los siguientes: %v", got)
	}
}
