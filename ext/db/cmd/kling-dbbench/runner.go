package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/pgmini"
)

// CopyResult es lo que le pasó a UNA copia de la ronda.
type CopyResult struct {
	Index int     `json:"index"`
	OK    bool    `json:"ok"`
	MS    float64 `json:"ms"` // desde el pistoletazo hasta el recuento correcto
	// ProvisionMS: hasta que la herramienta entregó la base (antes de la
	// primera consulta). ToolMS: lo que la herramienta informa de sí misma
	// (thaw_ms de kling, duración del fork), 0 si no informa.
	ProvisionMS float64 `json:"provision_ms"`
	ToolMS      int     `json:"tool_ms,omitempty"`
	// Phase y Err solo si falló: provision, connect, auth, query, seed,
	// wrong_count; con ":timeout" si se agotó el plazo de la copia.
	Phase string `json:"phase,omitempty"`
	Err   string `json:"error,omitempty"`
}

// Round es una repetición de una celda: N copias a la vez.
type Round struct {
	Mode   string `json:"mode"`
	N      int    `json:"n"`
	Rep    int    `json:"rep"`
	Status string `json:"status"` // ok | DEGRADED | skipped
	Reason string `json:"reason,omitempty"`

	OK       int            `json:"ok"`
	Failed   int            `json:"failed"`
	Errors   map[string]int `json:"errors,omitempty"`
	Latency  Dist           `json:"latency_ms"`
	Provis   Dist           `json:"provision_ms"`
	WallMS   float64        `json:"wall_ms"`
	Copies   []CopyResult   `json:"copies,omitempty"`
	MemAvail MemStats       `json:"mem_available_mib"`
	// DiskPerCopyMiB: delta de disco usado / copias que salieron bien. -1 si
	// no se pudo medir.
	DiskPerCopyMiB float64 `json:"disk_per_copy_mib"`
	PSIBefore      float64 `json:"psi_before"`
	PSIMax         float64 `json:"psi_max"`
	// Notes: cosas que el lector debe saber (PSI alto al empezar...).
	Notes []string `json:"notes,omitempty"`
}

// MemStats: -1 = no disponible (no es Linux).
type MemStats struct {
	Before int64 `json:"before"`
	Min    int64 `json:"min"`
	// PeakUsed = Before - Min: lo que la ronda le quitó al host como mucho.
	PeakUsed int64 `json:"peak_used"`
}

// sampler mira MemAvailable y PSI cada 250 ms mientras dura una ronda.
type sampler struct {
	mu     sync.Mutex
	before int64
	min    int64
	psiMax float64
	stop   chan struct{}
	done   chan struct{}
}

func startSampler() *sampler {
	s := &sampler{before: memAvailableMiB(), stop: make(chan struct{}), done: make(chan struct{})}
	s.min = s.before
	s.psiMax = psiSome()
	go func() {
		defer close(s.done)
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				s.sample()
			}
		}
	}()
	return s
}

func (s *sampler) sample() {
	m, p := memAvailableMiB(), psiSome()
	s.mu.Lock()
	if m >= 0 && (s.min < 0 || m < s.min) {
		s.min = m
	}
	if p > s.psiMax {
		s.psiMax = p
	}
	s.mu.Unlock()
}

func (s *sampler) finish() (MemStats, float64) {
	s.sample()
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := MemStats{Before: s.before, Min: s.min, PeakUsed: -1}
	if s.before >= 0 && s.min >= 0 {
		ms.PeakUsed = s.before - s.min
	}
	return ms, s.psiMax
}

// firstCount hace lo que el test haría: conectar y contar las filas del seed.
// Reintenta mientras la base no conteste (arranque, restauración); un error de
// autenticación o un recuento distinto del esperado no se reintentan: son
// respuestas, no falta de tiempo.
func firstCount(ctx context.Context, t target, cfg *config) (phase string, err error) {
	if t.Addr == "" {
		return "provision", errors.New("the copy has no reachable address")
	}
	phase = "connect"
	for {
		var conn *pgmini.Conn
		conn, err = pgmini.Dial(ctx, pgmini.Config{Addr: t.Addr, User: t.User, Password: t.Password, Database: t.Database})
		if err == nil {
			var rows [][]string
			rows, err = conn.Query(ctx, cfg.countSQL())
			conn.Close()
			if err == nil {
				if len(rows) == 1 && len(rows[0]) == 1 && rows[0][0] == fmt.Sprint(cfg.Expect) {
					return "", nil
				}
				return "wrong_count", fmt.Errorf("count = %v, expected %d", rows, cfg.Expect)
			}
			phase = "query"
		} else {
			phase = "connect"
		}
		var pe *pgmini.Error
		if errors.As(err, &pe) && len(pe.Code) >= 2 && pe.Code[:2] == "28" {
			return "auth", err
		}
		select {
		case <-ctx.Done():
			return phase, err
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// measureCopy mide una copia desde t0.
func measureCopy(ctx context.Context, rnd round, i int, cfg *config, t0 time.Time) CopyResult {
	cctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	res := CopyResult{Index: i}
	fail := func(phase string, err error) CopyResult {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			phase += ":timeout"
		}
		res.Phase, res.Err = phase, err.Error()
		res.MS = ms(time.Since(t0))
		return res
	}
	tg, tool, err := rnd.provision(cctx, i)
	if err != nil {
		return fail("provision", err)
	}
	res.ProvisionMS, res.ToolMS = ms(time.Since(t0)), tool
	if phase, err := firstCount(cctx, tg, cfg); err != nil {
		return fail(phase, err)
	}
	res.OK, res.MS = true, ms(time.Since(t0))
	return res
}

func ms(d time.Duration) float64 { return round2(float64(d) / float64(time.Millisecond)) }

// runRound ejecuta una ronda de n copias en paralelo detrás de una barrera de
// salida: la ráfaga es una ráfaga y no una rampa.
func runRound(ctx context.Context, b backend, cfg *config, n, rep int) Round {
	r := Round{Mode: b.name(), N: n, Rep: rep, DiskPerCopyMiB: -1, Status: "ok"}
	if p := psiSome(); p >= 0 {
		r.PSIBefore = p
		if p >= cfg.PSIMax {
			r.Notes = append(r.Notes, fmt.Sprintf("PSI some avg10 was %.2f (>= %.2f) when the round started", p, cfg.PSIMax))
		}
	} else {
		r.PSIBefore = -1
	}
	rnd, err := b.prepare(ctx, rep, n)
	if err != nil {
		r.Status, r.Reason = "DEGRADED", "prepare: "+err.Error()
		r.Failed = n
		r.Errors = map[string]int{"prepare": n}
		return r
	}
	// La limpieza corre siempre, también si se cancela el contexto.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		rnd.cleanup(cctx)
	}()

	var diskBefore int64 = -1
	if cfg.DiskPath != "" {
		if u, _, err := diskUsage(cfg.DiskPath); err == nil {
			diskBefore = u
		}
	}
	smp := startSampler()
	results := measureAll(ctx, rnd, cfg, n)

	// Disco: antes de borrar nada, con todas las copias vivas.
	if cfg.DiskPath != "" && diskBefore >= 0 {
		if u, _, err := diskUsage(cfg.DiskPath); err == nil {
			r.DiskPerCopyMiB = diskPerCopy(u-diskBefore, results)
		}
	}
	r.MemAvail, r.PSIMax = smp.finish()
	fillRound(&r, results)
	for _, c := range results {
		if c.MS > r.WallMS {
			r.WallMS = c.MS
		}
	}
	return r
}

// diskPerCopy reparte el delta de disco entre las copias que salieron bien;
// -1 si no hay ninguna o el delta es negativo (otro proceso liberó espacio: el
// dato no vale y se dice así en vez de inventar un número).
func diskPerCopy(delta int64, results []CopyResult) float64 {
	ok := 0
	for _, c := range results {
		if c.OK {
			ok++
		}
	}
	if ok == 0 || delta < 0 {
		return -1
	}
	return round2(float64(delta) / float64(ok) / (1 << 20))
}

// measureAll lanza n copias detrás de una barrera de salida (la ráfaga es una
// ráfaga y no una rampa) y devuelve sus resultados. t0 es común a todas.
func measureAll(ctx context.Context, rnd round, cfg *config, n int) []CopyResult {
	results := make([]CopyResult, n)
	start := make(chan struct{})
	var t0 time.Time // se escribe antes de close(start): la barrera ordena la lectura
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = measureCopy(ctx, rnd, i, cfg, t0)
		}(i)
	}
	t0 = time.Now()
	close(start)
	wg.Wait()
	return results
}

// fillRound resume los resultados de las copias en la ronda.
func fillRound(r *Round, results []CopyResult) {
	r.Copies = results
	var lat, prov []float64
	for _, c := range results {
		if c.OK {
			r.OK++
			lat = append(lat, c.MS)
			prov = append(prov, c.ProvisionMS)
			continue
		}
		r.Failed++
		if r.Errors == nil {
			r.Errors = map[string]int{}
		}
		r.Errors[c.Phase]++
	}
	r.Latency, r.Provis = summarize(lat), summarize(prov)
	r.Status = failStatus(r.Failed, len(results))
}

// errorsSummary devuelve "phase×N, ..." ordenado, para la tabla.
func errorsSummary(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := ""
	for i, k := range keys {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s×%d", k, m[k])
	}
	return s
}

func warnf(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
