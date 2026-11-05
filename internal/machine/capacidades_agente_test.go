package machine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Las promesas que el daemon le pide al agente del invitado, además de la del
// entorno (TestEntornoConAgenteViejoFalla): el comando de la máquina (cap
// cmd) y esperar a los volúmenes (cap volume-wait). Un agente que no las
// cumple deja corriendo otra cosa que lo pedido, y la máquina tiene que fallar
// en vez de parecer sana.

// maquinaConAgente registra la máquina id con un agente falso que anuncia
// caps en su /healthz y un Firecracker falso para el MMDS. Devuelve cuántas
// veces se le preguntó al agente.
func maquinaConAgente(t *testing.T, m *Manager, id string, caps []string) *atomic.Int32 {
	t.Helper()
	m.bus = events.New()
	f := nuevoFcFalso(t)
	var vistas atomic.Int32
	agente := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vistas.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.GuestHealth{Status: "ok", Agent: "kling-guest", Version: "0.20.0", Caps: caps})
	}))
	t.Cleanup(agente.Close)
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.Forwards = map[string]string{"8080": strings.TrimPrefix(agente.URL, "http://")}
	m.socket[id] = f.Sock
	m.mu.Unlock()
	return &vistas
}

// esperarFallida espera a que la máquina id quede failed y devuelve su causa.
func esperarFallida(t *testing.T, m *Manager, id string) string {
	t.Helper()
	plazo := time.Now().Add(5 * time.Second)
	for {
		m.mu.RLock()
		st, causa := m.byID[id].State, m.byID[id].LastErr
		m.mu.RUnlock()
		if st == api.StateFailed {
			return causa
		}
		if time.Now().After(plazo) {
			t.Fatalf("la máquina sigue %s con un agente que no cumple lo pedido", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// estadoYCausa es el estado de la viva, leído con el candado.
func estadoYCausa(m *Manager, id string) (api.State, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[id].State, m.byID[id].LastErr
}

// Un agente que no lee el comando de la máquina (una imagen de antes de
// RunRequest.Cmd) lanza el de la imagen: la máquina falla.
func TestComandoConAgenteSinCapCmdFalla(t *testing.T) {
	m := newTestManager(t)
	maquinaConAgente(t, m, "abc", []string{api.GuestCapEnv})
	m.mu.Lock()
	m.byID["abc"].Cmd = []string{"serve", "--puerto", "9000"}
	m.mu.Unlock()

	m.retirarEntornoMMDS("abc", "abc", nil)
	if causa := esperarFallida(t, m, "abc"); !strings.Contains(causa, "command") {
		t.Fatalf("falló sin decir por qué: %q", causa)
	}
}

// Con la capacidad anunciada, la misma máquina sigue running.
func TestComandoConAgenteConCapCmdSigue(t *testing.T) {
	m := newTestManager(t)
	maquinaConAgente(t, m, "abc", []string{api.GuestCapEnv, api.GuestCapCmd})
	m.mu.Lock()
	m.byID["abc"].Cmd = []string{"serve", "--puerto", "9000"}
	m.mu.Unlock()

	m.retirarEntornoMMDS("abc", "abc", nil)
	// La goroutine retira el entorno pendiente al acabar: para entonces ya
	// decidió.
	plazo := time.Now().Add(5 * time.Second)
	for {
		m.mu.RLock()
		_, pendiente := m.entornoPendiente["abc"]
		m.mu.RUnlock()
		if !pendiente {
			break
		}
		if time.Now().After(plazo) {
			t.Fatal("la comprobación del agente no terminó")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st, causa := estadoYCausa(m, "abc"); st != api.StateRunning {
		t.Fatalf("estado = %s (%q) con un agente que sí lee el comando", st, causa)
	}
}

// Un agente que no entiende kling.volwait monta los volúmenes y lanza el
// servicio antes de tiempo, y el commit que viene detrás no podría soltarlos:
// la máquina falla.
func TestEsperaVolumenConAgenteSinCapFalla(t *testing.T) {
	m := newTestManager(t)
	maquinaConAgente(t, m, "abc", []string{api.GuestCapEnv, api.GuestCapCmd})

	m.vigilarEsperaVolumen("abc")
	if causa := esperarFallida(t, m, "abc"); !strings.Contains(causa, "volumes") {
		t.Fatalf("falló sin decir por qué: %q", causa)
	}
}

// Con volume-wait anunciado, sigue running.
func TestEsperaVolumenConAgenteConCapSigue(t *testing.T) {
	m := newTestManager(t)
	vistas := maquinaConAgente(t, m, "abc", []string{api.GuestCapVolumeWait})

	m.vigilarEsperaVolumen("abc")
	plazo := time.Now().Add(5 * time.Second)
	for vistas.Load() == 0 {
		if time.Now().After(plazo) {
			t.Fatal("nadie preguntó al agente")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Tras contestar el agente, la decisión es inmediata: un margen holgado.
	time.Sleep(200 * time.Millisecond)
	if st, causa := estadoYCausa(m, "abc"); st != api.StateRunning {
		t.Fatalf("estado = %s (%q) con un agente que sí espera a sus volúmenes", st, causa)
	}
}
