package machine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// F7: la entrega de credenciales del thaw no se corta porque quien lo pidió
// se fue, y si la primera falla se intenta otra vez entera.
func TestEntregarSinPeticion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var vistos []error
	n := 0
	err := entregarSinPeticion(ctx, time.Second,
		func(c context.Context) error {
			n++
			vistos = append(vistos, c.Err())
			return errors.New("MMDS busy")
		},
		func(c context.Context) error {
			n++
			vistos = append(vistos, c.Err())
			if _, ok := c.Deadline(); !ok {
				return errors.New("sin plazo")
			}
			return nil
		})
	if err != nil || n != 2 {
		t.Fatalf("err %v tras %d intentos; quería el segundo bien", err, n)
	}
	for i, e := range vistos {
		if e != nil {
			t.Fatalf("el intento %d llegó con el ctx de la petición cancelado: %v", i+1, e)
		}
	}
	if err := entregarSinPeticion(ctx, time.Second, func(context.Context) error { return errors.New("x") }); err == nil {
		t.Fatal("un fallo en todos los intentos se dio por bueno")
	}
}
