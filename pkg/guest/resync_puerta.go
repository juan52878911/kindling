package guest

import (
	"net/http"
	"sync"
)

// puertaT retiene las rutas que actúan sobre el invitado (exec, ficheros,
// procesos, sesiones, ganchos, volúmenes) mientras /resync termina lo que
// contestó sin esperar: machine-id, fichero de generación, exec y procesos
// heredados del dorado. Sin ella, un exec que llegara justo tras la respuesta
// podría ser cortado por cortarExecs, o ver el machine-id viejo.
//
// Cerrar y Abrir no se anidan: resyncMu serializa los resync, y quien cierra
// es quien abre (siempre con defer, o el invitado se quedaría sin exec).
type puertaT struct {
	mu sync.Mutex
	ch chan struct{} // nil = abierta
}

// Cerrar cierra la puerta. Si ya estaba cerrada no hace nada.
func (p *puertaT) Cerrar() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch == nil {
		p.ch = make(chan struct{})
	}
}

// Abrir la abre y despierta a quien espera. Abrir una abierta no hace nada.
func (p *puertaT) Abrir() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch != nil {
		close(p.ch)
		p.ch = nil
	}
}

// Cerrada dice si hay un resync terminando su segundo tiempo.
func (p *puertaT) Cerrada() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ch != nil
}

// espera devuelve un canal que se cierra cuando la puerta está abierta
// (cerrado ya si lo está).
func (p *puertaT) espera() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ch == nil {
		c := make(chan struct{})
		close(c)
		return c
	}
	return p.ch
}

// puertaResync es la puerta del agente.
var puertaResync puertaT

// trasResync envuelve h para que espere a que la puerta esté abierta. Si el
// cliente se va mientras tanto, 503 como enArranque.
func trasResync(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-puertaResync.espera():
		case <-r.Context().Done():
			http.Error(w, "the guest is finishing a resync", http.StatusServiceUnavailable)
			return
		}
		h(w, r)
	}
}
