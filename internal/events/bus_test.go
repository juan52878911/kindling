package events

import (
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Publish NO puede bloquear. El bus lo usan el arranque, el congelado y el
// segador: si un suscriptor lento —un `kling events` cuyo cliente dejo de leer—
// pudiera pararlo, pararia el ciclo de vida de las microVM.
func TestUnSuscriptorQueNoLeeNoBloqueaAlQuePublica(t *testing.T) {
	b := New()
	_, cancelar := b.Subscribe() // nadie leera de ese canal
	defer cancelar()

	hecho := make(chan struct{})
	go func() {
		// Muchos mas eventos que el buffer: si Publish bloqueara, no acabaria.
		for i := 0; i < 10_000; i++ {
			b.Publish(api.Event{Type: api.EvCreated, ID: "x"})
		}
		close(hecho)
	}()

	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish se bloqueó con un suscriptor que no lee")
	}
}

func TestCadaSuscriptorRecibeLoSuyo(t *testing.T) {
	b := New()
	c1, cancelar1 := b.Subscribe()
	c2, cancelar2 := b.Subscribe()
	defer cancelar1()
	defer cancelar2()

	b.Publish(api.Event{Type: api.EvCreated, ID: "uno"})

	for i, ch := range []<-chan api.Event{c1, c2} {
		select {
		case ev := <-ch:
			if ev.ID != "uno" {
				t.Errorf("suscriptor %d recibió %q", i+1, ev.ID)
			}
		case <-time.After(time.Second):
			t.Errorf("suscriptor %d no recibió nada", i+1)
		}
	}
}

// Cancelar cierra el canal: es como el lector sabe que se acabó. Y cancelar dos
// veces no puede entrar en pánico por cerrar un canal ya cerrado — pasa cuando
// un defer y un cierre explícito coinciden.
func TestCancelarCierraYEsIdempotente(t *testing.T) {
	b := New()
	ch, cancelar := b.Subscribe()
	cancelar()

	select {
	case _, abierto := <-ch:
		if abierto {
			t.Error("el canal seguía abierto tras cancelar")
		}
	case <-time.After(time.Second):
		t.Fatal("el canal no se cerró")
	}
	cancelar() // no debe entrar en pánico
}

// Publicar mientras alguien entra y sale no puede dar carreras: el segador
// publica desde su bucle mientras un cliente se suscribe y se va.
func TestPublicarYSuscribirseALaVezNoDaCarreras(t *testing.T) {
	b := New()
	var wg sync.WaitGroup
	fin := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-fin:
				return
			default:
				b.Publish(api.Event{Type: api.EvStopped, ID: "y"})
			}
		}
	}()
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancelar := b.Subscribe()
			select {
			case <-ch:
			case <-time.After(50 * time.Millisecond):
			}
			cancelar()
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(fin)
	wg.Wait()
}

// Un suscriptor lento pierde eventos, pero no a ciegas: el bus los cuenta y,
// cuando vuelve a haber sitio, le avisa con un EvDropped que dice cuántos
// antes de entregarle el siguiente.
func TestLosDescartesSeCuentanYSeAvisan(t *testing.T) {
	b := New()
	ch, cancelar := b.Subscribe()
	defer cancelar()

	const extra = 10
	for i := 0; i < subBuffer+extra; i++ {
		b.Publish(api.Event{Type: api.EvCreated, ID: "x"})
	}
	if got := b.Descartados(); got != extra {
		t.Fatalf("Descartados = %d, quería %d", got, extra)
	}
	for i := 0; i < subBuffer; i++ {
		<-ch // se pone al día
	}
	b.Publish(api.Event{Type: api.EvStarted, ID: "y"})
	aviso := <-ch
	if aviso.Type != api.EvDropped || aviso.Dropped != extra {
		t.Fatalf("primero el aviso de %d perdidos; llegó %+v", extra, aviso)
	}
	if ev := <-ch; ev.Type != api.EvStarted || ev.ID != "y" {
		t.Fatalf("después del aviso, el evento: %+v", ev)
	}
	// Al día otra vez: ni avisos de más ni descartes nuevos.
	b.Publish(api.Event{Type: api.EvStopped, ID: "z"})
	if ev := <-ch; ev.Type != api.EvStopped {
		t.Fatalf("aviso repetido: %+v", ev)
	}
	if got := b.Descartados(); got != extra {
		t.Fatalf("Descartados cambió sin descartar: %d", got)
	}
}
