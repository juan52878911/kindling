//go:build darwin

package machine

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// En macOS el daemon llega a otra máquina por su reenvío, y solo si es del
// rango reservado: un reenvío que falta o que apunta a otro sitio no da
// dirección (y el broker no marca nada).
func TestVZDireccionCopiaSoloReenvioReservado(t *testing.T) {
	mc := &api.Machine{ID: idCopia, Name: "copia"}
	if _, err := direccionCopiaLocked(mc, 5432); err == nil || !strings.Contains(err.Error(), "no forward") {
		t.Fatalf("sin reenvío: %v", err)
	}
	for _, mal := range []string{"127.0.0.1:5432", "192.168.1.10:29432", "localhost:29432", "[::1]:29432x"} {
		mc.Forwards = map[string]string{"5432": mal}
		if addr, err := direccionCopiaLocked(mc, 5432); err == nil {
			t.Errorf("%s: dio %q", mal, addr)
		}
	}
	mc.Forwards = map[string]string{"5432": "127.0.0.1:29432"}
	if addr, err := direccionCopiaLocked(mc, 5432); err != nil || addr != "127.0.0.1:29432" {
		t.Fatalf("reenvío bueno: %q %v", addr, err)
	}
}
