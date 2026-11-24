package dorado

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// clienteVigilado es clienteFalso con lo que lee el vigilante de la carga: las
// estadísticas del daemon (PSS y CPU de la plantilla), el fichero de log del
// servicio (con exec) y la consola.
type clienteVigilado struct {
	*clienteFalso
	mu sync.Mutex
	// muestra da el PSS (MiB) y la CPU (s) de la plantilla en la llamada n
	// (desde 0) a ProcStats.
	muestra func(n int) (int64, float64)
	nPS     int
	// log da el contenido de /var/log/service.log en la lectura n.
	log      func(n int) string
	nLog     int
	consola  string
	sinStats bool // ProcStats contesta 404, como un daemon anterior
}

func (c *clienteVigilado) ProcStats(context.Context) (*api.ProcStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sinStats {
		return nil, &api.StatusError{Code: 404, Message: "not found"}
	}
	pss, cpu := c.muestra(c.nPS)
	c.nPS++
	return &api.ProcStats{Machines: []api.ProcStat{
		{ID: "otra", PSSMiB: 9999, CPUSeconds: 999},
		{ID: "m1", PSSMiB: pss, CPUSeconds: cpu},
	}}, nil
}

func (c *clienteVigilado) ReadFile(_ context.Context, _, path string) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.log == nil || path != "/var/log/service.log" {
		return nil, &api.StatusError{Code: 404, Message: "no such file"}
	}
	s := c.log(c.nLog)
	c.nLog++
	return io.NopCloser(strings.NewReader(s)), nil
}

func (c *clienteVigilado) LogsTail(context.Context, string, int) (string, *api.LogCursor, error) {
	return c.consola, &api.LogCursor{}, nil
}

// conRitmo acelera el vigilante para el test: sondeo y progreso cada pocos ms.
func conRitmo(t *testing.T, cada, logCada time.Duration) {
	t.Helper()
	viejoCada, viejoLog := vigiaCada, vigiaLogCada
	vigiaCada, vigiaLogCada = cada, logCada
	t.Cleanup(func() { vigiaCada, vigiaLogCada = viejoCada, viejoLog })
}

type registro struct {
	mu sync.Mutex
	b  strings.Builder
}

func (r *registro) logf(f string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(&r.b, f+"\n", a...)
}

func (r *registro) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b.String()
}

func TestAutoWait(t *testing.T) {
	for _, c := range []struct {
		bytes    int64
		min, max time.Duration
	}{
		{0, 5 * time.Minute, 5 * time.Minute},
		{100 << 20, 5 * time.Minute, 5 * time.Minute},
		// Q8 de 1,5B (1 894 532 128 bytes): 2 min + 903 s ≈ 17 min.
		{1894532128, 16 * time.Minute, 18 * time.Minute},
		// 1,9 GiB: ≈ 18 min.
		{1900 << 20, 17 * time.Minute, 19 * time.Minute},
		{100 << 30, 45 * time.Minute, 45 * time.Minute},
		{-1, 5 * time.Minute, 5 * time.Minute},
	} {
		if got := AutoWait(c.bytes); got < c.min || got > c.max {
			t.Fatalf("AutoWait(%d) = %s, quería entre %s y %s", c.bytes, got, c.min, c.max)
		}
	}
}

func TestPrecedenciaPlazo(t *testing.T) {
	const gb = 1900 << 20
	for _, c := range []struct {
		flag, carga, ficha time.Duration
		quiero             time.Duration
	}{
		{time.Minute, 2 * time.Minute, 3 * time.Minute, time.Minute},
		{0, 2 * time.Minute, 3 * time.Minute, 2 * time.Minute},
		{0, 0, 3 * time.Minute, 3 * time.Minute},
		{0, 0, 0, AutoWait(gb)},
	} {
		if got := ResolveWait(c.flag, c.carga, c.ficha, gb); got != c.quiero {
			t.Fatalf("ResolveWait(%s, %s, %s) = %s, quería %s", c.flag, c.carga, c.ficha, got, c.quiero)
		}
	}
	// Make sin Wait usa el automático por tamaño, no los 5 min fijos.
	f := &clienteFalso{}
	o := opcionesBase()
	o.Wait, o.ModelBytes = 0, gb
	o.WarmupKey = true
	o.Warmup = []Request{{Method: "GET", Path: "/x", Headers: map[string]string{"X-Api-Key": WarmupKeyPlaceholder}}}
	antes := time.Now()
	if _, err := Make(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	for _, kv := range f.run.Env {
		if v, ok := strings.CutPrefix(kv, EnvWarmupKeyExpires+"="); ok {
			cad, _ := time.Parse(time.RFC3339, v)
			if cad.Before(antes.Add(AutoWait(gb))) {
				t.Fatalf("caducidad %s: el plazo debería ser el automático (%s)", v, AutoWait(gb))
			}
			return
		}
	}
	t.Fatalf("sin caducidad en el entorno: %v", f.run.Env)
}

// P3: con estadísticas, el vigilante escribe MiB, porcentaje y ETA.
func TestProgresoSeEscribe(t *testing.T) {
	conRitmo(t, 10*time.Millisecond, 30*time.Millisecond)
	c := &clienteVigilado{
		clienteFalso: &clienteFalso{cargando: 12},
		muestra:      func(n int) (int64, float64) { return 100 + int64(n)*20, float64(n) },
	}
	o := opcionesBase()
	o.ModelBytes = 1000 << 20
	var r registro
	o.Log = r.logf
	if _, err := Make(context.Background(), c, o); err != nil {
		t.Fatal(err)
	}
	log := r.String()
	if !strings.Contains(log, "loading: ~") || !strings.Contains(log, " / 1000 MiB (") || !strings.Contains(log, "%)") ||
		!strings.Contains(log, "MiB/s") || !strings.Contains(log, "ETA") {
		t.Fatalf("el progreso debe llevar MiB, porcentaje y ETA:\n%s", log)
	}
	// Sin tamaño conocido, lo residente y el ritmo.
	c = &clienteVigilado{
		clienteFalso: &clienteFalso{cargando: 12},
		muestra:      func(n int) (int64, float64) { return 100 + int64(n)*20, float64(n) },
	}
	o.ModelBytes = 0
	r = registro{}
	o.Log = r.logf
	if _, err := Make(context.Background(), c, o); err != nil {
		t.Fatal(err)
	}
	if log := r.String(); !strings.Contains(log, "MiB resident") || strings.Contains(log, "ETA") {
		t.Fatalf("sin tamaño: %s", log)
	}
}

// P3: PSS y CPU planos durante Stall cortan con «stalled» y la cola del log,
// mucho antes del plazo total.
func TestEstancadoCorta(t *testing.T) {
	conRitmo(t, 10*time.Millisecond, time.Hour)
	c := &clienteVigilado{
		clienteFalso: &clienteFalso{cargando: 1 << 30},
		muestra:      func(int) (int64, float64) { return 300, 4 },
		log:          func(int) string { return "loading model\nllama_model_load: error\n" },
	}
	o := opcionesBase()
	o.Wait, o.Stall, o.AllowExec = time.Minute, 150*time.Millisecond, true
	t0 := time.Now()
	_, err := Make(context.Background(), c, o)
	if err == nil || !errors.Is(err, ErrStalled) || !strings.Contains(err.Error(), "stalled") ||
		!strings.Contains(err.Error(), "llama_model_load: error") {
		t.Fatalf("debería cortar por estancamiento con la cola del log: %v", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("tardó %s: el estancamiento debe cortar mucho antes del plazo", d)
	}
	if c.commit != "" || len(c.borradas) != 1 {
		t.Fatalf("commit %q, borradas %v", c.commit, c.borradas)
	}
	// Sin exec, la cola es la de la consola.
	c = &clienteVigilado{
		clienteFalso: &clienteFalso{cargando: 1 << 30},
		muestra:      func(int) (int64, float64) { return 300, 4 },
		consola:      "Out of memory: Killed process 123 (llama-server)\n",
	}
	o.AllowExec = false
	_, err = Make(context.Background(), c, o)
	if !errors.Is(err, ErrStalled) || !strings.Contains(err.Error(), "Killed process 123") {
		t.Fatalf("sin exec, la cola de la consola: %v", err)
	}
}

// P3: un calentamiento que no toca memoria nueva pero gasta CPU no está
// estancado.
func TestNoEstancadoSiHayCPU(t *testing.T) {
	conRitmo(t, 10*time.Millisecond, time.Hour)
	c := &clienteVigilado{
		clienteFalso: &clienteFalso{cargando: 25},
		muestra:      func(n int) (int64, float64) { return 300, float64(n) * 2 },
	}
	o := opcionesBase()
	o.Stall = 60 * time.Millisecond
	if _, err := Make(context.Background(), c, o); err != nil {
		t.Fatalf("con CPU avanzando no debería cortar: %v", err)
	}
}

// P3: un cliente sin estadísticas (o un daemon que no las sirve) no tiene
// progreso ni estancamiento: queda el plazo total, como hoy.
func TestSinProcStatsComoHoy(t *testing.T) {
	conRitmo(t, 10*time.Millisecond, 10*time.Millisecond)
	f := &clienteFalso{cargando: 1 << 30}
	o := opcionesBase()
	o.Wait, o.Stall = 300*time.Millisecond, 20*time.Millisecond
	_, err := Make(context.Background(), f, o)
	if err == nil || errors.Is(err, ErrStalled) || !strings.Contains(err.Error(), "not healthy after") {
		t.Fatalf("sin estadísticas, el plazo total: %v", err)
	}
	c := &clienteVigilado{clienteFalso: &clienteFalso{cargando: 1 << 30}, sinStats: true}
	var r registro
	o.Log = r.logf
	_, err = Make(context.Background(), c, o)
	if err == nil || errors.Is(err, ErrStalled) || !strings.Contains(err.Error(), "not healthy after") {
		t.Fatalf("daemon sin /procstats: %v", err)
	}
	if strings.Contains(r.String(), "loading:") {
		t.Fatalf("sin estadísticas no hay progreso: %s", r.String())
	}
}

// C9-13: el entrypoint relanza el servidor cuando muere; sin contar esas
// líneas, un servidor que muere en bucle se veía como «todavía cargando» hasta
// el plazo. Al segundo reinicio, error con la cola del log.
func TestServicioQueMuereFallaAlSegundoReinicio(t *testing.T) {
	conRitmo(t, 10*time.Millisecond, time.Hour)
	muere := func(n int) string {
		var b strings.Builder
		reinicios := n / 3 // uno cada tres lecturas
		for i := 0; i <= reinicios; i++ {
			b.WriteString("llama_model_load: loading model\n")
			if i < reinicios {
				b.WriteString(ServiceExitedMark + "137, restarting in 1s\n")
			}
		}
		return b.String()
	}
	c := &clienteVigilado{
		clienteFalso: &clienteFalso{cargando: 1 << 30},
		// La CPU avanza (el servidor carga una y otra vez): no es un estancamiento.
		muestra: func(n int) (int64, float64) { return 300 + int64(n%3)*50, float64(n) },
		log:     muere,
	}
	o := opcionesBase()
	o.Wait, o.AllowExec = time.Minute, true
	t0 := time.Now()
	_, err := Make(context.Background(), c, o)
	if err == nil || !errors.Is(err, ErrRestarting) || errors.Is(err, ErrStalled) ||
		!strings.Contains(err.Error(), ServiceExitedMark+"137") || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("debería fallar al segundo reinicio con la cola del log: %v", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("tardó %s", d)
	}
	// Un solo reinicio (p. ej. un OOM del primer intento que no se repite) se
	// tolera.
	c = &clienteVigilado{
		clienteFalso: &clienteFalso{cargando: 20},
		muestra:      func(n int) (int64, float64) { return 300, float64(n) },
		log:          func(int) string { return "x\n" + ServiceExitedMark + "1, restarting in 1s\nx\n" },
	}
	if _, err := Make(context.Background(), c, o); err != nil {
		t.Fatalf("un reinicio no basta para dar el servidor por perdido: %v", err)
	}
	// Por la consola (sin exec) también se cuentan.
	c = &clienteVigilado{
		clienteFalso: &clienteFalso{cargando: 1 << 30},
		muestra:      func(n int) (int64, float64) { return 300, float64(n) },
		consola:      ServiceExitedMark + "1, restarting in 1s\n" + ServiceExitedMark + "1, restarting in 1s\n",
	}
	o.AllowExec = false
	if _, err := Make(context.Background(), c, o); !errors.Is(err, ErrRestarting) {
		t.Fatalf("por la consola: %v", err)
	}
}

// Sin exec, el fabricante lee la consola, donde el entrypoint vuelca al morir
// las últimas líneas de service.log con ServiceLogDumpPrefix y luego la marca.
// Las marcas copiadas en un volcado no cuentan como otra muerte, y la cola del
// error enseña el log del servidor sin el prefijo.
func TestReiniciosConVolcadoEnLaConsola(t *testing.T) {
	conRitmo(t, 10*time.Millisecond, time.Hour)
	volcado := func(ls ...string) string {
		var b strings.Builder
		for _, l := range ls {
			b.WriteString(ServiceLogDumpPrefix + l + "\n")
		}
		return b.String()
	}
	marca := ServiceExitedMark + "137, restarting in 1s"
	una := "[    1.2] Out of memory: Killed process 77 (llama-server)\n" +
		volcado("llama_model_load: loading model", "llama_model_load: 1806 MiB") + marca + "\n"
	o := opcionesBase()
	o.Wait, o.AllowExec = time.Minute, false
	// Una muerte con su volcado: se tolera.
	c := &clienteVigilado{
		clienteFalso: &clienteFalso{cargando: 20},
		muestra:      func(n int) (int64, float64) { return 300, float64(n) },
		consola:      una,
	}
	if _, err := Make(context.Background(), c, o); err != nil {
		t.Fatalf("una muerte con su volcado no basta: %v", err)
	}
	// Dos: el segundo volcado trae copiada la marca de la primera, y son dos.
	c = &clienteVigilado{
		clienteFalso: &clienteFalso{cargando: 1 << 30},
		muestra:      func(n int) (int64, float64) { return 300, float64(n) },
		consola:      una + volcado("llama_model_load: 1806 MiB", marca, "llama_model_load: loading model again") + marca + "\n",
	}
	_, err := Make(context.Background(), c, o)
	if !errors.Is(err, ErrRestarting) || !strings.Contains(err.Error(), "2 times") {
		t.Fatalf("dos muertes por la consola: %v", err)
	}
	if strings.Contains(err.Error(), ServiceLogDumpPrefix) || !strings.Contains(err.Error(), "  | llama_model_load: loading model again") {
		t.Fatalf("la cola debe enseñar el log del servidor sin el prefijo del volcado: %v", err)
	}
}
