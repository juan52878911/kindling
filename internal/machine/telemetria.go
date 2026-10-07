package machine

// Telemetría del ciclo de vida para /metrics: cuántas operaciones salen bien,
// cuántas fallan, cuántas se rechazan por falta de sitio, cuánto tardan, y
// lo que hace el vigilante por su cuenta (expulsar dormidas, matar VMM
// huérfanos). Antes /metrics solo contaba máquinas por estado y memoria: un
// daemon que rechazaba la mitad de los arranques se veía igual que uno sano.
//
// Contadores atómicos y nada más: se tocan en el camino de cada operación y no
// pueden ponerle un candado delante. Se leen con Telemetria(), que copia.

import (
	"errors"
	"sync/atomic"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
)

// LimitesDuracionMS son los límites superiores (en ms, inclusivos) de los
// cubos de los histogramas de duración. Cubren del thaw de pocos ms al
// arranque en frío de decenas de segundos en un host cargado.
var LimitesDuracionMS = [...]int64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000}

// Las operaciones que se cuentan, y las clases de duración. Un run puede ser
// un arranque en frío (boot) o una restauración desde snapshot (restore); un
// thaw, cargar un volcado (thaw) o reanudar una pausada (resume): son
// distribuciones distintas y mezclarlas no diría nada.
const (
	OpRun    = "run"
	OpThaw   = "thaw"
	OpFreeze = "freeze"

	DurBoot    = "boot"
	DurRestore = "restore"
	DurThaw    = "thaw"
	DurResume  = "resume"
	DurFreeze  = "freeze"
)

var (
	opsTelemetria = []string{OpRun, OpThaw, OpFreeze}
	durTelemetria = []string{DurBoot, DurRestore, DurThaw, DurResume, DurFreeze}
	// codigosRechazo son las negativas de admisión: tope de máquinas (409),
	// disco (503) y memoria (507). Ver admision.go.
	codigosRechazo = []int{api.StatusMachineLimit, api.StatusDiskFull, api.StatusInsufficientMemory}
)

type contadorOp struct{ ok, fallos atomic.Int64 }

type histograma struct {
	cubos  [len(LimitesDuracionMS) + 1]atomic.Int64 // el último es +Inf
	suma   atomic.Int64
	cuenta atomic.Int64
}

func (h *histograma) observar(ms int64) {
	i := len(LimitesDuracionMS)
	for j, l := range LimitesDuracionMS {
		if ms <= l {
			i = j
			break
		}
	}
	h.cubos[i].Add(1)
	h.suma.Add(max(ms, 0))
	h.cuenta.Add(1)
}

type telemetria struct {
	ops       map[string]*contadorOp
	dur       map[string]*histograma
	rechazos  map[int]*atomic.Int64
	gcExpulsa atomic.Int64
	huerfanos atomic.Int64
}

// Los mapas se rellenan una vez y después solo se leen: los valores son
// atómicos. Una variable de paquete y no un campo del Manager, para que los
// managers de prueba hechos a mano no tengan que acordarse de inicializarla;
// en el daemon hay un solo Manager.
var tel = func() *telemetria {
	t := &telemetria{ops: map[string]*contadorOp{}, dur: map[string]*histograma{}, rechazos: map[int]*atomic.Int64{}}
	for _, o := range opsTelemetria {
		t.ops[o] = &contadorOp{}
	}
	for _, d := range durTelemetria {
		t.dur[d] = &histograma{}
	}
	for _, c := range codigosRechazo {
		t.rechazos[c] = &atomic.Int64{}
	}
	return t
}()

// exito anota una operación que salió bien y lo que tardó.
func (t *telemetria) exito(op, dur string, ms int64) {
	t.ops[op].ok.Add(1)
	t.dur[dur].observar(ms)
}

// fin clasifica el error con el que acabó una operación. Sin error no hace
// nada: el éxito lo anota exito() donde se mide. Una negativa de admisión es
// un rechazo y no un fallo; un error de quien llama (no existe, estado que no
// lo admite) no es ninguna de las dos cosas, y errYaNoToca es el vigilante
// cambiando de idea.
func (t *telemetria) fin(op string, err error) {
	var se *api.StatusError
	switch {
	case err == nil, errors.Is(err, errYaNoToca), errors.Is(err, ErrNoMachine), errors.Is(err, ErrWrongState):
		return
	case errors.As(err, &se) && t.rechazos[se.Code] != nil:
		t.rechazos[se.Code].Add(1)
		return
	}
	t.ops[op].fallos.Add(1)
}

// Histograma es una distribución de duraciones en ms, con cubos NO
// acumulados (Cubos[i] cuenta lo que cae en (Limites[i-1], Limites[i]]; el
// último, lo que pasa del último límite).
type Histograma struct {
	Cubos  []int64
	Suma   int64
	Cuenta int64
}

// Telemetria es la foto que lee /metrics.
type Telemetria struct {
	OK, Fallos       map[string]int64 // por operación (OpRun, OpThaw, OpFreeze)
	Rechazos         map[int]int64    // por código HTTP de la negativa
	Duraciones       map[string]Histograma
	ExpulsionesGC    int64 // dormidas que gcDisk eliminó para liberar disco
	HuerfanosMatados int64 // VMM sin máquina que el daemon mató
	// DiscoLibreMiB es el disco libre bajo la raíz; -1 si no se pudo medir.
	DiscoLibreMiB int64
	// PendienteMiB es la memoria reservada por arranques en curso.
	PendienteMiB int64
}

// Telemetria toma la foto de los contadores, el disco libre y la memoria
// reservada por los arranques en curso.
func (m *Manager) Telemetria() Telemetria {
	out := Telemetria{
		OK: map[string]int64{}, Fallos: map[string]int64{}, Rechazos: map[int]int64{},
		Duraciones:       map[string]Histograma{},
		ExpulsionesGC:    tel.gcExpulsa.Load(),
		HuerfanosMatados: tel.huerfanos.Load(),
		DiscoLibreMiB:    -1,
	}
	for o, c := range tel.ops {
		out.OK[o], out.Fallos[o] = c.ok.Load(), c.fallos.Load()
	}
	for c, n := range tel.rechazos {
		out.Rechazos[c] = n.Load()
	}
	for d, h := range tel.dur {
		hh := Histograma{Cubos: make([]int64, len(h.cubos)), Suma: h.suma.Load(), Cuenta: h.cuenta.Load()}
		for i := range h.cubos {
			hh.Cubos[i] = h.cubos[i].Load()
		}
		out.Duraciones[d] = hh
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(m.root, &st); err == nil {
		out.DiscoLibreMiB = int64(st.Bavail) * int64(st.Bsize) >> 20
	}
	m.mu.RLock()
	out.PendienteMiB = int64(m.pendingMiB)
	m.mu.RUnlock()
	return out
}
