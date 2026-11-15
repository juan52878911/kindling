package events

import "testing"

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
