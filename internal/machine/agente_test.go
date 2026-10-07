package machine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El "ok" de un agente anterior y cualquier cosa que no sea el JSON de uno
// nuevo son un agente que no anuncia nada: se le sigue sondeando por ruta.
func TestInterpretarAgente(t *testing.T) {
	viejo := interpretarAgente(200, "text/plain; charset=utf-8", []byte("ok\n"))
	if viejo == nil || viejo.Announces() || viejo.Lacks(api.GuestCapResync) {
		t.Fatalf("agente viejo: %+v", viejo)
	}
	if a := interpretarAgente(404, "application/json", []byte(`{"status":"ok","caps":["x"]}`)); a.Announces() {
		t.Fatalf("un 404 no anuncia nada: %+v", a)
	}
	nuevo := interpretarAgente(200, "application/json",
		[]byte(`{"status":"ok","agent":"kling-guest","version":"v0.18.0","caps":["resync","ready"],"futuro":1}`))
	if nuevo.Agent != "kling-guest" || nuevo.Version != "v0.18.0" || !nuevo.Announces() {
		t.Fatalf("agente nuevo: %+v", nuevo)
	}
	if nuevo.Lacks(api.GuestCapReady) || !nuevo.Lacks(api.GuestCapHooks) {
		t.Fatalf("capacidades mal leídas: %+v", nuevo)
	}
}

// El daemon pregunta en segundo plano y lo deja apuntado en la máquina.
func TestConocerAgenteLoApuntaEnLaMaquina(t *testing.T) {
	m, id := managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.GuestHealthPath || !strings.Contains(r.Header.Get("Accept"), "application/json") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.GuestHealth{Status: "ok", Agent: "kling-bridge", Version: "v0.18.0",
			Caps: []string{api.GuestCapResync, api.GuestCapMCP}})
	}))
	m.byID[id].State = api.StateRunning
	m.conocerAgente(id)
	deadline := time.Now().Add(5 * time.Second)
	for m.agenteDe(id) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ag := m.agenteDe(id)
	if ag == nil || ag.Version != "v0.18.0" || ag.Agent != "kling-bridge" || ag.Lacks(api.GuestCapMCP) {
		t.Fatalf("agente apuntado: %+v", ag)
	}
}

// Lo que el agente no anuncia no se le pide: ni /resync, ni /ready, ni
// /hooks. Antes cada restauración de una imagen así costaba la petición y el
// 404.
func TestSinCapacidadNoSeSondea(t *testing.T) {
	var pedidas atomic.Int32
	m, id := managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pedidas.Add(1)
		http.NotFound(w, r)
	}))
	m.byID[id].Agent = &api.GuestAgent{Agent: "kling-guest", Version: "v9.0.0", Caps: []string{api.GuestCapMCP}}
	buf := capturarLog(t)

	if _, ok, _ := m.resyncGuest(context.Background(), id, "", api.ResyncThaw); ok {
		t.Fatal("resync dado por aplicado")
	}
	if !strings.Contains(buf.String(), "predates") {
		t.Fatalf("sin el aviso de agente viejo:\n%s", buf)
	}
	if _, err := m.consultarListo(context.Background(), id); !errors.Is(err, errListoViejo) {
		t.Fatalf("/ready: %v", err)
	}
	if _, err := m.lanzarGanchos(context.Background(), id, "thaw"); !errors.Is(err, errListoViejo) {
		t.Fatalf("/hooks: %v", err)
	}
	if n := pedidas.Load(); n != 0 {
		t.Fatalf("%d peticiones a un agente que ya dijo que no sabe", n)
	}

	// Un agente que no anuncia nada (anterior a v0.18) se sigue sondeando.
	m.byID[id].Agent = &api.GuestAgent{}
	_, _ = m.consultarListo(context.Background(), id)
	if pedidas.Load() == 0 {
		t.Fatal("a un agente viejo hay que seguir preguntándole")
	}
}

// esperarAgente acota cada /healthz con el plazo de /healthz, no con el de
// /ready (125 s): una primera petición que se pierde (el agente aún no
// escuchaba) se repite en vez de comerse la espera entera.
func TestEsperarAgenteRepiteUnaPeticionPerdida(t *testing.T) {
	antes := plazoPeticionAgente
	plazoPeticionAgente = 100 * time.Millisecond
	t.Cleanup(func() { plazoPeticionAgente = antes })

	var pedidas atomic.Int32
	m, id := managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pedidas.Add(1) == 1 {
			// La primera no contesta: el cliente la corta por su plazo.
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.GuestHealth{Status: "ok", Agent: "kling-guest", Version: "v0.18.0",
			Caps: []string{api.GuestCapEnv}})
	}))
	m.byID[id].State = api.StateRunning

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ag := m.esperarAgente(ctx, id)
	if ag == nil || ag.Lacks(api.GuestCapEnv) {
		t.Fatalf("agente = %+v tras %d peticiones: la primera, perdida, se comió la espera", ag, pedidas.Load())
	}
}
