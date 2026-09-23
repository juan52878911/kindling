package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
)

func eventsFrom(lines ...string) io.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

func collect(ch <-chan api.ExecEvent) []api.ExecEvent {
	var out []api.ExecEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func TestReadExecEventsNoSeFiaDelInvitado(t *testing.T) {
	// El invitado manda más de lo pedido, un flujo con nombre raro, y sigue
	// escribiendo después del final: nada de eso llega a quien llama.
	evs := collect(readExecEvents(eventsFrom(
		`{"stream":"stdout","data":"`+b64("abcdef")+`"}`,
		`{"stream":"evil","data":"`+b64("zzz")+`"}`,
		`{"stream":"stdout","data":"`+b64("ghi")+`"}`,
		`{"exit":0,"duration_ms":5}`,
		`{"stream":"stdout","data":"`+b64("despues")+`"}`,
	), 4))
	if len(evs) != 2 {
		t.Fatalf("events %+v", evs)
	}
	if string(evs[0].Data) != "abcd" {
		t.Fatalf("first chunk %q", evs[0].Data)
	}
	if evs[1].Exit == nil || !evs[1].Truncated {
		t.Fatalf("final %+v: the daemon cut the output, it must say so", evs[1])
	}
}

func TestReadExecEventsSinFinalEsError(t *testing.T) {
	evs := collect(readExecEvents(eventsFrom(`{"stream":"stderr","data":"`+b64("x")+`"}`), 1<<20))
	last := evs[len(evs)-1]
	if last.Error == "" {
		t.Fatalf("a stream cut without exit must end in an error, got %+v", last)
	}
	evs = collect(readExecEvents(eventsFrom(`no es json`), 1<<20))
	if len(evs) != 1 || evs[0].Error == "" {
		t.Fatalf("garbage from the guest: %+v", evs)
	}
}

func TestAggregateExec(t *testing.T) {
	res, err := aggregateExec(readExecEvents(eventsFrom(
		`{"stream":"stdout","data":"`+b64("o")+`"}`,
		`{"stream":"stderr","data":"`+b64("e")+`"}`,
		`{"exit":2,"timed_out":true}`,
	), 1<<20))
	if err != nil || res.ExitCode != 2 || string(res.Stdout) != "o" || string(res.Stderr) != "e" || !res.TimedOut {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := aggregateExec(readExecEvents(eventsFrom(`{"error":"boom"}`), 1)); err == nil || err.Error() != "boom" {
		t.Fatalf("error event: %v", err)
	}
}

func TestExecStatus(t *testing.T) {
	for err, want := range map[error]int{
		machine.ErrNoMachine:                              404,
		fmt.Errorf("x: %w", machine.ErrExecNotAllowed):    403,
		fmt.Errorf("x: %w", machine.ErrNotRunning):        409,
		fmt.Errorf("x: %w", machine.ErrExecNotInSnapshot): 409,
		errors.New("otra cosa"):                           502,
	} {
		if got := execStatus(err); got != want {
			t.Errorf("%v: %d, want %d", err, got, want)
		}
	}
}

func TestRutasDeExecYSandboxSinMaquina(t *testing.T) {
	_, h := testServer(t)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rr
	}
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/machines/nada/exec", `{"cmd":["true"]}`, 404},
		{"POST", "/machines/nada/exec", `{}`, 400},
		{"POST", "/machines/nada/exec", `{"cmd":["true"],"timeout_seconds":99999}`, 400},
		{"GET", "/machines/nada/files?path=/etc/hosts", "", 404},
		{"GET", "/machines/nada/files", "", 400},
		{"POST", "/sandboxes", `{}`, 400},
		{"POST", "/sandboxes", `{"image":"a","from":"b"}`, 400},
		{"POST", "/sandboxes", `{"image":"a","ttl_seconds":999999}`, 400},
		{"POST", "/sandboxes", `{"image":"no-existe"}`, 400},
		{"POST", "/sandboxes", `{"image":"a","on_ttl":"apagar"}`, 400},
		{"GET", "/sandboxes/nada", "", 404},
		{"POST", "/sandboxes/nada/renew", `{}`, 404},
		{"DELETE", "/sandboxes/nada", "", 404},
	}
	for _, c := range cases {
		if rr := do(c.method, c.path, c.body); rr.Code != c.want {
			t.Errorf("%s %s %s: %d (%s), want %d", c.method, c.path, c.body, rr.Code, strings.TrimSpace(rr.Body.String()), c.want)
		}
	}
	rr := do("GET", "/sandboxes", "")
	var list []api.Machine
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &list) != nil || len(list) != 0 {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
}

func b64(s string) string {
	b, _ := json.Marshal([]byte(s))
	return strings.Trim(string(b), `"`)
}

// Daemon e invitado de verdad, sin microVM: el agente en httptest y el mismo
// camino que usa handleExec.
func TestExecContraElAgenteDelInvitado(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/exec/stream", guest.StreamExecHandler(nil))
	g := httptest.NewServer(mux)
	defer g.Close()

	body, code, err := openGuestExec(context.Background(), g.URL, api.ExecRequest{
		Cmd: []string{"sh", "-c", "printf hola; printf adios >&2; exit 4"},
	})
	if err != nil {
		t.Fatalf("%d %v", code, err)
	}
	defer body.Close()
	res, err := aggregateExec(readExecEvents(body, 1<<20))
	if err != nil || res.ExitCode != 4 || string(res.Stdout) != "hola" || string(res.Stderr) != "adios" {
		t.Fatalf("%+v %v", res, err)
	}

	// Un agente sin la ruta (imagen anterior a v0.7) da 501, no un 404 críptico.
	viejo := httptest.NewServer(http.NewServeMux())
	defer viejo.Close()
	if _, code, err := openGuestExec(context.Background(), viejo.URL, api.ExecRequest{Cmd: []string{"true"}}); code != http.StatusNotImplemented || err == nil {
		t.Fatalf("old agent: %d %v", code, err)
	}
}

// El proxy solo llega al puerto del agente, salvo que la máquina declare otros.
func TestProxySoloAPuertosDeclarados(t *testing.T) {
	mc := &api.Machine{Name: "m", Labels: map[string]string{api.LabelPorts: "9000, 9001"}}
	for port, want := range map[int]bool{api.GuestPort: true, 9000: true, 9001: true, 22: false, 9002: false} {
		if got := puertoPermitido(mc, port); got != want {
			t.Errorf("puerto %d: %v, quería %v", port, got, want)
		}
	}
	if puertoPermitido(&api.Machine{Name: "sin"}, 22) {
		t.Error("sin etiqueta solo vale el puerto del agente")
	}
}
