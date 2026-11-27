package dorado

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Plazo y vigilancia de la carga (ronda 9, P3 y C9-13). El plazo deja de ser
// fijo y ciego: crece con el tamaño del modelo, y un vigilante lee del daemon
// la memoria y la CPU de la plantilla para contar el progreso, cortar un
// servidor que no avanza y otro que muere en bucle.

const (
	// DefaultStall es cuánto puede pasar la plantilla sin tocar memoria nueva
	// NI gastar CPU antes de darla por estancada.
	DefaultStall = 2 * time.Minute

	autoWaitMin  = 5 * time.Minute
	autoWaitMax  = 45 * time.Minute
	autoWaitBase = 2 * time.Minute
	// autoWaitBps es el ritmo de carga que se supone: la mitad de lo que el
	// laboratorio anidado da con fallos de página de ~1 ms (≈ 4 MiB/s). En
	// metal no se nota: es un tope, no una espera.
	autoWaitBps = 2 << 20

	// Progreso mínimo que cuenta como «avanza» dentro de Stall.
	stallMinMiB = 8
	stallMinCPU = 1.0
	// maxReinicios es a cuántas líneas ServiceExitedMark se da el servidor
	// por perdido: una puede ser un OOM del primer intento; dos es un bucle.
	maxReinicios = 2
	// colaLineas es cuánto del log acompaña a un error.
	colaLineas = 20
	// maxLogServicio acota lo que se lee de service.log en cada vuelta.
	maxLogServicio = 4 << 20
	// logServicio es donde el entrypoint de las imágenes de kindling deja la
	// salida del servidor (scripts/81-base-image.sh, internal/imagen).
	logServicio = "/var/log/service.log"
)

// Ritmo del vigilante: sondea cada vigiaCada y escribe el progreso cada
// vigiaLogCada. Variables para que los tests no esperen minutos.
var (
	vigiaCada    = 5 * time.Second
	vigiaLogCada = 15 * time.Second
)

// ErrStalled y ErrRestarting son las dos causas por las que el vigilante corta
// la carga antes del plazo (errors.Is).
var (
	ErrStalled    = errors.New("stalled")
	ErrRestarting = errors.New("the server keeps restarting")
)

// AutoWait es el plazo de carga para un modelo de bytes:
// clamp(5 min, 2 min + bytes / 2 MiB/s, 45 min). bytes <= 0 = 5 min.
func AutoWait(bytes int64) time.Duration {
	if bytes <= 0 {
		return autoWaitMin
	}
	d := autoWaitBase + time.Duration(bytes/autoWaitBps)*time.Second
	return min(max(d, autoWaitMin), autoWaitMax)
}

// ResolveWait aplica la precedencia del plazo: la bandera (-wait) > load_wait
// del bloque en ai.json > el de la ficha del catálogo > el automático por
// tamaño. Un valor <= 0 es «no dicho».
func ResolveWait(flag, loadWait, catalog time.Duration, bytes int64) time.Duration {
	for _, d := range []time.Duration{flag, loadWait, catalog} {
		if d > 0 {
			return d
		}
	}
	return AutoWait(bytes)
}

// Lo que el vigilante usa del daemon, si el cliente lo tiene (*api.Client sí):
// por aserción de tipo, para que un Client mínimo siga valiendo.
type (
	procStatser interface {
		ProcStats(ctx context.Context) (*api.ProcStats, error)
	}
	lectorFicheros interface {
		ReadFile(ctx context.Context, ref, path string) (io.ReadCloser, error)
	}
	lectorConsola interface {
		LogsTail(ctx context.Context, ref string, tail int) (string, *api.LogCursor, error)
	}
)

// vigia mira la plantilla mientras carga y se calienta.
type vigia struct {
	c         Client
	ref       string
	total     int64 // bytes del modelo; 0 = no se sabe
	stall     time.Duration
	allowExec bool
	logf      func(string, ...any)
	limpiar   func(string) string // quita lo que no debe verse (la clave)
	cancel    context.CancelCauseFunc

	mu   sync.Mutex
	fase string

	// Estado de la muestra.
	conStats   bool
	base       int64 // MiB de la primera muestra
	ultMem     int64
	t0         time.Time
	refMem     int64
	refCPU     float64
	refT       time.Time
	ultimoLog  time.Time
	sinConsola bool
}

// vigilar arranca el vigilante si el cliente sabe dar estadísticas o logs;
// devuelve con qué pararlo (espera a que acabe). ctx es el de las fases que
// se vigilan y cancel su cancelación con causa.
func vigilar(ctx context.Context, cancel context.CancelCauseFunc, c Client, ref string, o *Options, limpiar func(string) string) (fase func(string), parar func()) {
	_, ps := c.(procStatser)
	_, lf := c.(lectorFicheros)
	_, lc := c.(lectorConsola)
	nada := func(string) {}
	if !ps && !(lf && o.AllowExec) && !lc {
		return nada, func() {}
	}
	v := &vigia{
		c: c, ref: ref, total: o.ModelBytes, stall: o.Stall, allowExec: o.AllowExec,
		logf: o.Log, limpiar: limpiar, cancel: cancel, fase: "loading", conStats: ps,
	}
	if v.logf == nil {
		v.logf = func(string, ...any) {}
	}
	if v.stall == 0 {
		v.stall = DefaultStall
	}
	hecho := make(chan struct{})
	pctx, pcancel := context.WithCancel(ctx)
	go func() {
		defer close(hecho)
		v.bucle(pctx)
	}()
	return func(f string) {
			v.mu.Lock()
			v.fase = f
			v.mu.Unlock()
		}, func() {
			pcancel()
			<-hecho
		}
}

func (v *vigia) bucle(ctx context.Context) {
	t := time.NewTicker(vigiaCada)
	defer t.Stop()
	for {
		if err := v.vuelta(ctx); err != nil {
			v.cancel(err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// vuelta es una muestra: cuenta reinicios, mira si avanza y, cada
// vigiaLogCada, escribe el progreso. Devuelve la causa para cortar, o nil.
func (v *vigia) vuelta(ctx context.Context) error {
	ahora := time.Now()
	if n, log := v.reinicios(ctx); n >= maxReinicios {
		return fmt.Errorf("%w: the server exited %d times while loading (a crash, or out of memory: raise -mem if the log says so)%s",
			ErrRestarting, n, cola(v.limpiar(log)))
	}
	if !v.conStats {
		return nil
	}
	mem, cpu, ok := v.muestra(ctx)
	if !ok {
		return nil
	}
	if v.t0.IsZero() {
		v.t0, v.base, v.ultMem = ahora, mem, mem
		v.refMem, v.refCPU, v.refT, v.ultimoLog = mem, cpu, ahora, ahora
		return nil
	}
	v.ultMem = mem
	if mem-v.refMem >= stallMinMiB || cpu-v.refCPU >= stallMinCPU {
		v.refMem, v.refCPU, v.refT = mem, cpu, ahora
	} else if v.stall > 0 && ahora.Sub(v.refT) >= v.stall {
		return fmt.Errorf("%w: in %s the server touched under %d MiB of new memory and used under %.0f s of CPU; it is not loading%s",
			ErrStalled, v.stall, stallMinMiB, stallMinCPU, cola(v.limpiar(v.logServidor(ctx))))
	}
	if ahora.Sub(v.ultimoLog) >= vigiaLogCada {
		v.ultimoLog = ahora
		v.mu.Lock()
		fase := v.fase
		v.mu.Unlock()
		v.logf("%s: %s", fase, progreso(mem-v.base, v.total, ahora.Sub(v.t0)))
	}
	return nil
}

// progreso es la línea de progreso: «~812 / 1946 MiB (42 %) · 6.1 MiB/s · ETA
// 3m05s», o sin tamaño «~812 MiB resident · 6.1 MiB/s». Es aproximada (lleva
// kernel y caché del rootfs; se resta la primera muestra), y por eso la «~».
func progreso(mib, total int64, dt time.Duration) string {
	mib = max(mib, 0)
	ritmo := 0.0
	if s := dt.Seconds(); s > 0 {
		ritmo = float64(mib) / s
	}
	if total <= 0 {
		return fmt.Sprintf("~%d MiB resident · %.1f MiB/s", mib, ritmo)
	}
	tot := max(total>>20, 1)
	pct := min(mib*100/tot, 99)
	eta := "?"
	if ritmo > 0 && mib < tot {
		eta = (time.Duration(float64(tot-mib)/ritmo) * time.Second).Round(time.Second).String()
	} else if mib >= tot {
		eta = "soon"
	}
	return fmt.Sprintf("~%d / %d MiB (%d %%) · %.1f MiB/s · ETA %s", mib, tot, pct, ritmo, eta)
}

// muestra es la memoria (MiB: PSS en Linux, huella en macOS) y la CPU (s) de
// la plantilla. Un daemon sin /procstats apaga las estadísticas para siempre.
func (v *vigia) muestra(ctx context.Context) (int64, float64, bool) {
	sctx, cancel := context.WithTimeout(ctx, vigiaCada)
	defer cancel()
	ps, err := v.c.(procStatser).ProcStats(sctx)
	if err != nil {
		if api.IsUnsupported(err) {
			v.conStats = false
		}
		return 0, 0, false
	}
	for _, m := range ps.Machines {
		if m.ID == v.ref {
			mem := m.PSSMiB
			if m.FootprintMiB > 0 {
				mem = m.FootprintMiB
			}
			return mem, m.CPUSeconds, true
		}
	}
	return 0, 0, false
}

// reinicios cuenta las líneas ServiceExitedMark del log del servidor y
// devuelve también el log leído (para la cola del error). En la consola, las
// copias de la marca dentro de un volcado llevan ServiceLogDumpPrefix delante
// y no cuentan: cada muerte deja una sola marca sin prefijo.
func (v *vigia) reinicios(ctx context.Context) (int, string) {
	log := v.logServidor(ctx)
	n := 0
	for _, l := range strings.Split(log, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), ServiceExitedMark) {
			n++
		}
	}
	return n, log
}

// logServidor es la salida del servidor: service.log si hay exec (es donde la
// escribe el entrypoint), y si no la consola de la máquina, que lleva los
// avisos del kernel (un OOM, por ejemplo) y, por cada muerte del servicio, las
// últimas líneas de service.log que vuelca el entrypoint y su marca.
func (v *vigia) logServidor(ctx context.Context) string {
	lctx, cancel := context.WithTimeout(ctx, vigiaCada)
	defer cancel()
	if lf, ok := v.c.(lectorFicheros); ok && v.allowExec {
		// Mientras el fichero no exista (el servidor aún no escribió), la
		// lectura falla y se mira la consola.
		if rc, err := lf.ReadFile(lctx, v.ref, logServicio); err == nil {
			b, _ := io.ReadAll(io.LimitReader(rc, maxLogServicio))
			rc.Close()
			return string(b)
		}
	}
	if lc, ok := v.c.(lectorConsola); ok && !v.sinConsola {
		// 400 líneas: cabe un informe de OOM del kernel (~70 líneas) y un
		// volcado del entrypoint (31) por muerte, y hacen falta dos muertes.
		out, _, err := lc.LogsTail(lctx, v.ref, 400)
		if err == nil {
			return out
		}
		// Un daemon que no la sirve no la servirá en la vuelta siguiente.
		v.sinConsola = api.IsUnsupported(err)
	}
	return ""
}

// cola son las últimas colaLineas de log, sangradas, para un error. Las
// líneas que el entrypoint volcó a la consola al morir el servicio
// (ServiceLogDumpPrefix) salen sin el prefijo: son el log del servidor.
func cola(log string) string {
	log = strings.TrimRight(log, "\n")
	if strings.TrimSpace(log) == "" {
		return ""
	}
	ls := strings.Split(log, "\n")
	if len(ls) > colaLineas {
		ls = ls[len(ls)-colaLineas:]
	}
	for i, l := range ls {
		l = strings.TrimPrefix(l, ServiceLogDumpPrefix)
		if len(l) > 300 {
			l = l[:300] + "..."
		}
		ls[i] = "  | " + l
	}
	return "\nlast lines of the server log:\n" + strings.Join(ls, "\n")
}
