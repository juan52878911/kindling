package machine

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un arranque en frío que ya eligió su imagen y aún no está en byID cuenta
// como uso: sustituirla (o el kernel) por debajo lo haría arrancar con otro
// contenido.
func TestSustituirConArranqueEnVuelo(t *testing.T) {
	m := newTestManager(t)
	if err := m.ImageReplaceable("img"); err != nil {
		t.Fatalf("sin nadie usándola: %v", err)
	}
	soltar := m.reservarImagen("img")
	if err := m.ImageReplaceable("img"); err == nil {
		t.Fatal("una imagen con un arranque en vuelo no se puede sustituir")
	}
	if err := m.KernelReplaceable(); err == nil {
		t.Fatal("el kernel con un arranque en vuelo no se puede sustituir")
	}
	if err := m.ImageReplaceable("otra"); err != nil {
		t.Fatalf("otra imagen no tiene nada que ver: %v", err)
	}
	soltar()
	if err := m.ImageReplaceable("img"); err != nil {
		t.Fatalf("soltada: %v", err)
	}
	if err := m.KernelReplaceable(); err != nil {
		t.Fatalf("soltada, el kernel: %v", err)
	}
}

// Mientras se sustituye (comprobar y renombrar), un run espera antes de
// mirar la imagen: si no, la comprobación y el rename no servirían de nada.
func TestRunEsperaALaSustitucion(t *testing.T) {
	m := newTestManager(t)
	dentro, salir := make(chan struct{}), make(chan struct{})
	go m.ConImagenesQuietas(func() error {
		close(dentro)
		<-salir
		return nil
	})
	<-dentro
	hecho := make(chan error, 1)
	go func() {
		_, err := m.Run(context.Background(), api.RunRequest{Image: "no-existe"})
		hecho <- err
	}()
	select {
	case err := <-hecho:
		t.Fatalf("run no esperó a la sustitución en curso: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(salir)
	select {
	case err := <-hecho:
		if err == nil {
			t.Fatal("una imagen que no existe no arranca")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run se quedó esperando tras la sustitución")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.arrancando) != 0 || m.arrancandoTotal != 0 {
		t.Fatalf("run dejó su reserva: %v %d", m.arrancando, m.arrancandoTotal)
	}
}
