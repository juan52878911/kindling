package daemon

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El healthcheck de una arista depends corre con el usuario del servicio,
// no como root.
func TestPeticionSaludUsaElUsuarioDelServicio(t *testing.T) {
	r := peticionSalud([]string{"pg_isready"}, 1500*time.Millisecond)
	if r.User != api.ExecUserService || r.TimeoutSeconds != 1 || r.MaxOutputBytes != 4<<10 {
		t.Fatalf("petición = %+v", r)
	}
}
