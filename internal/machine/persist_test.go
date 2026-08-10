package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// newTestManager arma un Manager con lo justo para ejercitar la persistencia,
// sin tocar KVM, red ni cgroups.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	m := &Manager{
		root:   root,
		byID:   map[string]*api.Machine{},
		socket: map[string]string{},
		wake:   make(chan struct{}, 1),
		quit:   make(chan struct{}),
	}
	m.persistWG.Add(1)
	go m.persistLoop()
	t.Cleanup(m.Close)
	return m
}

func (m *Manager) addForTest(id string) *api.Machine {
	mc := &api.Machine{ID: id, Name: id, State: api.StateRunning, CreatedAt: time.Now()}
	m.mu.Lock()
	m.byID[id] = mc
	m.persist()
	m.mu.Unlock()
	return mc
}

func readState(t *testing.T, m *Manager) []api.Machine {
	t.Helper()
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		return nil
	}
	var out []api.Machine
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("state.json ilegible: %v", err)
	}
	return out
}

// El caso que importa: persistir mientras otra goroutine muta las máquinas.
//
// Con la foto tomada por PUNTERO, json.Marshal lee los api.Machine fuera del
// lock mientras el mutador les cambia State, PID y los punteros de fecha. Bajo
// -race eso es un fallo inmediato; sin -race, un state.json que describe un
// estado que nunca existió. Con la foto por VALOR no hay nada que compartir.
func TestPersistNoCorreConLasMutaciones(t *testing.T) {
	m := newTestManager(t)
	for i := range 8 {
		m.addForTest(string(rune('a'+i)) + "-id")
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Mutadores: tocan exactamente los campos que toca el ciclo de vida real.
	for i := range 4 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := string(rune('a'+n)) + "-id"
			for {
				select {
				case <-stop:
					return
				default:
				}
				now := time.Now()
				m.mu.Lock()
				if mc := m.byID[id]; mc != nil {
					mc.State = api.StateWarm
					mc.PID = n
					mc.StartedAt = &now
					mc.Labels = api.MergeLabels(mc.Labels, map[string]string{"k": "v"})
					m.persist()
					mc.State = api.StateRunning
					mc.StartedAt = nil
					m.persist()
				}
				m.mu.Unlock()
			}
		}(i)
	}

	// Y persistencia a mano en paralelo, para no depender solo del debounce.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.mu.RLock()
			m.persist()
			m.mu.RUnlock()
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// Una ráfaga de transiciones debe acabar en UNA escritura, no en una por
// transición: ese era el objetivo de sacar el fsync del lock.
func TestPersistAgrupaLasRafagas(t *testing.T) {
	m := newTestManager(t)
	for i := range 20 {
		m.addForTest(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}
	// Antes del debounce no debería haber nada en disco todavía.
	if _, err := os.Stat(m.statePath()); err == nil {
		t.Log("aviso: ya había fichero; el debounce puede haber disparado, no es un fallo")
	}
	time.Sleep(persistDebounce + 150*time.Millisecond)
	if got := len(readState(t, m)); got != 20 {
		t.Errorf("tras la ráfaga el estado tiene %d máquinas, want 20", got)
	}
}

// El tope máximo: con avisos constantes el debounce se reinicia siempre, y sin
// tope la escritura no llegaría nunca. Debe llegar dentro de persistMaxDelay.
func TestPersistNoSePosponeParaSiempre(t *testing.T) {
	m := newTestManager(t)
	m.addForTest("uno")

	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.mu.RLock()
			m.persist()
			m.mu.RUnlock()
			time.Sleep(persistDebounce / 3) // más rápido que el debounce
		}
	}()
	defer close(stop)

	deadline := time.Now().Add(persistMaxDelay + 300*time.Millisecond)
	for time.Now().Before(deadline) {
		if len(readState(t, m)) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("el estado nunca se escribió: el debounce se pospone sin tope")
}

// Close tiene que volcar lo pendiente, no solo parar el bucle. Es lo que sostiene
// que el apagado del daemon no pierda la última transición.
func TestCloseVuelcaLoPendiente(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.byID["ultima"] = &api.Machine{ID: "ultima", Name: "ultima", State: api.StateWarm}
	m.persist()
	m.mu.Unlock()

	// Sin esperar al debounce: Close debe escribirlo igual.
	m.Close()

	got := readState(t, m)
	if len(got) != 1 || got[0].ID != "ultima" {
		t.Fatalf("Close no volcó el estado pendiente: %+v", got)
	}
}

// Close se llama desde el apagado y puede llegar dos veces (defer + señal).
func TestCloseEsIdempotente(t *testing.T) {
	m := newTestManager(t)
	m.addForTest("x")
	m.Close()
	m.Close() // no debe entrar en pánico por cerrar un canal ya cerrado
}

// El fichero se sustituye de forma atómica: nunca debe quedar un .tmp suelto ni
// un state.json a medias.
func TestPersistEsAtomico(t *testing.T) {
	m := newTestManager(t)
	m.addForTest("a")
	m.Close()

	if _, err := os.Stat(m.statePath() + ".tmp"); err == nil {
		t.Error("quedó un .tmp sin renombrar")
	}
	entries, _ := os.ReadDir(filepath.Dir(m.statePath()))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("residuo temporal: %s", e.Name())
		}
	}
}
