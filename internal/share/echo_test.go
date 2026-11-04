package share

import (
	"container/list"
	"fmt"
	"testing"
	"time"
)

func watcherMudo() *watcher {
	return &watcher{be: mudo{}, entries: map[string]*watchEntry{}, lru: list.New(),
		echo: map[string]time.Time{}, pend: map[string]rawEvent{}}
}

// Las marcas de eco vencidas se sueltan aunque flush no corra nunca (el
// invitado escribe rutas que el host no vigila, así que no llega ningún aviso).
func TestEchoSePurgaSinFlush(t *testing.T) {
	viejo := guestEcho
	guestEcho = 5 * time.Millisecond
	defer func() { guestEcho = viejo }()

	w := watcherMudo()
	for i := 0; i < 500; i++ {
		w.touched(fmt.Sprintf("f%d", i))
	}
	time.Sleep(3 * guestEcho)
	w.touched("ultima")
	if n := len(w.echo); n != 1 {
		t.Fatalf("echo con %d marcas tras vencer las demás; quería 1", n)
	}
}

// Dentro de la ventana de eco, el mapa no pasa del tope.
func TestEchoAcotado(t *testing.T) {
	viejo := maxEcho
	maxEcho = 64
	defer func() { maxEcho = viejo }()

	w := watcherMudo()
	for i := 0; i < 1000; i++ {
		w.touched(fmt.Sprintf("f%d", i))
	}
	if n := len(w.echo); n > maxEcho {
		t.Fatalf("echo con %d marcas; tope %d", n, maxEcho)
	}
	// Una ruta ya apuntada se refresca aunque el mapa esté lleno.
	w.touched("f0")
	if _, ok := w.echo["f0"]; !ok {
		t.Error("f0 debía seguir apuntada")
	}
}
