package machine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// El entorno se valida antes de nada, sin repetir valores en el error, y con
// From se rechaza: la copia lleva el entorno del dorado.
func TestEntornoDePeticion(t *testing.T) {
	env, err := entornoDePeticion(api.RunRequest{Image: "x", Env: []string{"PW=uno", "MODE=a=b", "PW=dos"}})
	if err != nil || env["PW"] != "dos" || env["MODE"] != "a=b" || len(env) != 2 {
		t.Fatalf("env %v err %v", env, err)
	}
	if env, err := entornoDePeticion(api.RunRequest{Image: "x"}); env != nil || err != nil {
		t.Fatalf("sin entorno: %v %v", env, err)
	}
	for _, req := range []api.RunRequest{
		{From: "dorado", Env: []string{"PW=secreto-1"}},
		{Image: "x", Env: []string{"1PW=secreto-1"}},
		{Image: "x", Env: []string{"PW secreto-1"}},
		{Image: "x", Env: []string{"PW=secreto-1\x00"}},
	} {
		_, err := entornoDePeticion(req)
		if !errors.Is(err, ErrEnvRequest) || strings.Contains(err.Error(), "secreto-1") {
			t.Errorf("%q: err %v (ErrEnvRequest, sin el valor)", req.Env, err)
		}
	}
}

// Run con From y entorno falla con ErrEnvRequest (400) antes de reservar el
// nombre ni el snapshot.
func TestRunFromConEntornoSeRechaza(t *testing.T) {
	m := newTestManager(t)
	_, err := m.Run(context.Background(), api.RunRequest{Name: "copia", From: "dorado", Env: []string{"PW=x"}})
	if !errors.Is(err, ErrEnvRequest) {
		t.Fatalf("err = %v, quería ErrEnvRequest", err)
	}
}

// Lo que se le pide al VMM: el entorno bajo su clave antes de arrancar, y
// después un merge patch que solo quita esa clave.
func TestEntornoEnMMDS(t *testing.T) {
	f := nuevoFcFalso(t)
	ctx := context.Background()
	if err := ponerEntornoMMDS(ctx, f.cliente(), map[string]string{"PW": "s3"}); err != nil {
		t.Fatal(err)
	}
	if err := borrarEntornoMMDS(ctx, f.cliente()); err != nil {
		t.Fatal(err)
	}
	put := f.llamadasA(http.MethodPut, "/mmds")
	patch := f.llamadasA(http.MethodPatch, "/mmds")
	if len(put) != 1 || len(patch) != 1 {
		t.Fatalf("PUT %d, PATCH %d", len(put), len(patch))
	}
	var doc map[string]map[string]string
	if err := json.Unmarshal(put[0].Cuerpo, &doc); err != nil || doc[api.MachineEnvMMDSKey]["PW"] != "s3" || len(doc) != 1 {
		t.Fatalf("PUT /mmds: %s", put[0].Cuerpo)
	}
	if got := strings.TrimSpace(string(patch[0].Cuerpo)); got != `{"`+api.MachineEnvMMDSKey+`":null}` {
		t.Fatalf("PATCH /mmds: %s", got)
	}
	if envBootArg(false) != "" || envBootArg(true) != " kling.env=1" {
		t.Fatalf("envBootArg: %q %q", envBootArg(false), envBootArg(true))
	}
}

// retirarEntornoMMDS borra el entorno de MMDS cuando el agente contesta, no
// antes: hasta ese momento el agente aún puede estar leyéndolo.
func TestRetirarEntornoTrasContestarElAgente(t *testing.T) {
	m := newTestManager(t)
	f := nuevoFcFalso(t)
	var contesto atomic.Bool
	listo := make(chan struct{})
	agente := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-listo:
		default:
			// Todavía no escucha: un error de transporte, que es lo que
			// hace reintentar.
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
				}
			}
			return
		}
		contesto.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.GuestHealth{Status: "ok", Agent: "kling-guest", Caps: []string{api.GuestCapEnv}})
	}))
	defer agente.Close()
	mc := m.addForTest("abc")
	m.mu.Lock()
	mc.Forwards = map[string]string{"8080": strings.TrimPrefix(agente.URL, "http://")}
	m.socket["abc"] = f.Sock
	m.mu.Unlock()

	m.retirarEntornoMMDS("abc", "abc", map[string]string{"PW": "x"})
	time.Sleep(3 * pasoListo)
	if n := len(f.llamadasA(http.MethodPatch, "/mmds")); n != 0 {
		t.Fatalf("borró el entorno (%d PATCH) antes de que contestara el agente", n)
	}
	close(listo)
	plazo := time.Now().Add(5 * time.Second)
	for len(f.llamadasA(http.MethodPatch, "/mmds")) == 0 {
		if time.Now().After(plazo) {
			t.Fatal("no borró el entorno de MMDS tras contestar el agente")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !contesto.Load() {
		t.Fatal("borró sin que el agente contestara")
	}
}

// En state.json solo van los nombres: la máquina no tiene dónde guardar un
// valor.
func TestEstadoSoloLlevaNombres(t *testing.T) {
	m := newTestManager(t)
	mc := m.addForTest("abc")
	env, _ := entornoDePeticion(api.RunRequest{Image: "x", Env: []string{"PW=valor-secreto"}})
	m.mu.Lock()
	mc.EnvKeys = api.MachineEnvKeys(env)
	m.persist()
	m.mu.Unlock()
	m.persistirYa()
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		t.Fatal(err)
	}
	st := readState(t, m)
	if strings.Contains(string(b), "valor-secreto") || len(st) != 1 || len(st[0].EnvKeys) != 1 || st[0].EnvKeys[0] != "PW" {
		t.Fatalf("state.json: %s", b)
	}
}

// Con un agente que no sabe leer el entorno (una imagen de antes de run -e),
// la máquina falla: su servicio arrancó sin la contraseña que se le pasó.
func TestEntornoConAgenteViejoFalla(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	f := nuevoFcFalso(t)
	agente := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.GuestHealth{Status: "ok", Agent: "kling-guest"})
	}))
	defer agente.Close()
	mc := m.addForTest("abc")
	m.mu.Lock()
	mc.Forwards = map[string]string{"8080": strings.TrimPrefix(agente.URL, "http://")}
	m.socket["abc"] = f.Sock
	m.mu.Unlock()

	m.retirarEntornoMMDS("abc", "abc", map[string]string{"PW": "x"})
	plazo := time.Now().Add(5 * time.Second)
	for {
		m.mu.RLock()
		st, causa := m.byID["abc"].State, m.byID["abc"].LastErr
		m.mu.RUnlock()
		if st == api.StateFailed {
			if !strings.Contains(causa, "environment") {
				t.Fatalf("falló sin decir por qué: %q", causa)
			}
			return
		}
		if time.Now().After(plazo) {
			t.Fatalf("la máquina sigue %s con un agente que no leyó su entorno", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Un PutMMDS mientras el agente aún no ha leído su entorno lo conserva.
func TestConEntornoPendiente(t *testing.T) {
	m := newTestManager(t)
	doc, err := m.conEntornoPendiente("abc", map[string]any{"token": "t"})
	if err != nil || len(doc.(map[string]any)) != 1 {
		t.Fatalf("sin entorno pendiente no se toca: %v %v", doc, err)
	}
	m.entornoPendiente = map[string]map[string]string{"abc": {"PW": "x"}}
	doc, err = m.conEntornoPendiente("abc", map[string]any{"token": "t"})
	if err != nil {
		t.Fatal(err)
	}
	obj := doc.(map[string]any)
	if obj["token"] != "t" || obj[api.MachineEnvMMDSKey].(map[string]string)["PW"] != "x" {
		t.Fatalf("no conservó el entorno: %v", obj)
	}
	doc, err = m.conEntornoPendiente("abc", json.RawMessage(`{"token":"r"}`))
	if err != nil || doc.(map[string]any)["token"] != "r" || doc.(map[string]any)[api.MachineEnvMMDSKey] == nil {
		t.Fatalf("el JSON tal cual del daemon: %v %v", doc, err)
	}
	if _, err := m.conEntornoPendiente("abc", "texto"); err == nil {
		t.Fatal("un almacén que no es un objeto pisaría el entorno")
	}
}
