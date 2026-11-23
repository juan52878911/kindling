package machine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
)

// R6 A2: con el contexto del cliente ya cancelado, waitSocket no debe seguir
// sondeando hasta su plazo de 5 s: en runFromUna y thaw se llama con un hueco
// de la puerta de arranques tomado.
func TestR6WaitSocketCedeAlCancelar(t *testing.T) {
	m := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sock := filepath.Join(t.TempDir(), "no-existe.sock")
	t0 := time.Now()
	err := m.waitSocket(ctx, fc.New(sock), "ab000000000000ff", os.Getpid()) // pid vivo: el VMM "sigue arrancando"
	d := time.Since(t0)
	if err == nil {
		t.Fatal("sin socket no puede darse por listo")
	}
	if d > 500*time.Millisecond {
		t.Fatalf("waitSocket siguió %s con el contexto cancelado (hueco de la puerta retenido): %v", d.Round(time.Millisecond), err)
	}
}
