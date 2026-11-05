//go:build darwin && arm64

package hvvm

import (
	"strings"
	"testing"
)

// Sin VM no ha terminado nada: FinMotivo vacío (el servidor no registra
// nada). Un error de pausa siempre lleva texto y dice qué operación era.
func TestFinMotivoYErrorDePausa(t *testing.T) {
	if m := FinMotivo(); m != "" {
		t.Fatalf("FinMotivo without a VM = %q, want empty", m)
	}
	err := errPausa("pausing")
	if err == nil || !strings.HasPrefix(err.Error(), "pausing") || len(err.Error()) <= len("pausing") {
		t.Fatalf("errPausa = %v, want a message that starts with the operation", err)
	}
}
