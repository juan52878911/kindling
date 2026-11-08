package machine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un nombre de imagen inválido se rechaza al entrar en run, antes de tocar
// nada con él (reservarImagen, la receta, la clave del snapshot por imagen).
// Con imgMu tomado, el código de antes se quedaba esperando en reservarImagen
// con "../machines/x" en la mano.
func TestRunRechazaLaImagenInvalidaAlEntrar(t *testing.T) {
	m := newTestManager(t)
	m.imgMu.Lock()
	defer m.imgMu.Unlock()
	hecho := make(chan error, 1)
	go func() {
		_, err := m.Run(context.Background(), api.RunRequest{Image: "../machines/x"})
		hecho <- err
	}()
	select {
	case err := <-hecho:
		if !errors.Is(err, ErrInvalidName) {
			t.Fatalf("Run = %v; quería ErrInvalidName", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run usó la imagen sin validarla (se quedó en reservarImagen)")
	}
}
