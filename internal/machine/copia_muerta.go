package machine

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Copias muertas. En una ráfaga de restauraciones, algunas copias no llegan a
// correr (o su VMM sale solo) y su agente no contesta nunca: en 22 casos
// medidos, el segundo POST de 6 s a la misma copia tampoco, y la creación
// acababa en un 500 a los 12 s. Una copia que no contesta pronto se descarta
// y se restaura otra, con VMM, red e id nuevos.

// errCopiaMuerta marca el error de una copia descartada porque su agente no se
// resincronizó a tiempo: runFrom la repite una vez.
var errCopiaMuerta = errors.New("restored copy did not come alive")

// copiasMuertas cuenta las copias descartadas por errCopiaMuerta
// (kling_restore_dead_copies_total).
var copiasMuertas atomic.Int64

// CopiasMuertas es el valor de kling_restore_dead_copies_total desde que
// arrancó el daemon.
func CopiasMuertas() int64 { return copiasMuertas.Load() }

// plazoCopiaViva es lo que se espera al agente de una copia recién restaurada:
// 4 veces el peor resync correcto medido (~390 ms). KLING_RESYNC_FIRST_MS lo
// cambia, para hosts más lentos que la EC2 donde se midió.
func plazoCopiaViva() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("KLING_RESYNC_FIRST_MS")); err == nil && v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return 1500 * time.Millisecond
}

// esperaDiagnostico da al diagnóstico de una copia muerta (dos muestras de la
// vCPU, 250 ms) tiempo de acabar ANTES de que se mate su VMM: sin proceso no
// hay muestra. Con tope, por si el host está tan cargado que ni /proc contesta.
const esperaDiagnostico = 1500 * time.Millisecond

func esperarDiagnostico(diag <-chan struct{}) {
	if diag == nil {
		return
	}
	t := time.NewTimer(esperaDiagnostico)
	defer t.Stop()
	select {
	case <-diag:
	case <-t.C:
	}
}

// conCopiaDeRepuesto ejecuta intento y, si falla porque la copia estaba muerta
// (errCopiaMuerta), lo repite UNA vez: restauración entera, con id, red y VMM
// nuevos. El segundo error se devuelve tal cual.
//
// muerta, si no es nil, se llama antes del repuesto: la puerta de arranques
// retrocede (puertaRetroceder), porque una copia muda en un host anidado es
// la misma señal que un VMM que muere.
func conCopiaDeRepuesto(ctx context.Context, nombre string, muerta func(), intento func() (*api.Machine, error)) (*api.Machine, error) {
	mc, err := intento()
	if err == nil || !errors.Is(err, errCopiaMuerta) || ctx.Err() != nil {
		return mc, err
	}
	log.Printf("%s: the restored copy did not come alive; restoring another one", nombre)
	if muerta != nil {
		muerta()
	}
	return intento()
}
