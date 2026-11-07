package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/durable"
)

// discoQueFalla hace que las n primeras escrituras de state.json fallen.
func discoQueFalla(t *testing.T, n int32) *atomic.Int32 {
	t.Helper()
	var llamadas atomic.Int32
	prev := escribirEstado
	escribirEstado = func(ruta string, datos []byte, perm os.FileMode) error {
		if llamadas.Add(1) <= n {
			return errors.New("disco lleno")
		}
		return durable.Escribir(ruta, datos, perm)
	}
	t.Cleanup(func() { escribirEstado = prev })
	return &llamadas
}

func escritoGen(m *Manager) uint64 {
	m.escrituraMu.Lock()
	defer m.escrituraMu.Unlock()
	return m.escritoGen
}

// Una escritura de state.json que falla no puede perder la foto: se repone y
// se reintenta sola hasta llegar a disco. Antes se ponía pending a nil antes
// de escribir y el fallo solo quedaba en el log.
func TestEstadoQueNoSeEscribeSeReintenta(t *testing.T) {
	m := newTestManager(t)
	discoQueFalla(t, 2)
	m.addForTest("ab00000000000001")
	m.mu.RLock()
	gen := m.persist()
	m.mu.RUnlock()

	plazo := time.Now().Add(5 * time.Second)
	for escritoGen(m) < gen {
		if time.Now().After(plazo) {
			t.Fatal("la foto no llegó a disco tras los fallos")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := readState(t, m); len(got) != 1 || got[0].ID != "ab00000000000001" {
		t.Fatalf("state.json = %+v", got)
	}
}

// persistirYa dice si la foto no llegó a disco: quien la pide va a lanzar un
// VMM y no debe hacerlo sin su máquina escrita.
func TestPersistirYaDevuelveElFalloDeEscritura(t *testing.T) {
	m := newTestManager(t)
	discoQueFalla(t, 1000)
	m.addForTest("ab00000000000002")
	if err := m.persistirYa(); err == nil {
		t.Fatal("persistirYa no devolvió error con el disco fallando")
	}
}

// Start no lanza el VMM si la máquina no quedó en disco antes.
func TestStartNoLanzaElVMMSinSuEstadoEnDisco(t *testing.T) {
	m := newTestManager(t)
	m.fcBin = filepath.Join(t.TempDir(), "no-hay-vmm")
	mc, _ := paradaParaStart(t, m, "5a5a0000000000e1")
	discoQueFalla(t, 1000)
	_, err := m.Start(context.Background(), mc.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "state not saved") {
		t.Fatalf("Start = %v; quería que no arrancase sin su estado en disco", err)
	}
}
