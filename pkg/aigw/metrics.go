package aigw

import (
	"fmt"
	"io"
	"math"
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

// latencyBuckets van de 10 µs (Chispa) a 60 s (un arranque en frío con carga).
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
	// Mejora continua (learn.go): respuestas por versión de Chispa (confiadas y
	// totales, desde que el proceso la sirve) y una ventana por minutos.
	learnVer map[string]*[2]uint64 // task|version
	learnWin map[string]*winRing   // task
}

// winRing cuenta respuestas por minuto en una ventana deslizante: la tasa de
// escalado «desde el arranque» no enseña si el último reentreno sirvió.
type winRing struct {
	min  []int64 // minuto (Unix/60) de cada cubo
	conf []uint64
	tot  []uint64
}

func newWinRing(n int) *winRing {
	return &winRing{min: make([]int64, n), conf: make([]uint64, n), tot: make([]uint64, n)}
}

func (w *winRing) add(now int64, confident bool) {
	i := int(now % int64(len(w.min)))
	if w.min[i] != now {
		w.min[i], w.conf[i], w.tot[i] = now, 0, 0
	}
	w.tot[i]++
	if confident {
		w.conf[i]++
	}
}

func (w *winRing) sum(now int64) (conf, tot uint64) {
	for i, m := range w.min {
		if m > now-int64(len(w.min)) && m <= now {
			conf += w.conf[i]
			tot += w.tot[i]
		}
	}
	return
}

func newMetrics() *metrics {
	return &metrics{
		requests: map[string]uint64{}, latency: map[string]*histogram{}, unknown: map[string]uint64{},
		degraded: map[string]uint64{}, audits: map[string]uint64{}, vonErrors: map[string]uint64{},
		wakes: map[string]*histogram{}, proxy: map[string]uint64{},
		learnVer: map[string]*[2]uint64{}, learnWin: map[string]*winRing{},
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

// learnAnswer cuenta una clasificación de una tarea con "learn".
func (m *metrics) learnAnswer(task, version string, confident bool, window int) {
	if version == "" {
		version = "unversioned"
	}
	now := time.Now().Unix() / 60
	m.mu.Lock()
	c := m.learnVer[key(task, version)]
	if c == nil {
		c = &[2]uint64{}
		m.learnVer[key(task, version)] = c
	}
	c[1]++
	if confident {
		c[0]++
	}
	w := m.learnWin[task]
	if w == nil || len(w.min) != window {
		w = newWinRing(max(window, 1))
		m.learnWin[task] = w
	}
	w.add(now, confident)
	m.mu.Unlock()
}

// learnSnap es lo que /metrics necesita del almacén de la mejora continua,
// copiado fuera del candado de las métricas.
type learnSnap struct {
	counts  map[string]uint64 // task|outcome
	heldout []heldoutGauge
	current map[string]heldoutGauge // task -> versión servida
	base    map[string]heldoutGauge // task -> v1
	escMS   map[string]float64      // task -> coste de una escalada (0 = medido)
}

type heldoutGauge struct {
	task, version       string
	coverage, precision float64
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
func (m *metrics) write(w io.Writer, js chispaStats, samples map[string]int, replicas map[string]replicaCount, ls learnSnap) {
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

	counter("kling_ai_requests_total", "Answers by endpoint, task and source: chispa (confident), escalated (Chispa unsure, answered by Chispa with escalate: true) or von (the cascade, or a generation).", m.requests, "endpoint", "task", "source")

	// Cobertura y escalado por tarea de clasificación, derivados de los
	// contadores: lo que Chispa contesta seguro y lo que duda se lee de un
	// vistazo sin escribir PromQL. Las generaciones no cuentan: no hay Chispa.
	chispaN, vonN := map[string]uint64{}, map[string]uint64{}
	for k, v := range m.requests {
		p := strings.Split(k, "|")
		if p[0] == "generate" {
			continue
		}
		switch p[2] {
		case "chispa":
			chispaN[p[1]] += v
		case "von", "escalated":
			vonN[p[1]] += v
		}
	}
	tasks := map[string]bool{}
	for t := range chispaN {
		tasks[t] = true
	}
	for t := range vonN {
		tasks[t] = true
	}
	fmt.Fprintf(w, "# HELP kling_ai_chispa_coverage Fraction of classifications Chispa answered confidently (since start).\n# TYPE kling_ai_chispa_coverage gauge\n")
	for _, t := range sortedKeys(tasks) {
		fmt.Fprintf(w, "kling_ai_chispa_coverage{task=%q} %g\n", t, float64(chispaN[t])/float64(chispaN[t]+vonN[t]))
	}
	fmt.Fprintf(w, "# HELP kling_ai_escalation_rate Fraction of classifications where Chispa was unsure: sent to VON by an active cascade, or returned with escalate: true (since start).\n# TYPE kling_ai_escalation_rate gauge\n")
	for _, t := range sortedKeys(tasks) {
		fmt.Fprintf(w, "kling_ai_escalation_rate{task=%q} %g\n", t, float64(vonN[t])/float64(chispaN[t]+vonN[t]))
	}

	hist("kling_ai_latency_seconds", "End-to-end latency by task and source.", m.latency, "task", "source")
	counter("kling_ai_von_unknown_total", "VON answers that were not exactly one of the labels.", m.unknown, "task")
	counter("kling_ai_degraded_total", "Escalations answered by Chispa because VON failed.", m.degraded, "task")
	counter("kling_ai_audits_total", "Confident Chispa answers double-checked by VON in the background.", m.audits, "task", "outcome")
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
	m.writeLearn(w, ls)
	fmt.Fprintf(w, "# TYPE kling_ai_inflight gauge\nkling_ai_inflight %d\n", m.inflight)
	fmt.Fprintf(w, "# TYPE kling_ai_chispa_models_loaded gauge\nkling_ai_chispa_models_loaded %d\n", js.Loaded)
	fmt.Fprintf(w, "# TYPE kling_ai_chispa_bytes gauge\nkling_ai_chispa_bytes %d\n", js.Bytes)
	fmt.Fprintf(w, "# TYPE kling_ai_chispa_loads_total counter\nkling_ai_chispa_loads_total %d\n", js.Loads)
	fmt.Fprintf(w, "# TYPE kling_ai_chispa_evictions_total counter\nkling_ai_chispa_evictions_total %d\n", js.Evictions)
	fmt.Fprintf(w, "# TYPE kling_ai_chispa_load_failures_total counter\nkling_ai_chispa_load_failures_total %d\n", js.Failures)
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

// writeLearn vuelca las métricas del bucle de mejora continua: lo que enseña
// si funciona es la cobertura por versión subiendo, la tasa de escalado de la
// ventana bajando y la precisión en el conjunto de confianza plana.
func (m *metrics) writeLearn(w io.Writer, ls learnSnap) {
	if len(m.learnVer) == 0 && len(ls.heldout) == 0 && len(ls.counts) == 0 {
		return
	}
	fmt.Fprintf(w, "# HELP kling_ai_chispa_version_coverage Fraction of classifications each Chispa version answered confidently (live, since this process served it).\n# TYPE kling_ai_chispa_version_coverage gauge\n")
	for _, k := range sortedKeys(m.learnVer) {
		c := m.learnVer[k]
		fmt.Fprintf(w, "kling_ai_chispa_version_coverage{%s} %g\n", labelPairs([]string{"task", "version"}, k), float64(c[0])/float64(max(c[1], 1)))
	}
	now := time.Now().Unix() / 60
	fmt.Fprintf(w, "# HELP kling_ai_escalation_rate_window Fraction of classifications where Chispa was unsure, over the task's learn window.\n# TYPE kling_ai_escalation_rate_window gauge\n")
	for _, t := range sortedKeys(m.learnWin) {
		conf, tot := m.learnWin[t].sum(now)
		if tot > 0 {
			fmt.Fprintf(w, "kling_ai_escalation_rate_window{task=%q} %g\n", t, 1-float64(conf)/float64(tot))
		}
	}
	fmt.Fprintf(w, "# HELP kling_ai_requests_window Classifications over the task's learn window.\n# TYPE kling_ai_requests_window gauge\n")
	for _, t := range sortedKeys(m.learnWin) {
		_, tot := m.learnWin[t].sum(now)
		fmt.Fprintf(w, "kling_ai_requests_window{task=%q} %d\n", t, tot)
	}
	fmt.Fprintf(w, "# HELP kling_ai_heldout_coverage Coverage of each Chispa version on the task's trusted held-out set.\n# TYPE kling_ai_heldout_coverage gauge\n")
	for _, h := range ls.heldout {
		fmt.Fprintf(w, "kling_ai_heldout_coverage{task=%q,version=%q} %g\n", h.task, h.version, h.coverage)
	}
	fmt.Fprintf(w, "# HELP kling_ai_heldout_precision Precision of each Chispa version's confident answers on the task's trusted held-out set.\n# TYPE kling_ai_heldout_precision gauge\n")
	for _, h := range ls.heldout {
		fmt.Fprintf(w, "kling_ai_heldout_precision{task=%q,version=%q} %g\n", h.task, h.version, h.precision)
	}
	// Estimación, y dicha como tal: escaladas de la ventana que la versión
	// servida contesta y la v1 no, según su cobertura en el conjunto de
	// confianza; por lo que cuesta una escalada (escalation_ms, o la latencia
	// media medida de VON en la tarea).
	fmt.Fprintf(w, "# HELP kling_ai_learn_escalations_avoided_estimate Escalations in the window the served version avoids compared with v1 (held-out coverage difference times window requests).\n# TYPE kling_ai_learn_escalations_avoided_estimate gauge\n")
	type saved struct {
		task    string
		avoided float64
		secs    float64
		hasCost bool
	}
	var out []saved
	for _, t := range sortedKeys(ls.current) {
		cur, base := ls.current[t], ls.base[t]
		win := m.learnWin[t]
		if win == nil {
			continue
		}
		_, tot := win.sum(now)
		av := float64(tot) * math.Max(0, cur.coverage-base.coverage)
		ms := ls.escMS[t]
		if ms == 0 {
			if h := m.latency[key(t, "von")]; h != nil && h.n > 0 {
				ms = h.sum / float64(h.n) * 1000
			}
		}
		out = append(out, saved{t, av, av * ms / 1000, ms > 0})
		fmt.Fprintf(w, "kling_ai_learn_escalations_avoided_estimate{task=%q} %g\n", t, av)
	}
	fmt.Fprintf(w, "# HELP kling_ai_learn_seconds_saved_estimate Escalation time the served version saves in the window (avoided escalations times the cost of one).\n# TYPE kling_ai_learn_seconds_saved_estimate gauge\n")
	for _, s := range out {
		if s.hasCost {
			fmt.Fprintf(w, "kling_ai_learn_seconds_saved_estimate{task=%q} %g\n", s.task, s.secs)
		}
	}
	counter := func(name, help string, mp map[string]uint64, labels ...string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
		for _, k := range sortedKeys(mp) {
			fmt.Fprintf(w, "%s{%s} %d\n", name, labelPairs(labels, k), mp[k])
		}
	}
	counter("kling_ai_learn_captures_total", "Escalations captured for learning by outcome: written, dedup, cap (store full), queue_full, votes_dropped, error.", ls.counts, "task", "outcome")
}
