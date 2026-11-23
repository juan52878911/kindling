package machine

import (
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// R3-3: volumenesTrasReconcile (el arreglo de R6) devuelve los volúmenes solo
// a la plantilla readoptada tras revertir un commit cortado, con volúmenes,
// sin VolumeWait y no pausada; si no puede, la máquina queda failed. Falla si
// reconcile deja de llamarla o si se la llama de más.
func TestReconcileVolumenesSoloParaElCommitRevertido(t *testing.T) {
	casos := []struct {
		nombre     string
		estado     api.State
		volumenes  bool
		volumeWait bool
		marca      bool
		falla      bool
		llamadas   int32
		quiere     api.State
	}{
		{nombre: "revertido", estado: api.StateRunning, volumenes: true, marca: true, llamadas: 1, quiere: api.StateRunning},
		{nombre: "revertido-falla", estado: api.StateRunning, volumenes: true, marca: true, falla: true, llamadas: 1, quiere: api.StateFailed},
		{nombre: "sin-volumenes", estado: api.StateRunning, marca: true, llamadas: 0, quiere: api.StateRunning},
		{nombre: "volume-wait", estado: api.StateRunning, volumenes: true, volumeWait: true, marca: true, llamadas: 0, quiere: api.StateRunning},
		{nombre: "pausada", estado: api.StatePaused, volumenes: true, marca: true, llamadas: 0, quiere: api.StatePaused},
		{nombre: "sin-commit", estado: api.StateRunning, volumenes: true, llamadas: 0},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			m := newTestManager(t)
			id := "a7a7000000000f16"
			_, _ = vmmEnPausaTrasReinicio(t, m, id, c.estado)
			m.mu.Lock()
			if c.volumenes {
				m.byID[id].Volumes = []api.VolumeAttachment{{}}
			}
			m.byID[id].VolumeWait = c.volumeWait
			m.mu.Unlock()
			if c.marca {
				if err := marcarCommitEnCurso(m.dir(id), "dorado", 0); err != nil {
					t.Fatal(err)
				}
			}
			var llamadas atomic.Int32
			old := devolverVolumenes
			devolverVolumenes = func(_ *Manager, mc *api.Machine) error {
				llamadas.Add(1)
				if mc.ID != id {
					t.Errorf("devolverVolumenes con %s, quería %s", mc.ID, id)
				}
				if c.falla {
					return errors.New("el invitado no monta")
				}
				return nil
			}
			t.Cleanup(func() { devolverVolumenes = old })

			m.reconcile()

			if got := llamadas.Load(); got != c.llamadas {
				t.Fatalf("acquireVolumes llamado %d veces, quería %d", got, c.llamadas)
			}
			if c.quiere != "" {
				if viva := vivaDe(t, m, id); viva.State != c.quiere {
					t.Fatalf("quedó %s (%q), quería %s", viva.State, viva.LastErr, c.quiere)
				}
			}
		})
	}
}

// R3-5: un VMM colgado (no contesta) tras un commit cortado no es uno que
// vuelca: no se le espera plazoVolcado (90 s con 4 GiB) antes de que el daemon
// escuche, sino como mucho reintentoCommitSinRespuesta.
func TestRevertirCommitCortadoConVMMColgadoNoRetrasaElArranque(t *testing.T) {
	m := newTestManager(t)
	id := "a7a7000000000f26"
	falso := nuevoFcFalso(t)
	m.pruebasSocketVMM = func(string) string { return falso.Sock }
	soltar := make(chan struct{})
	t.Cleanup(func() { close(soltar) })
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/drives/overlay" {
			<-soltar
		}
	})
	inicio := time.Now()
	ok := m.revertirCommitCortado(id, m.dir(id), 1, "Paused", false, 4096)
	if ok {
		t.Fatal("un VMM que no contesta no puede darse por revertido")
	}
	if d := time.Since(inicio); d > 12*time.Second {
		t.Fatalf("revertirCommitCortado esperó %s a un VMM colgado; el tope es 10 s", d.Round(time.Millisecond))
	}
}

// Un VMM que CONTESTA «ocupado» sí está volcando: se le espera y, cuando
// acaba, recupera su disco.
func TestRevertirCommitCortadoEsperaAlVMMQueContestaOcupado(t *testing.T) {
	m := newTestManager(t)
	id := "a7a7000000000f27"
	falso := nuevoFcFalso(t)
	falso.ponerEstado("Paused")
	m.pruebasSocketVMM = func(string) string { return falso.Sock }
	falso.fallar(http.MethodPatch, "/drives/overlay", http.StatusBadRequest, "Resource busy: snapshot in progress")
	time.AfterFunc(1500*time.Millisecond, func() { falso.dejarDeFallar(http.MethodPatch, "/drives/overlay") })
	if !m.revertirCommitCortado(id, m.dir(id), 1, "Paused", false, 4096) {
		t.Fatal("el VMM ocupado acabó y no se le devolvió el disco")
	}
	if n := len(falso.llamadasA(http.MethodPatch, "/drives/overlay")); n < 2 {
		t.Fatalf("PATCH /drives/overlay = %d llamadas: no se reintentó", n)
	}
	if propio := filepath.Join(m.dir(id), "overlay.ext4"); indiceDe(falso.todas(), 0, http.MethodPatch, "/drives/overlay", propio) < 0 {
		t.Fatal("no se pidió el overlay propio")
	}
}
