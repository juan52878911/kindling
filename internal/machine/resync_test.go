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
		if _, ok := m.resyncGuest(context.Background(), id); !ok {
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
	for _, code := range []int{http.StatusNotFound, http.StatusBadRequest} {
		m, id := managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no", code)
		}))
		buf := capturarLog(t)
		for i := 0; i < 3; i++ {
			took, ok := m.resyncGuest(context.Background(), id)
			if ok {
				t.Fatalf("%d: se dio por aplicado", code)
			}
			if took > time.Second {
				t.Fatalf("%d: un agente que contesta no debería costar %s", code, took)
			}
		}
		if n := strings.Count(buf.String(), "warning:"); n != 1 {
			t.Fatalf("%d: %d avisos, quería 1:\n%s", code, n, buf)
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
	took, ok := m.resyncGuest(context.Background(), "x")
	if ok || took > resyncReintento+time.Second {
		t.Fatalf("ok=%v took=%s", ok, took)
	}
	// Otra máquina de la misma imagen, con otro puerto: el aviso no se repite.
	m.byID["z"] = &api.Machine{ID: "z", Name: "z", Image: "sin-agente", Forwards: map[string]string{"8080": "127.0.0.1:1"}}
	m.resyncGuest(context.Background(), "z")
	if n := strings.Count(buf.String(), "warning:"); n != 1 {
		t.Fatalf("%d avisos para una imagen, quería 1:\n%s", n, buf)
	}
	if took, ok := m.resyncGuest(context.Background(), "y"); ok || took != 0 {
		t.Fatalf("máquina inalcanzable: ok=%v took=%s", ok, took)
	}
}
