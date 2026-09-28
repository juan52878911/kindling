package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metric es una línea del formato de texto de Prometheus: nombre, etiquetas y
// valor. Solo lo que escribe el daemon (internal/daemon/metrics.go): gauges sin
// marca de tiempo. Si trae marca de tiempo, se ignora.
type metric struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// parseMetrics lee el /metrics del daemon. Escrito a mano por la misma razón
// que el daemon lo escribe a mano: son pocas líneas y no justifican una
// dependencia. Una línea que no se entiende es un error, no se salta: un
// parser que traga basura en silencio daría gráficas con ceros que no lo son.
func parseMetrics(r io.Reader) ([]metric, error) {
	var out []metric
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	ln := 0
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m, err := parseMetricLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", ln, err)
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

func parseMetricLine(line string) (metric, error) {
	m := metric{Labels: map[string]string{}}
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 {
		return m, fmt.Errorf("no value in %q", line)
	}
	m.Name = line[:i]
	rest := line[i:]
	if rest[0] == '{' {
		var err error
		rest, err = parseLabels(rest[1:], m.Labels)
		if err != nil {
			return m, fmt.Errorf("%s: %w", m.Name, err)
		}
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return m, fmt.Errorf("%s: no value", m.Name)
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return m, fmt.Errorf("%s: bad value %q", m.Name, fields[0])
	}
	m.Value = v
	return m, nil
}

// parseLabels lee `k="v",k2="v2"}` (ya sin la llave de apertura) y devuelve lo
// que queda tras la llave de cierre. Deshace los escapes de Prometheus: \\, \"
// y \n, los mismos que emite esc() en el daemon.
func parseLabels(s string, into map[string]string) (string, error) {
	for {
		s = strings.TrimLeft(s, " ,")
		if s == "" {
			return "", fmt.Errorf("unterminated labels")
		}
		if s[0] == '}' {
			return s[1:], nil
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 || eq+1 >= len(s) || s[eq+1] != '"' {
			return "", fmt.Errorf("bad label near %q", s)
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+2:]
		var b strings.Builder
		closed := false
		for j := 0; j < len(s); j++ {
			c := s[j]
			if c == '\\' && j+1 < len(s) {
				j++
				switch s[j] {
				case 'n':
					b.WriteByte('\n')
				default:
					b.WriteByte(s[j])
				}
				continue
			}
			if c == '"' {
				s = s[j+1:]
				closed = true
				break
			}
			b.WriteByte(c)
		}
		if !closed {
			return "", fmt.Errorf("unterminated value for label %q", key)
		}
		into[key] = b.String()
	}
}

// parsePSI lee /proc/pressure/memory: "some avg10" y "full avg10". ok=false si
// no aparece la línea some (PSI apagado).
func parsePSI(r io.Reader) (some, full float64, ok bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		var v float64
		found := false
		for _, c := range f[1:] {
			if x, cut := strings.CutPrefix(c, "avg10="); cut {
				if p, err := strconv.ParseFloat(x, 64); err == nil {
					v, found = p, true
				}
			}
		}
		if !found {
			continue
		}
		switch f[0] {
		case "some":
			some, ok = v, true
		case "full":
			full = v
		}
	}
	return some, full, ok
}

// sample es una foto del host durante la prueba.
type sample struct {
	At        time.Duration // desde el inicio de la ronda
	Live      int           // microVMs vivas (con proceso) de los servicios medidos
	IDs       []string      // sus ids, para contar réplicas distintas
	TargetPSS float64       // PSS sumada de esas microVMs, MiB
	TotalPSS  float64       // kling_total_pss_mib
	AvailMiB  float64       // kling_available_mib
	Frozen    float64       // kling_machines{state="frozen"} (congeladas, de todo el host)
	PSISome   float64       // -1 si no hay PSI
}

// reduce convierte el /metrics en una foto, contando solo las máquinas de los
// servicios medidos (por la etiqueta service, o por from: el snapshot del que
// nació, que para el gateway MCP es el nombre del servicio).
func reduce(ms []metric, targets map[string]bool) sample {
	var s sample
	for _, m := range ms {
		switch m.Name {
		case "kling_machine_pss_mib":
			if targets[m.Labels["service"]] || targets[m.Labels["from"]] {
				s.Live++
				s.IDs = append(s.IDs, m.Labels["id"])
				s.TargetPSS += m.Value
			}
		case "kling_total_pss_mib":
			s.TotalPSS = m.Value
		case "kling_available_mib":
			s.AvailMiB = m.Value
		case "kling_machines":
			if st := m.Labels["state"]; st == "frozen" || st == "warm" { // "warm": daemons hasta 0.13
				s.Frozen += m.Value
			}
		}
	}
	return s
}

// daemonClient habla con el daemon por su socket unix, como el CLI. Solo lee:
// /metrics e /info.
func daemonClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			MaxIdleConns: 2,
		},
	}
}

// sampler toma fotos cada `every` hasta que se cancela su contexto.
type sampler struct {
	client  *http.Client
	targets map[string]bool
	psiPath string
	t0      time.Time

	mu      sync.Mutex
	samples []sample
	errs    int
	lastErr string
}

func (s *sampler) once(ctx context.Context) (sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://kling/metrics", nil)
	if err != nil {
		return sample{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return sample{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return sample{}, fmt.Errorf("/metrics: HTTP %d", resp.StatusCode)
	}
	ms, err := parseMetrics(resp.Body)
	if err != nil {
		return sample{}, fmt.Errorf("/metrics: %w", err)
	}
	sm := reduce(ms, s.targets)
	sm.PSISome = -1
	if f, err := os.Open(s.psiPath); err == nil {
		if some, _, ok := parsePSI(f); ok {
			sm.PSISome = some
		}
		f.Close()
	}
	sm.At = time.Since(s.t0)
	return sm, nil
}

func (s *sampler) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		sm, err := s.once(ctx)
		s.mu.Lock()
		if err != nil {
			if ctx.Err() == nil {
				s.errs++
				s.lastErr = err.Error()
			}
		} else {
			s.samples = append(s.samples, sm)
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// last devuelve la última foto tomada después de `after`, si la hay.
func (s *sampler) last(after time.Duration) (sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.samples); n > 0 && s.samples[n-1].At > after {
		return s.samples[n-1], true
	}
	return sample{}, false
}

// HostMetrics es lo que se saca de las fotos.
type HostMetrics struct {
	Samples int `json:"samples"`
	// SampleErrors son las fotos que no se pudieron tomar (el daemon no
	// contestó a tiempo, p. ej. bajo presión). Se cuentan: una prueba con la
	// mitad de las fotos perdidas no es la misma prueba.
	SampleErrors int    `json:"sample_errors"`
	LastError    string `json:"last_error,omitempty"`
	// PeakLive es el máximo de microVMs vivas a la vez de los servicios medidos
	// durante la ronda; DistinctMachines, cuántas distintas se vieron (primarias
	// + réplicas).
	PeakLive         int     `json:"peak_live"`
	DistinctMachines int     `json:"distinct_machines"`
	PeakTargetPSSMiB float64 `json:"peak_target_pss_mib"`
	PeakTotalPSSMiB  float64 `json:"peak_total_pss_mib"`
	MinAvailableMiB  float64 `json:"min_available_mib"`
	MaxPSISomeAvg10  float64 `json:"max_psi_some_avg10"` // -1 = sin PSI
	// TimeToZeroMS es lo que tardaron en desaparecer (congeladas o retiradas)
	// todas las microVMs vivas de los servicios medidos desde que acabó la
	// ronda. nil = no se llegó a cero dentro de -settle (o no se esperó).
	TimeToZeroMS *float64 `json:"time_to_zero_ms"`
}

// summarizeSamples reduce las fotos de la ronda (hasta end) y, si se llegó a
// cero después, el tiempo hasta cero.
func summarizeSamples(ss []sample, end time.Duration) HostMetrics {
	h := HostMetrics{MaxPSISomeAvg10: -1, MinAvailableMiB: -1}
	ids := map[string]bool{}
	for _, s := range ss {
		if s.At <= end {
			h.Samples++
			if s.Live > h.PeakLive {
				h.PeakLive = s.Live
			}
			for _, id := range s.IDs {
				ids[id] = true
			}
			h.PeakTargetPSSMiB = max(h.PeakTargetPSSMiB, s.TargetPSS)
		}
		h.PeakTotalPSSMiB = max(h.PeakTotalPSSMiB, s.TotalPSS)
		if h.MinAvailableMiB < 0 || s.AvailMiB < h.MinAvailableMiB {
			h.MinAvailableMiB = s.AvailMiB
		}
		h.MaxPSISomeAvg10 = max(h.MaxPSISomeAvg10, s.PSISome)
		if s.At > end && s.Live == 0 && h.TimeToZeroMS == nil {
			v := round2(float64(s.At-end) / float64(time.Millisecond))
			h.TimeToZeroMS = &v
		}
	}
	h.DistinctMachines = len(ids)
	return h
}
