//go:build !darwin

package machine

import (
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
)

// FONDO DE RANURAS DE RED DEL DAEMON.
//
// Ver internal/net/ranuras_fc.go: N redes con la base montada, cada una con
// su id y su índice de subred. nuevoIDMaquina entrega el id de una ranura lista
// y asignarRed, su red, sin volcar el host ni reservar nada; montarRed solo
// aplica lo propio de la petición. Sin ranura (fondo apagado, vacío o sin
// root), todo como siempre.
//
// El fondo se arranca al primer id que se pide y vive lo que el daemon: sus
// ranuras en espera las recoge barrerNamespaces del siguiente arranque si el
// daemon muere (llevan la marca de red propia desde que se montan).

// fondoRed es la entrada de un Manager en fondosRed. Es un mapa global y no un
// campo de Manager: el fondo no pide tocar la estructura.
type fondoRed struct {
	once sync.Once
	f    atomic.Pointer[knet.Fondo] // nil si el fondo no está activo
}

var fondosRed sync.Map // *Manager → *fondoRed

// ranurasPosibles dice si este daemon puede montar ranuras: con privilegios
// (el tap se crea a nombre del usuario sin privilegios), root y las
// herramientas de red. Sustituible en tests.
var ranurasPosibles = func(m *Manager) bool {
	return m.priv != nil && os.Geteuid() == 0 && knet.Available() == nil
}

// intervaloRecogida: cada cuánto se destruyen las ranuras tomadas y nunca
// montadas (knet.PlazoRanuraTomada).
const intervaloRecogida = 15 * time.Second

func (m *Manager) fondoDeRed() *knet.Fondo {
	v, _ := fondosRed.LoadOrStore(m, &fondoRed{})
	e := v.(*fondoRed)
	e.once.Do(func() {
		tam := knet.TamPool()
		if tam == 0 || !ranurasPosibles(m) {
			return
		}
		f := knet.NuevoFondo(tam, m.montarRanura, m.destruirRanura)
		e.f.Store(f)
		go func() {
			for range time.Tick(intervaloRecogida) {
				f.Recoger()
			}
		}()
		f.Reponer()
		log.Printf("network: slot pool of %d prepared networks started", tam)
	})
	return e.f.Load()
}

// fondoActivo es el fondo si ya está arrancado, sin arrancarlo: lo usan el
// barrido de huérfanos y asignarRed.
func (m *Manager) fondoActivo() *knet.Fondo {
	if v, ok := fondosRed.Load(m); ok {
		return v.(*fondoRed).f.Load()
	}
	return nil
}

// montarRanura crea una red con la base montada, con id e índice ya dados.
func (m *Manager) montarRanura() (*knet.Ranura, error) {
	f := m.fondoDeRed()
	id := m.idLibre(func() string {
		for {
			id := newID()
			if !f.PrefijoEnUso(knet.Corto(id)) {
				return id
			}
		}
	})
	ns := "kl-" + knet.Corto(id)
	f.Marcar(ns) // el barrido de huérfanos no la toca mientras se monta
	n, err := m.asignarRedNueva(id)
	if err != nil {
		f.Desmarcar(ns)
		return nil, err
	}
	// La marca va ANTES de montar, como en montarRed: si el daemon muere a
	// medias, el siguiente arranque barre lo que quedó.
	m.marcarRedPropia(n.NS)
	uid := 0
	if m.priv != nil {
		uid = m.priv.UID
	}
	err = n.MontarRanura(uid)
	// Montada la base, el veth ya ocupa la dirección en el host: la reserva
	// del índice sobra.
	n.SoltarReserva()
	if err != nil {
		m.olvidarRedPropia(n.NS)
		f.Desmarcar(ns)
		return nil, err
	}
	return &knet.Ranura{ID: id, Net: n}, nil
}

func (m *Manager) destruirRanura(r *knet.Ranura) {
	r.Net.DestruirRanura()
	m.olvidarRedPropia(r.Net.NS)
}

// ranuraTomar entrega el id de una ranura lista, si hay.
func (m *Manager) ranuraTomar() (string, bool) {
	f := m.fondoDeRed()
	if f == nil {
		return "", false
	}
	r := f.Tomar()
	if r == nil {
		return "", false
	}
	// El id vuelve a quedar acuñado: pasó mucho en espera y el plazo de
	// acuñados pudo vencer.
	acuñados.Lock()
	acuñados.m[knet.Corto(r.ID)] = time.Now()
	acuñados.Unlock()
	return r.ID, true
}

// ranuraEntregar da la red de la ranura de la máquina id, si lo es.
func (m *Manager) ranuraEntregar(id string) *knet.Net {
	f := m.fondoActivo()
	if f == nil {
		return nil
	}
	return f.Entregar(id)
}

// ranuraPropia: el namespace es de una ranura del fondo (en espera, tomada o
// a medio montar): el barrido de huérfanos no lo toca.
func (m *Manager) ranuraPropia(ns string) bool {
	f := m.fondoActivo()
	if f == nil {
		return false
	}
	return f.Propia(ns)
}

// ranuraPrefijoEnUso: el prefijo es de una ranura del fondo.
func (m *Manager) ranuraPrefijoEnUso(corto string) bool {
	f := m.fondoActivo()
	if f == nil {
		return false
	}
	return f.PrefijoEnUso(corto)
}
