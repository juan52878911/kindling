package machine

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// Apretar el globo antes de volcar.
//
// Un volcado (freeze, save) escribe la RAM del invitado tal cual está, y lo
// que está LIBRE para el invitado no está a ceros: son páginas que usó y soltó,
// y sobre todo su caché de página, que en un servicio con modelos es casi
// tanto como el propio proceso (Hindsight, medido: 1,1 GiB de proceso y 0,9
// GiB de caché de los ficheros del modelo, que el proceso ya tiene en memoria
// anónima). perforarHuecos solo quita lo que está a ceros, así que todo eso
// iba al mem.file: 1,9 GiB de dorado para 1,1 GiB en uso, y cada copia o cada
// thaw lo faultea desde ahí.
//
// Inflar el globo justo antes hace que el invitado entregue esas páginas, y
// Firecracker las suelta con madvise(DONTNEED): desde entonces se leen a
// ceros, y el volcado las perfora. Después se desinfla a la línea base, para
// que el estado volcado —el que heredan las copias— tenga el presupuesto
// entero y el globo como al arrancar. La caché que el invitado soltó vuelve a
// entrar desde el disco de la imagen si hace falta, y ese fichero sí lo
// comparten todas las copias en la caché del HOST.
//
// A diferencia de squeeze, aquí da igual que la máquina comparta memoria con
// un dorado (MemShared): el VMM muere con el volcado y lo que se perdería de
// compartido no llega a existir. Solo en Firecracker: en macOS el framework
// vuelve a poblar las páginas al desinflar (ver squeezeLocked) y el volcado
// no se perfora. KLING_SQUEEZE_BEFORE_DUMP=0 lo apaga.

// plazoApretonVolcado acota lo que se espera al driver del invitado: un
// freeze tarda ~2 s y no debe doblarse por un invitado que no coopera.
const plazoApretonVolcado = 2 * time.Second

// apretarAntesDeVolcar devuelve los MiB que el invitado llegó a entregar, 0 si
// no había globo, no había nada que reclamar o está apagado. Nunca falla: un
// volcado sin apretón es el de siempre.
func apretarAntesDeVolcar(ctx context.Context, c *fc.Client, mc *api.Machine) int {
	if globoSinEstadisticas || os.Getenv("KLING_SQUEEZE_BEFORE_DUMP") == "0" {
		return 0
	}
	stats, err := c.BalloonStats(ctx)
	if err != nil {
		return 0 // sin globo (imagen anterior a él): el volcado de siempre
	}
	reclaim := int(stats.FreeMemory >> 20)
	if avail := int(stats.AvailableMemory >> 20); avail > reclaim {
		reclaim = avail
	}
	// ActualMiB puede venir OBSOLETO: el driver reporta cada segundo, y una
	// copia recién restaurada trae en el dorado el valor que el invitado vio
	// la última vez (medido: el inflado de un apretón anterior, ya desinflado).
	// Lo disponible nunca supera el total menos el globo, así que el objetivo
	// se acota al total: con un actual obsoleto, pedir más daba un 400 del VMM.
	target := stats.ActualMiB + reclaim
	if tot := int(stats.TotalMemory >> 20); tot > 0 && target > tot {
		target = tot
	}
	target -= balloonSqueezeMarginMiB
	if target <= stats.ActualMiB {
		return 0
	}
	if err := c.PatchBalloon(ctx, target); err != nil {
		log.Printf("warning: %s: could not inflate the balloon before the dump: %v", mc.ID[:12], err)
		return 0
	}
	entregado := esperarGlobo(ctx, c, target, plazoApretonVolcado) - stats.ActualMiB
	// A la línea base y no a 0, como squeeze: con techo (mem_max) el globo
	// retiene la diferencia. Sin cancelar: el volcado sigue aunque el cliente
	// se haya ido, y un globo inflado en el dorado lo heredarían las copias.
	if err := c.PatchBalloon(context.WithoutCancel(ctx), globoBase(mc)); err != nil {
		log.Printf("warning: %s: could not deflate the balloon before the dump: %v", mc.ID[:12], err)
	}
	return max(entregado, 0)
}

// esperarGlobo espera, como mucho plazo, a que el globo se acerque al
// objetivo, y devuelve a cuánto llegó (ActualMiB) según el invitado.
func esperarGlobo(ctx context.Context, c *fc.Client, targetMiB int, plazo time.Duration) int {
	deadline := time.Now().Add(plazo)
	actual := 0
	for {
		s, err := c.BalloonStats(ctx)
		if err == nil {
			actual = s.ActualMiB
			if actual >= targetMiB-16 {
				return actual
			}
		}
		if !time.Now().Before(deadline) {
			return actual
		}
		time.Sleep(50 * time.Millisecond)
	}
}
