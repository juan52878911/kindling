package guest

import (
	"testing"

	share "github.com/juan52878911/kindling/internal/shareproto"
)

// Un fallo del analizador ante una petición rara no puede tumbar el proceso:
// servido por virtio-fs (fuse_host.go), el proceso es el VMM de kling-hv. Se
// simula con un analizador que se fía del tamaño de arg y lee fuera.
func TestFUSEPanicDaEIO(t *testing.T) {
	m := montar(t, carpeta(t), false)
	m.fs.onReq = func(req fuseReq) {
		if req.opcode == opGetattr {
			_ = req.arg[64] // arg corto: index out of range
		}
	}
	for i := 0; i < 3; i++ {
		if e, _ := m.dev.pedir(t, opGetattr, rootID, make([]byte, 16)); e != share.EIO {
			t.Fatalf("getattr %d with a panicking parser: errno %d, want EIO", i, e)
		}
	}
	if n := m.fs.panics.Load(); n != 3 {
		t.Fatalf("panics counted: %d, want 3", n)
	}
	// El proceso sigue vivo y el resto de peticiones se atienden.
	if e, _, a := m.lookup(t, rootID, "hello.txt"); e != 0 || a.size != 20 {
		t.Fatalf("lookup after the panics: errno %d attr %+v", e, a)
	}
}
