package aigw

import (
	"net/http"
	"time"

	"github.com/juan52878911/kindling/pkg/scheduler"
)

// Rutas de la mejora continua. /v1/feedback está abierta a cualquier token
// válido, pero solo el principal habla como persona (la verdad); un tenant
// solo aporta votos de maestro, que nunca entrenan sin validarse. El resto
// reescribe modelos: solo el token principal (admin).

func (g *Gateway) handleFeedback(w http.ResponseWriter, r *http.Request) {
	var req FeedbackRequest
	if !decodeBody(w, r, &req) {
		return
	}
	t := scheduler.TenantFrom(r.Context()).Name()
	resp, err := g.Feedback(req, t == "default", t)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, resp)
}

func (g *Gateway) handleReview(w http.ResponseWriter, r *http.Request) {
	var req ReviewRequest
	if !decodeBody(w, r, &req) {
		return
	}
	resp, err := g.Review(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, resp)
}

func (g *Gateway) handleRetrain(w http.ResponseWriter, r *http.Request) {
	var req RetrainRequest
	if !decodeBody(w, r, &req) {
		return
	}
	rep, err := g.Retrain(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, rep)
}

func (g *Gateway) handlePromote(w http.ResponseWriter, r *http.Request) {
	var req PromoteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	out, err := g.Promote(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, out)
}

func (g *Gateway) handleRollback(w http.ResponseWriter, r *http.Request) {
	var req RollbackRequest
	if !decodeBody(w, r, &req) {
		return
	}
	out, err := g.Rollback(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, out)
}

// LearnInfo es la mejora continua de una tarea en /v1/tasks y `kling ai ls`.
type LearnInfo struct {
	Capture  string         `json:"capture"`
	Version  string         `json:"version,omitempty"`
	Versions []VersionEntry `json:"versions,omitempty"`
	Pending  *VersionEntry  `json:"pending_version,omitempty"`
	Captured uint64         `json:"captured"`
	// PendingReview es del último review o retrain de este proceso (-1: no
	// se ha calculado; contarlo exige leer el almacén entero).
	PendingReview        int             `json:"pending_review"`
	LastRetrain          *RetrainSummary `json:"last_retrain,omitempty"`
	WindowMinutes        int             `json:"window_minutes"`
	WindowRequests       uint64          `json:"window_requests"`
	WindowEscalationRate float64         `json:"window_escalation_rate"`
}

func (g *Gateway) learnInfo(task string) *LearnInfo {
	st, ok := g.learnFor(task)
	if !ok {
		return nil
	}
	li := &LearnInfo{Capture: st.cfg.Capture, Version: st.version, PendingReview: -1, WindowMinutes: st.cfg.WindowMinutes}
	if li.Capture == "" {
		li.Capture = CaptureOff
	}
	g.learn.mu.Lock()
	if man := g.learn.manifests[task]; man != nil {
		li.Versions = append([]VersionEntry(nil), man.Versions...)
		li.Pending = man.Pending
	}
	li.Captured = g.learn.counts[task+"|written"]
	if n, ok := g.learn.pending[task]; ok {
		li.PendingReview = n
	}
	li.LastRetrain = g.learn.lastRun[task]
	g.learn.mu.Unlock()
	g.met.mu.Lock()
	if w := g.met.learnWin[task]; w != nil {
		conf, tot := w.sum(time.Now().Unix() / 60)
		li.WindowRequests = tot
		if tot > 0 {
			li.WindowEscalationRate = ratio(int(tot-conf), int(tot))
		}
	}
	g.met.mu.Unlock()
	return li
}

// learnSnapshot copia lo que /metrics necesita del almacén.
func (g *Gateway) learnSnapshot() learnSnap {
	s := learnSnap{counts: map[string]uint64{}, current: map[string]heldoutGauge{}, base: map[string]heldoutGauge{}, escMS: map[string]float64{}}
	g.cfgMu.RLock()
	states := make(map[string]learnState, len(g.learnStates))
	for k, v := range g.learnStates {
		states[k] = v
	}
	g.cfgMu.RUnlock()
	g.learn.mu.Lock()
	defer g.learn.mu.Unlock()
	for k, v := range g.learn.counts {
		s.counts[k] = v
	}
	for _, task := range sortedKeys(states) {
		st := states[task]
		s.escMS[task] = st.cfg.EscalationMS
		man := g.learn.manifests[task]
		if man == nil {
			continue
		}
		for _, e := range man.Versions {
			if e.Heldout == nil {
				continue
			}
			h := heldoutGauge{task: task, version: vLabel(e.N), coverage: e.Heldout.Coverage, precision: e.Heldout.ConfidentPrecision}
			s.heldout = append(s.heldout, h)
			if e.N == 1 {
				s.base[task] = h
			}
			if h.version == st.version {
				s.current[task] = h
			}
		}
	}
	return s
}
