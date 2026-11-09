package share

import (
	"sync"
	"time"
)

// pollInterval es cada cuánto repasa lo vigilado una sesión que vigila por
// sondeo.
var pollInterval = 2 * time.Second

// pollBackend vigila por sondeo: no gasta nada del sistema y, cada
// pollInterval, pide al watcher que lo repase todo, lo mismo que tras perder
// eventos (resyncLocked compara cada ruta con su foto o su huella). Lo usa
// Linux cuando se acaba el presupuesto de instancias de inotify: un cambio del
// host tarda hasta pollInterval en llegar, pero los vigilantes del invitado
// siguen disparando.
type pollBackend struct {
	stop chan struct{}
	once sync.Once
}

func newPollBackend(emit func(rawEvent)) *pollBackend {
	b := &pollBackend{stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-b.stop:
				return
			case <-t.C:
				emit(rawEvent{overflow: true})
			}
		}
	}()
	return b
}

// add no hace nada: el watcher ya acota las rutas (maxWatchesPerSession) y el
// repaso recorre las suyas.
func (b *pollBackend) add(string, bool) error { return nil }
func (b *pollBackend) remove(string)          {}
func (b *pollBackend) close()                 { b.once.Do(func() { close(b.stop) }) }
