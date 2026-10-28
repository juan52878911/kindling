package machine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// sujetarLimpieza detiene la limpieza de m hasta que se llame a la función
// devuelta (idempotente). Solo afecta a ese Manager.
func sujetarLimpieza(t *testing.T, m *Manager) (soltar func()) {
	t.Helper()
	puerta := make(chan struct{})
	var una sync.Once
	prev := antesDeLimpiar
	antesDeLimpiar = func(o *Manager, id string) {
		if o == m {
			<-puerta
		}
	}
	soltar = func() { una.Do(func() { close(puerta) }) }
	t.Cleanup(func() { soltar(); m.EsperarBorrados(5 * time.Second); antesDeLimpiar = prev })
	return soltar
}

func maquinaConDir(t *testing.T, m *Manager, id, nombre string) {
	t.Helper()
	m.addForTest(id)
	m.mu.Lock()
	m.byID[id].Name = nombre
	m.mu.Unlock()
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir(id), "overlay"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Mientras la limpieza sigue, la máquina se ve como "removing" bajo un nombre
// que nadie pide, el suyo queda libre y, al acabar, no queda rastro.
func TestRemoveContestaAntesDeLimpiarYLiberaElNombre(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	soltar := sujetarLimpieza(t, m)
	maquinaConDir(t, m, "aa00000000000001", "web")

	if err := m.Remove("web"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("web"); ok {
		t.Error("el nombre sigue resolviendo a la máquina que se borra")
	}
	tumba, ok := m.Get("aa00000000000001")
	if !ok || tumba.Transition != api.TransitionRemoving || !esTumba(tumba) {
		t.Fatalf("esperaba una tumba en removing, hay %+v (%v)", tumba, ok)
	}
	liberar, err := m.reservarNombre("web")
	if err != nil {
		t.Fatalf("el nombre no quedó libre: %v", err)
	}
	liberar()
	if _, err := os.Stat(m.dir("aa00000000000001")); err != nil {
		t.Errorf("el directorio se borró antes de tiempo: %v", err)
	}
	// La tumba ya está en disco, no a merced del debounce.
	var enDisco bool
	for _, mc := range readState(t, m) {
		enDisco = enDisco || (mc.ID == "aa00000000000001" && mc.Name == nombreTumba(mc.ID))
	}
	if !enDisco {
		t.Error("la tumba no estaba escrita en state.json al contestar")
	}

	soltar()
	if !m.EsperarBorrados(5 * time.Second) {
		t.Fatal("la limpieza no terminó")
	}
	if _, ok := m.Get("aa00000000000001"); ok {
		t.Error("la tumba sigue tras limpiar")
	}
	if _, err := os.Stat(m.dir("aa00000000000001")); !os.IsNotExist(err) {
		t.Errorf("el directorio sigue: %v", err)
	}
}

// Borrada, no quedan ni su cuenta de freezes fallidos ni su marca de "sin
// agente": eran entradas por ID que ya no volverá.
func TestRemovePurgaLoQueQuedabaPorID(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "aa00000000000003"
	maquinaConDir(t, m, id, "web")
	m.noteFreezeFailure(id)
	m.resyncSinAgente.Store(claveThaw(id), time.Time{})

	if err := m.Remove("web"); err != nil {
		t.Fatal(err)
	}
	if !m.EsperarBorrados(5 * time.Second) {
		t.Fatal("la limpieza no terminó")
	}
	m.mu.RLock()
	_, cuenta := m.freezeFails[id]
	m.mu.RUnlock()
	if cuenta {
		t.Error("quedó su cuenta de freezes fallidos")
	}
	if _, ok := m.resyncSinAgente.Load(claveThaw(id)); ok {
		t.Error("quedó su marca de sin agente")
	}
}

// rm y un run con el mismo nombre en carrera: ninguno choca con ErrNameTaken.
func TestRemoveYReservaDelMismoNombreEnCarrera(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("bb%014d", i)
		maquinaConDir(t, m, id, "dev")
		var wg sync.WaitGroup
		var errRm, errRes error
		wg.Add(2)
		go func() { defer wg.Done(); errRm = m.Remove(id) }()
		go func() {
			defer wg.Done()
			// Mientras la vieja siga llamándose "dev", el run falla con razón;
			// en cuanto rm contesta, el nombre tiene que estar libre.
			for {
				var liberar func()
				if liberar, errRes = m.reservarNombre("dev"); errRes == nil {
					liberar()
					return
				}
				time.Sleep(50 * time.Microsecond)
			}
		}()
		wg.Wait()
		if errRm != nil {
			t.Fatal(errRm)
		}
		// Contestado el rm, el nombre ya no puede estar ocupado.
		liberar, err := m.reservarNombre("dev")
		if err != nil {
			t.Fatalf("vuelta %d: nombre ocupado tras rm: %v", i, err)
		}
		liberar()
	}
	if !m.EsperarBorrados(10 * time.Second) {
		t.Fatal("las limpiezas no terminaron")
	}
	if n := m.Count(); n != 0 {
		t.Errorf("quedaron %d máquinas", n)
	}
}

// Un reinicio a mitad de la limpieza: el estado en disco trae la tumba y el
// directorio, y el arranque siguiente acaba de borrar.
func TestRecuperarTumbasTrasReinicio(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	soltar := sujetarLimpieza(t, m)
	maquinaConDir(t, m, "cc00000000000001", "api")
	maquinaConDir(t, m, "cc00000000000002", "~rm-cc00000000000009") // nombre de usuario, no tumba
	if err := m.Remove("cc00000000000001"); err != nil {
		t.Fatal(err)
	}

	// "Reinicio": otro Manager lee el mismo directorio de estado.
	m2 := newTestManager(t)
	m2.root = m.root
	m2.bus = events.New()
	m2.load()
	if _, ok := m2.byID["cc00000000000001"]; !ok {
		t.Fatal("la tumba no llegó al estado")
	}
	m2.recuperarTumbas()
	if !m2.EsperarBorrados(5 * time.Second) {
		t.Fatal("la recuperación no terminó")
	}
	if _, ok := m2.byID["cc00000000000001"]; ok {
		t.Error("la tumba sobrevivió a la recuperación")
	}
	if _, err := os.Stat(m.dir("cc00000000000001")); !os.IsNotExist(err) {
		t.Errorf("el directorio sigue tras la recuperación: %v", err)
	}
	if _, ok := m2.byID["cc00000000000002"]; !ok {
		t.Error("una máquina con nombre ~rm-... del usuario fue borrada")
	}
	soltar()
}
