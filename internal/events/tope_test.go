package events

import (
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// R8: el filtro se aplica al publicar, antes del búfer. Filtrando después, la
// actividad de las máquinas de b llenaba el búfer de a, que perdía su propio
// evento (y solo recibía un aviso de descartes).
func TestFiltroAntesDelBufer(t *testing.T) {
	b := New()
	b.SetDueño(func(ev api.Event) string { return ev.Owner })
	ch, baja, ok := b.SubscribeFiltrado(0, func(ev api.Event, dueño string) bool { return dueño == "a" })
	if !ok {
		t.Fatal("no suscribió")
	}
	defer baja()
	for range 10 * subBuffer {
		b.Publish(api.Event{Type: api.EvStarted, ID: "bbbb", Owner: "b"})
	}
	b.Publish(api.Event{Type: api.EvStarted, ID: "aaaa", Owner: "a"})
	select {
	case ev := <-ch:
		if ev.ID != "aaaa" || ev.Type == api.EvDropped {
			t.Fatalf("a recibió %+v; quería su evento", ev)
		}
	default:
		t.Fatal("a no recibió nada")
	}
	if n := b.Descartados(); n != 0 {
		t.Fatalf("%d descartes por eventos que a no iba a ver", n)
	}
	// Sin filtro, todo, como siempre.
	todo, baja2, _ := b.SubscribeHasta(0)
	defer baja2()
	b.Publish(api.Event{Type: api.EvStarted, ID: "bbbb", Owner: "b"})
	if ev := <-todo; ev.ID != "bbbb" {
		t.Fatalf("sin filtro: %+v", ev)
	}
}

// SubscribeHasta no pasa del tope, y darse de baja deja sitio.
func TestSubscribeHasta(t *testing.T) {
	b := New()
	_, baja1, ok1 := b.SubscribeHasta(2)
	_, baja2, ok2 := b.SubscribeHasta(2)
	if !ok1 || !ok2 {
		t.Fatal("por debajo del tope tiene que suscribir")
	}
	if _, baja, ok := b.SubscribeHasta(2); ok {
		baja()
		t.Fatal("pasó del tope")
	}
	baja1()
	_, baja3, ok := b.SubscribeHasta(2)
	if !ok {
		t.Fatal("tras una baja tiene que haber sitio")
	}
	baja2()
	baja3()
	if _, baja, ok := b.SubscribeHasta(0); !ok {
		t.Fatal("0 es sin tope")
	} else {
		baja()
	}
}
