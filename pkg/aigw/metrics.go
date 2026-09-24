package aigw

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Métricas en texto de Prometheus, escritas a mano: el núcleo no tiene
// dependencias y un puñado de contadores e histogramas no las justifica.
//
// Las etiquetas salen del registro (nombres de tarea y modelo, validados con
// nameRE) y de un conjunto cerrado (source, how, reason): un cliente no puede
// crear series nuevas mandando nombres inventados, porque una tarea que no
// existe se rechaza antes de contar nada.

// latencyBuckets van de 10 µs (JEV) a 60 s (un arranque en frío con carga).
var latencyBuckets = []float64{1e-5, 2.5e-5, 5e-5, 1e-4, 2.5e-4, 5e-4, 1e-3, 5e-3, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type histogram struct {
	counts []uint64 // por cubo (no acumulado); el último es +Inf
	sum    float64
	n      uint64
}

func (h *histogram) observe(v float64) {
	if h.counts == nil {
		h.counts = make([]uint64, len(latencyBuckets)+1)
	}
	i := sort.SearchFloat64s(latencyBuckets, v)
	h.counts[i]++
	h.sum += v
	h.n++
}

type metrics struct {
	mu        sync.Mutex
	requests  map[string]uint64     // endpoint|task|source
	latency   map[string]*histogram // task|source
	unknown   map[string]uint64     // task
	degraded  map[string]uint64     // task
	audits    map[string]uint64     // task|outcome (sent, dropped)
	vonErrors map[string]uint64     // model|reason
	wakes     map[string]*histogram // model|how
	proxy     map[string]uint64     // model|code
	inflight  int64
}

func newMetrics() *metrics {
	return &metrics{
		requests: map[string]uint64{}, latency: map[string]*histogram{}, unknown: map[string]uint64{},
		degraded: map[string]uint64{}, audits: map[string]uint64{}, vonErrors: map[string]uint64{},
		wakes: map[string]*histogram{}, proxy: map[string]uint64{},
	}
}

func key(parts ...string) string { return strings.Join(parts, "|") }

func (m *metrics) answer(endpoint, task, source string, d time.Duration) {
	m.mu.Lock()
	m.requests[key(endpoint, task, source)]++
	h := m.latency[key(task, source)]
	if h == nil {
		h = &histogram{}
		m.latency[key(task, source)] = h
	}
	h.observe(d.Seconds())
	m.mu.Unlock()
}

func (m *metrics) inc(mp map[string]uint64, parts ...string) {
	m.mu.Lock()
	mp[key(parts...)]++
	m.mu.Unlock()
}

func (m *metrics) vonErr(model, reason string) { m.inc(m.vonErrors, model, reason) }

func (m *metrics) wake(model, how string, d time.Duration) {
	m.mu.Lock()
	h := m.wakes[key(model, how)]
	if h == nil {
		h = &histogram{}
		m.wakes[key(model, how)] = h
	}
	h.observe(d.Seconds())
	m.mu.Unlock()
}

func (m *metrics) addInflight(d int64) {
	m.mu.Lock()
	m.inflight += d
	m.mu.Unlock()
}

// replicaCount es cuántas réplicas de un modelo hay en cada estado.
type replicaCount struct{ running, warm int }

// write vuelca todo. replicas y extra los calcula quien llama (sin el candado
// de las métricas: preguntar al daemon no puede ocurrir con él tomado).
func (m *metrics) write(w io.Writer, js jevStats, samples map[string]int, replicas map[string]replicaCount) {
	m.mu.Lock()
	defer m.mu.Unlock()

	counter := func(name, help string, mp map[string]uint64, labels ...string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
		for _, k := range sortedKeys(mp) {
			fmt.Fprintf(w, "%s{%s} %d\n", name, labelPairs(labels, k), mp[k])
		}
	}
	hist := func(name, help string, mp map[string]*histogram, labels ...string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
		for _, k := range sortedKeys(mp) {
			h, lp := mp[k], labelPairs(labels, k)
			var acc uint64
			for i, le := range latencyBuckets {
				acc += h.counts[i]
				fmt.Fprintf(w, "%s_bucket{%s,le=\"%s\"} %d\n", name, lp, strconv.FormatFloat(le, 'g', -1, 64), acc)
			}
			fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %d\n", name, lp, h.n)
			fmt.Fprintf(w, "%s_sum{%s} %g\n%s_count{%s} %d\n", name, lp, h.sum, name, lp, h.n)
		}
	}

	counter("kling_ai_requests_total", "Answers by endpoint, task and source: jev (confident), escalated (JEV unsure, answered by JEV with escalate: true) or von (the cascade, or a generation).", m.requests, "endpoint", "task", "source")

	// Cobertura y escalado por tarea de clasificación, derivados de los
	// contadores: lo que JEV contesta seguro y lo que duda se lee de un
	// vistazo sin escribir PromQL. Las generaciones no cuentan: no hay JEV.
	jevN, vonN := map[string]uint64{}, map[string]uint64{}
	for k, v := range m.requests {
		p := strings.Split(k, "|")
		if p[0] == "generate" {
			continue
		}
		switch p[2] {
		case "jev":
			jevN[p[1]] += v
		case "von", "escalated":
			vonN[p[1]] += v
		}
	}
	tasks := map[string]bool{}
	for t := range jevN {
		tasks[t] = true
	}
	for t := range vonN {
		tasks[t] = true
	}
	fmt.Fprintf(w, "# HELP kling_ai_jev_coverage Fraction of classifications JEV answered confidently (since start).\n# TYPE kling_ai_jev_coverage gauge\n")
	for _, t := range sortedKeys(tasks) {
		fmt.Fprintf(w, "kling_ai_jev_coverage{task=%q} %g\n", t, float64(jevN[t])/float64(jevN[t]+vonN[t]))
	}
	fmt.Fprintf(w, "# HELP kling_ai_escalation_rate Fraction of classifications where JEV was unsure: sent to VON by an active cascade, or returned with escalate: true (since start).\n# TYPE kling_ai_escalation_rate gauge\n")
	for _, t := range sortedKeys(tasks) {
		fmt.Fprintf(w, "kling_ai_escalation_rate{task=%q} %g\n", t, float64(vonN[t])/float64(jevN[t]+vonN[t]))
	}

	hist("kling_ai_latency_seconds", "End-to-end latency by task and source.", m.latency, "task", "source")
	counter("kling_ai_von_unknown_total", "VON answers that were not exactly one of the labels.", m.unknown, "task")
	counter("kling_ai_degraded_total", "Escalations answered by JEV because VON failed.", m.degraded, "task")
	counter("kling_ai_audits_total", "Confident JEV answers double-checked by VON in the background.", m.audits, "task", "outcome")
	counter("kling_ai_von_errors_total", "Errors talking to VON replicas.", m.vonErrors, "model", "reason")
	hist("kling_ai_von_wake_seconds", "Time to get a replica ready: thaw (was frozen), restore (cold start from the golden snapshot) or adopt.", m.wakes, "model", "how")
	counter("kling_ai_proxy_requests_total", "OpenAI-compatible requests proxied to VON, by status code.", m.proxy, "model", "code")

	fmt.Fprintf(w, "# HELP kling_ai_von_replicas VON replicas of this gateway by state.\n# TYPE kling_ai_von_replicas gauge\n")
	for _, mo := range sortedKeys(replicas) {
		fmt.Fprintf(w, "kling_ai_von_replicas{model=%q,state=\"running\"} %d\n", mo, replicas[mo].running)
		fmt.Fprintf(w, "kling_ai_von_replicas{model=%q,state=\"warm\"} %d\n", mo, replicas[mo].warm)
	}
	fmt.Fprintf(w, "# HELP kling_ai_samples Recorded VON answers available for recalibration.\n# TYPE kling_ai_samples gauge\n")
	for _, t := range sortedKeys(samples) {
		fmt.Fprintf(w, "kling_ai_samples{task=%q} %d\n", t, samples[t])
	}
	fmt.Fprintf(w, "# TYPE kling_ai_inflight gauge\nkling_ai_inflight %d\n", m.inflight)
	fmt.Fprintf(w, "# TYPE kling_ai_jev_models_loaded gauge\nkling_ai_jev_models_loaded %d\n", js.Loaded)
	fmt.Fprintf(w, "# TYPE kling_ai_jev_bytes gauge\nkling_ai_jev_bytes %d\n", js.Bytes)
	fmt.Fprintf(w, "# TYPE kling_ai_jev_loads_total counter\nkling_ai_jev_loads_total %d\n", js.Loads)
	fmt.Fprintf(w, "# TYPE kling_ai_jev_evictions_total counter\nkling_ai_jev_evictions_total %d\n", js.Evictions)
	fmt.Fprintf(w, "# TYPE kling_ai_jev_load_failures_total counter\nkling_ai_jev_load_failures_total %d\n", js.Failures)
}

func labelPairs(names []string, k string) string {
	vals := strings.Split(k, "|")
	ps := make([]string, len(names))
	for i, n := range names {
		v := ""
		if i < len(vals) {
			v = vals[i]
		}
		ps[i] = fmt.Sprintf("%s=%q", n, v)
	}
	return strings.Join(ps, ",")
}
