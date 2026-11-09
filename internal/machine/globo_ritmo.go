package machine

import (
	"context"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
)

// Ritmo de las estadísticas del globo.
//
// El driver virtio-balloon del invitado reporta su memoria cada
// stats_polling_interval_s: cada informe es un temporizador en el VMM, una
// interrupción y un despertar del invitado, en TODAS las máquinas y aunque
// nadie lea las cifras. Solo hacen falta frescas al apretar (squeeze, el
// apretón al estar lista, el de antes de volcar), así que en reposo se reporta
// cada balloonStatsPollSec y, justo antes de apretar, se baja a
// balloonStatsPollSecApreton con PATCH /balloon/statistics. Firecracker, al
// cambiar el intervalo, le pide un informe al invitado en el acto
// (trigger_stats_update), y el apretón lee cifras de ese momento, como cuando
// se reportaba cada segundo. Al acabar se vuelve al de reposo.
//
// actual_mib y target_mib no dependen del intervalo: Firecracker los lee del
// espacio de configuración del dispositivo en cada GET, así que la espera al
// inflado (waitBalloon, esperarGlobo) no cambia.
//
// El intervalo viaja en el snapshot: un dorado o un volcado de antes de esto
// trae 1 s, y por eso cada restauración lo deja en el de reposo
// (ritmoGloboReposo). Solo Firecracker: en macOS el globo no da estadísticas.

// balloonStatsPollSec es el intervalo de reposo, el que se configura al
// arrancar. Tiene que ser >0: con 0 Firecracker no deja activarlas después.
const balloonStatsPollSec = 10

// balloonStatsPollSecApreton es el intervalo mientras se aprieta.
const balloonStatsPollSecApreton = 1

// plazoEstadisticasFrescas acota la espera al informe que pide el PATCH. El
// driver contesta en microsegundos; si no lo hace (un invitado ocupado, uno que
// no tiene el descriptor de la cola a mano) se aprieta con las últimas cifras,
// de hace como mucho balloonStatsPollSec.
const plazoEstadisticasFrescas = 250 * time.Millisecond

// estadisticasFrescas lee las estadísticas del globo después de pedirle al
// invitado un informe nuevo, y devuelve con qué volver al ritmo de reposo (hay
// que llamarlo siempre, con el apretón ya hecho). Falla solo si no hay globo o
// estadísticas, como BalloonStats; si el VMM no acepta el cambio de intervalo,
// devuelve las cifras que tenga.
func estadisticasFrescas(ctx context.Context, c *fc.Client) (*fc.BalloonStats, func(), error) {
	nada := func() {}
	viejas, err := c.BalloonStats(ctx)
	if err != nil || globoSinEstadisticas {
		return viejas, nada, err
	}
	if err := c.PatchBalloonStatsInterval(ctx, balloonStatsPollSecApreton); err != nil {
		return viejas, nada, nil
	}
	volver := func() { ritmoGloboReposo(context.WithoutCancel(ctx), c) }
	limite := time.Now().Add(plazoEstadisticasFrescas)
	paso := pasoGloboMin / 2
	for {
		s, err := c.BalloonStats(ctx)
		if err == nil && informeNuevo(viejas, s) {
			return s, volver, nil
		}
		if !time.Now().Before(limite) {
			return viejas, volver, nil
		}
		select {
		case <-ctx.Done():
			return viejas, volver, nil
		case <-time.After(paso):
		}
	}
}

// informeNuevo dice si b viene de un informe del invitado posterior al de a.
// Los fallos de página son acumulados y suben en cada informe de un invitado
// vivo; la memoria cambia casi siempre.
func informeNuevo(a, b *fc.BalloonStats) bool {
	return a.MinorFaults != b.MinorFaults || a.MajorFaults != b.MajorFaults ||
		a.FreeMemory != b.FreeMemory || a.AvailableMemory != b.AvailableMemory ||
		a.DiskCaches != b.DiskCaches
}

// ritmoGloboReposo deja las estadísticas del globo en el intervalo de reposo.
// Nunca falla: sin globo, sin estadísticas o con un VMM que no lo admite, la
// máquina sigue como estaba.
func ritmoGloboReposo(ctx context.Context, c *fc.Client) {
	if globoSinEstadisticas {
		return
	}
	_ = c.PatchBalloonStatsInterval(ctx, balloonStatsPollSec)
}
