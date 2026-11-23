package machine

import (
	"context"
	"os"
	"testing"
)

// A-10: preparar el almacén es del daemon, no de la petición que llegó
// primero. Si el cliente de ese primer run corta mientras se crea, la
// cancelación no puede quedar fijada como «almacén no disponible» hasta
// reiniciar: todas las altas pasarían a copia completa.
func TestPrepararAlmacenNoFijaLaCancelacionDelCliente(t *testing.T) {
	root := t.TempDir()
	f := &almacenFalso{}
	a := nuevoAlmacenFalso(t, root, f)
	// Como exec.CommandContext: con el contexto cancelado, la orden no corre.
	a.crear = func(ctx context.Context, img string, _ int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.creados.Add(1)
		return os.WriteFile(img, nil, 0o600)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.mu.Lock()
	errCortado := a.preparar(ctx, 0)
	a.mu.Unlock()

	a.mu.Lock()
	err := a.preparar(context.Background(), 0)
	a.mu.Unlock()
	if err != nil {
		t.Fatalf("tras un primer intento con el cliente ya ido (%v), el almacén quedó no disponible: %v", errCortado, err)
	}
	if !a.estaListo() {
		t.Fatal("preparar acabó sin error y el almacén no está montado")
	}
}
