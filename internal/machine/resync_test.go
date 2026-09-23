package machine

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// managerConInvitado registra una máquina cuyo agente es h, alcanzable por un
// reenvío como en macOS (en Linux sería la IP; Addr resuelve igual).
func managerConInvitado(t *testing.T, h http.Handler) (*Manager, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	m := &Manager{byID: map[string]*api.Machine{}}
	id := "abcdef0123456789"
	m.byID[id] = &api.Machine{ID: id, Name: "r", Image: "img",
		Forwards: map[string]string{"8080": strings.TrimPrefix(srv.URL, "http://")}}
	return m, id
}

// capturarLog redirige el log del paquete mientras dura la prueba.
func capturarLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	antes := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(antes) })
	return &buf
}

func TestResyncGuestMandaHoraYEntropiaNuevas(t *testing.T) {
	var mu sync.Mutex
	var vistos []api.GuestResync
	m, id := managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != api.GuestResyncPath {
			http.NotFound(w, r)
			return
		}
		var req api.GuestResync
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		vistos = append(vistos, req)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"skew_ms":300000}`))
	}))

	antes := time.Now()
	for i := 0; i < 2; i++ {
		if _, ok := m.resyncGuest(context.Background(), id, ""); !ok {
			t.Fatal("resync no se aplicó contra un agente que lo sabe hacer")
		}
	}
	if len(vistos) != 2 {
		t.Fatalf("%d peticiones", len(vistos))
	}
	for _, v := range vistos {
		if len(v.Entropy) != api.GuestResyncEntropy {
			t.Fatalf("entropía de %d bytes", len(v.Entropy))
		}
		if d := time.Unix(0, v.UnixNano).Sub(antes); d < 0 || d > 5*time.Second {
			t.Fatalf("hora enviada desfasada %s", d)
		}
	}
	// Lo que importa: cada restauración recibe entropía DISTINTA.
	if bytes.Equal(vistos[0].Entropy, vistos[1].Entropy) {
		t.Fatal("dos resync mandaron la misma entropía")
	}
}

// Un agente anterior a /resync contesta 404 (kling-guest) o 400 (el puente
// MCP viejo, que atiende "/" entero). La restauración sigue y se avisa UNA vez
// por imagen, no en cada thaw.
func TestResyncGuestAgenteViejoNoFalla(t *testing.T) {
	for code, cuerpo := range map[int]string{
		http.StatusNotFound:   "404 page not found",
		http.StatusBadRequest: "missing header Mcp-Session-Id (send initialize first)", // el del puente viejo
	} {
		m, id := managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, cuerpo, code)
		}))
		buf := capturarLog(t)
		for i := 0; i < 3; i++ {
			took, ok := m.resyncGuest(context.Background(), id, "")
			if ok {
				t.Fatalf("%d: se dio por aplicado", code)
			}
			if took > time.Second {
				t.Fatalf("%d: un agente que contesta no debería costar %s", code, took)
			}
		}
		if n := strings.Count(buf.String(), "predates"); n != 1 {
			t.Fatalf("%d: %d avisos de agente viejo, quería 1:\n%s", code, n, buf)
		}
	}
}

// Sin nadie escuchando (una máquina sin agente) se insiste un instante y se
// sigue: no puede costar el plazo entero en cada restauración.
func TestResyncGuestSinAgente(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	m := &Manager{byID: map[string]*api.Machine{
		"x": {ID: "x", Name: "x", Image: "sin-agente", Forwards: map[string]string{"8080": addr}},
		"y": {ID: "y", Name: "y"}, // sin IP ni reenvíos: ni se intenta
	}}
	buf := capturarLog(t)
	clave := claveSnapshot(&api.Snapshot{Name: "dorado", CreatedAt: time.Now()})
	took, ok := m.resyncGuest(context.Background(), "x", clave)
	if ok || took > resyncReintento+time.Second {
		t.Fatalf("ok=%v took=%s", ok, took)
	}
	// Otra instancia del MISMO snapshot: ni se intenta —nadie escuchaba, y su
	// memoria no cambia— ni se repite el aviso.
	m.byID["z"] = &api.Machine{ID: "z", Name: "z", Image: "sin-agente", Forwards: map[string]string{"8080": addr}}
	if took, ok := m.resyncGuest(context.Background(), "z", clave); ok || took != 0 {
		t.Fatalf("snapshot sin agente recordado: ok=%v took=%s", ok, took)
	}
	// Sin clave (un thaw) no se recuerda nada: se intenta.
	if took, _ := m.resyncGuest(context.Background(), "z", ""); took == 0 {
		t.Fatal("sin clave no debería saltarse el intento")
	}
	if n := strings.Count(buf.String(), "no guest agent"); n != 1 {
		t.Fatalf("%d avisos para una imagen, quería 1:\n%s", n, buf)
	}
	if took, ok := m.resyncGuest(context.Background(), "y", ""); ok || took != 0 {
		t.Fatalf("máquina inalcanzable: ok=%v took=%s", ok, took)
	}
}

// Solo "nadie escucha" se recuerda. Un agente que contesta con error lo sigue
// intentando en cada restauración: saltárselo sería dejar el CSPRNG compartido.
func TestResyncGuestFalloDelAgenteNoSeRecuerda(t *testing.T) {
	var n int
	var mu sync.Mutex
	m, id := managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		http.Error(w, "RNDADDENTROPY: operation not permitted", http.StatusInternalServerError)
	}))
	capturarLog(t)
	for i := 0; i < 3; i++ {
		if _, ok := m.resyncGuest(context.Background(), id, ""); ok {
			t.Fatal("un 500 no es un resync aplicado")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 3 {
		t.Fatalf("el agente recibió %d intentos, quería 3", n)
	}
}

// Freeze apunta si el agente escuchaba: sondea el puerto con el invitado en
// marcha, donde un "nadie" es un RST inmediato y no un plazo tras restaurar.
func TestAgenteEscucha(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cerrado, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	libre := cerrado.Addr().String()
	cerrado.Close()
	m := &Manager{byID: map[string]*api.Machine{
		"si": {ID: "si", Forwards: map[string]string{"8080": ln.Addr().String()}},
		"no": {ID: "no", Forwards: map[string]string{"8080": libre}},
		"??": {ID: "??"}, // sin dirección: ante la duda, sí
	}, socket: map[string]string{}}
	ctx := context.Background()
	if !m.agenteEscucha(ctx, "si") || m.agenteEscucha(ctx, "no") || !m.agenteEscucha(ctx, "??") {
		t.Fatalf("si=%v no=%v ??=%v", m.agenteEscucha(ctx, "si"), m.agenteEscucha(ctx, "no"), m.agenteEscucha(ctx, "??"))
	}
}
