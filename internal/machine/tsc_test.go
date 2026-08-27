package machine

import (
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

// El daemon TRADUCE el error del TSC antes de devolverlo, asi que el CLI nunca
// ve el texto crudo de Firecracker: ve esta traduccion, cruzada por HTTP. Si la
// traduccion dejara de ser reconocible, `kling mcp heal` dejaria de curar nada
// y no habria ningun sintoma — seguiria diciendo "unhealthy" tan tranquilo.
func TestLaTraduccionDelTSCSigueSiendoReconocible(t *testing.T) {
	crudo := errors.New("Could not set TSC scaling within the snapshot: Invalid argument (os error 22)")

	traducido := explainRestoreErr(crudo, `snapshot "semgrep"`, "  kling mcp import semgrep -force")
	if traducido == nil {
		t.Fatal("explainRestoreErr devolvio nil sobre un fallo de TSC")
	}
	if !api.EsFalloTSC(traducido) {
		t.Errorf("el CLI no reconoceria la traduccion como fallo de TSC:\n%v", traducido)
	}
	// La traduccion debe seguir explicando y proponiendo el remedio: es lo que
	// convierte un error opaco en algo accionable.
	for _, quiero := range []string{"host boot", "kling mcp import semgrep -force", "os error 22"} {
		if !strings.Contains(traducido.Error(), quiero) {
			t.Errorf("la traduccion ya no menciona %q", quiero)
		}
	}

	// Y lo contrario: un error ajeno pasa intacto, sin adornar.
	otro := errors.New("no space left on device")
	if got := explainRestoreErr(otro, "x", "y"); got != otro {
		t.Errorf("explainRestoreErr adorno un error que no entiende: %v", got)
	}
}
