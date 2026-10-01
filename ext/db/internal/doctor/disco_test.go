package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Una copia cuyo invitado vio errores de disco (el almacén de copia al
// escribir lleno) lo dice como CRITICAL, con la línea del invitado.
func TestCopiaConErroresDeDisco(t *testing.T) {
	k, files := limpio()
	at := time.Now().Add(-time.Minute)
	k.machine.DiskErrors, k.machine.DiskErrorAt = 3, &at
	k.machine.DiskError = "[ 78.2] I/O error, dev vdb, sector 0 op 0x1:(WRITE)"
	estado(t, files)
	_, out := correr(t, k)
	tiene(t, out, "CRITICAL", "DB055")
	if !strings.Contains(out, "dev vdb") || !strings.Contains(out, "kling cow grow") {
		t.Errorf("el aviso no dice qué vio ni qué hacer:\n%s", out)
	}
}

// Una copia pausada porque el almacén se llenó no es un error del doctor: lo
// dice y explica cómo sale de ahí.
func TestCopiaRetenidaPorElAlmacen(t *testing.T) {
	k, files := limpio()
	k.machine.State, k.machine.Hold = api.StatePaused, api.HoldStoreFull
	estado(t, files)
	_, out := correr(t, k)
	tiene(t, out, "HIGH", "DB055")
	if !strings.Contains(out, "on hold") {
		t.Errorf("no dice que está retenida:\n%s", out)
	}
}
