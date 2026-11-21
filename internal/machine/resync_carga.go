package machine

import (
	"context"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// plazoResyncCarga es el plazo de un resync con agente conocido.
func plazoResyncCarga(enVuelo, ncpu int) time.Duration { return resyncPlazoConAgente }

// sinAgenteAlCongelar dice si Freeze debe apuntar que no hay agente.
func (m *Manager) sinAgenteAlCongelar(ctx context.Context, id string) bool {
	return !m.agenteEscucha(ctx, id)
}

// diagnosticoInvitado describe en una línea cómo está el invitado.
func diagnosticoInvitado(pid int, red string, espera func()) string { return "" }

// resyncTrasThaw es el resync de un thaw; devuelve un aviso si no se logró.
func (m *Manager) resyncTrasThaw(ctx context.Context, id string, allowExec bool) (resultadoResync, string) {
	return m.resyncGuest(ctx, id, "", api.ResyncThaw), ""
}

// resyncThawReintentos reintenta en segundo plano un resync de thaw fallido.
func (m *Manager) resyncThawReintentos(id string, ej ejecucion, estricto bool) {}

// resyncThawEspera es el retroceso del resync de thaw en segundo plano.
var resyncThawEspera = []time.Duration{time.Second}

// procRaiz es dónde está /proc; los tests lo apuntan a un directorio falso.
var procRaiz = "/proc"
