package aigw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/guest"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// Handler expone la API. token vacío y sin tenants = sin autenticación, que
// solo es aceptable en el socket Unix (lo decide `kling ai serve`).
func (g *Gateway) Handler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /metrics", g.handleMetrics)
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", g.handleProxy)
	mux.HandleFunc("POST /v1/completions", g.handleProxy)
	mux.HandleFunc("POST /v1/classify", g.handleClassify("classify"))
	mux.HandleFunc("POST /v1/decide", g.handleClassify("decide"))
	mux.HandleFunc("POST /v1/generate", g.handleGenerate)
	mux.HandleFunc("GET /v1/tasks", g.handleTasks)
	mux.HandleFunc("POST /v1/admin/calibrate", g.admin(g.handleCalibrate))
	mux.HandleFunc("POST /v1/admin/reload", g.admin(g.handleReload))
	mux.HandleFunc("POST /v1/admin/eval", g.admin(g.handleEval))
	mux.HandleFunc("POST /v1/feedback", g.handleFeedback)
	mux.HandleFunc("POST /v1/admin/review", g.admin(g.handleReview))
	mux.HandleFunc("POST /v1/admin/retrain", g.admin(g.handleRetrain))
	mux.HandleFunc("POST /v1/admin/promote", g.admin(g.handlePromote))
	mux.HandleFunc("POST /v1/admin/rollback", g.admin(g.handleRollback))
	return g.sched.AuthHandler(g.limits(mux), token)
}

// limits es lo que vale para todas las rutas: cuerpo acotado, ninguna ruta de
// control del agente del invitado (el gateway no reenvía rutas arbitrarias,
// pero la comprobación es barata y no depende de que eso siga siendo así) y
// la cuota de peticiones en vuelo del tenant.
func (g *Gateway) limits(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if guest.IsControlPath(path.Clean(r.URL.Path)) {
			http.NotFound(w, r)
			return
		}
		limit := g.opts.MaxBody
		if r.URL.Path == "/v1/admin/eval" || r.URL.Path == "/v1/admin/retrain" {
			// Un conjunto etiquetado entero; solo el token principal llega a
			// leerlo (admin rechaza antes de tocar el cuerpo).
			limit = maxEvalBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			t := scheduler.TenantFrom(r.Context())
			if !g.sched.TenantBegin(t) {
				writeError(w, http.StatusTooManyRequests, "rate_limit",
					fmt.Sprintf("tenant %q has %d requests in flight (its quota); retry when they finish", t.Name(), t.MaxInflight()))
				return
			}
			defer g.sched.TenantEnd(t)
			g.met.addInflight(1)
			defer g.met.addInflight(-1)
		}
		h.ServeHTTP(w, r)
	})
}

// maxEvalBody acota el cuerpo de /v1/admin/eval: ~100k commits de ejemplo.
const maxEvalBody = 64 << 20

// admin deja pasar solo al tenant por defecto (el token principal, o el socket
// Unix): un token con nombre es para usar tareas, no para reescribir modelos.
func (g *Gateway) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if n := scheduler.TenantFrom(r.Context()).Name(); n != "default" {
			writeError(w, http.StatusForbidden, "forbidden", "admin endpoints need the gateway's main token")
			return
		}
		h(w, r)
	}
}

// writeError contesta con la forma de error de OpenAI, que es la que los
// clientes de /v1 saben leer.
func writeError(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	if code == http.StatusServiceUnavailable || code == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "2")
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decodeBody lee un JSON acotado (MaxBytesReader ya puso el tope) y estricto.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, err error) {
	var se *StatusError
	if errors.As(err, &se) {
		writeError(w, se.Code, errType(se.Code), se.Msg)
		return
	}
	writeError(w, http.StatusInternalServerError, "server_error", err.Error())
}

func errType(code int) string {
	switch {
	case code == http.StatusNotFound:
		return "not_found"
	case code >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

func (g *Gateway) handleClassify(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ClassifyRequest
		if !decodeBody(w, r, &req) {
			return
		}
		if tc := g.config().Tasks[req.Task]; endpoint == "decide" && tc != nil && tc.Domotica != nil {
			resp, err := g.Decide(r.Context(), req)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, resp)
			return
		}
		resp, err := g.Classify(r.Context(), endpoint, req)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, resp)
	}
}

func (g *Gateway) handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req GenerateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	resp, err := g.Generate(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, resp)
}

func (g *Gateway) handleEval(w http.ResponseWriter, r *http.Request) {
	var req EvalRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if tc := g.config().Tasks[req.Task]; tc != nil && tc.Domotica != nil {
		rec, err := g.evalDomotica(r.Context(), req)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"record": rec, "cascade": g.cascade(req.Task)})
		return
	}
	rec, err := g.Eval(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"record": rec, "cascade": g.cascade(req.Task)})
}

// handleModels lista los modelos VON del registro sin despertar nada: una
// consulta de catálogo no debe costar un thaw.
func (g *Gateway) handleModels(w http.ResponseWriter, _ *http.Request) {
	cfg := g.config()
	type model struct {
		ID       string `json:"id"`
		Object   string `json:"object"`
		Created  int64  `json:"created"`
		OwnedBy  string `json:"owned_by"`
		Snapshot string `json:"snapshot"`
	}
	data := []model{}
	for _, n := range sortedKeys(cfg.Models) {
		if m := cfg.Models[n]; m.Kind == KindVON {
			data = append(data, model{ID: n, Object: "model", OwnedBy: "kindling", Snapshot: m.Snapshot})
		}
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

// TaskInfo es una tarea tal como la ve `kling ai ls`.
type TaskInfo struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // classify | generate | domotica
	Chispa string `json:"chispa,omitempty"`
	// ChispaBackend es dónde vive ese modelo: inprocess o microvm.
	ChispaBackend string        `json:"chispa_backend,omitempty"`
	VON           string        `json:"von,omitempty"` // el de una generación
	Cascade       *CascadeState `json:"cascade,omitempty"`
	Labels        []string      `json:"labels,omitempty"`
	Thresholds    []float64     `json:"thresholds,omitempty"` // los efectivos, si el modelo está cargado
	Loaded        bool          `json:"chispa_loaded"`
	Audit         float64       `json:"audit,omitempty"`
	Samples       int           `json:"samples"`
	Learn         *LearnInfo    `json:"learn,omitempty"`
}

func (g *Gateway) handleTasks(w http.ResponseWriter, _ *http.Request) {
	cfg := g.config()
	out := []TaskInfo{}
	for _, n := range sortedKeys(cfg.Tasks) {
		t := cfg.Tasks[n]
		ti := TaskInfo{Name: n, Kind: "classify", Chispa: t.Chispa, VON: t.VON, Labels: t.Labels, Audit: t.Audit}
		if t.Domotica != nil {
			ti.Kind, ti.Chispa = "domotica", t.Domotica.Intent
			c := g.cascade(n)
			ti.Cascade = &c
		} else if t.IsGenerate() {
			ti.Kind = "generate"
		} else {
			c := g.cascade(n)
			ti.Cascade = &c
		}
		if t.Chispa != "" {
			// Solo si ya está cargado: listar no carga modelos.
			g.chispa.mu.Lock()
			if el, ok := g.chispa.items[cfg.Models[t.Chispa].Path]; ok {
				m := el.Value.(*chispaItem).model
				ti.Loaded, ti.Labels, ti.Thresholds = true, m.Labels, effective(m, t.Thresholds)
			}
			g.chispa.mu.Unlock()
		}
		g.cfgMu.RLock()
		if r := g.rings[n]; r != nil {
			ti.Samples = r.len()
		}
		g.cfgMu.RUnlock()
		if m := cfg.Models[ti.Chispa]; m != nil {
			ti.ChispaBackend = BackendInProcess
			if m.Backend == BackendMicroVM {
				ti.ChispaBackend = BackendMicroVM
			}
		}
		ti.Learn = g.learnInfo(n)
		out = append(out, ti)
	}
	writeJSON(w, map[string]any{"tasks": out})
}

func (g *Gateway) handleCalibrate(w http.ResponseWriter, r *http.Request) {
	var req CalibrateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	rep, err := g.Calibrate(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, rep)
}

func (g *Gateway) handleReload(w http.ResponseWriter, _ *http.Request) {
	notes, err := g.Reload()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, map[string]any{"reloaded": true, "cascades": notes})
}

func (g *Gateway) handleMetrics(w http.ResponseWriter, r *http.Request) {
	samples := map[string]int{}
	g.cfgMu.RLock()
	for n, rg := range g.rings {
		samples[n] = rg.len()
	}
	g.cfgMu.RUnlock()
	reps := g.replicaCounts(r.Context())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	g.met.write(w, g.chispa.stats(), samples, reps, g.learnSnapshot())
}

// replicaCounts cuenta las réplicas de cada modelo por estado, preguntando al
// daemon como mucho cada 5 s: List recorre el disco de todas las máquinas, y
// un Prometheus que raspa cada segundo no debe notarse.
func (g *Gateway) replicaCounts(ctx context.Context) map[string]replicaCount {
	g.repMu.Lock()
	defer g.repMu.Unlock()
	if g.opts.Client == nil || time.Since(g.repAt) < 5*time.Second {
		return g.repCache
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ms, err := g.opts.Client.List(ctx)
	if err != nil {
		return g.repCache
	}
	cfg := g.config()
	out := map[string]replicaCount{}
	for _, n := range sortedKeys(cfg.Models) {
		if cfg.Models[n].Kind == KindVON {
			out[n] = replicaCount{}
		}
	}
	for _, m := range ms {
		if m.Labels[LabelGateway] != g.opts.ID {
			continue
		}
		name, _ := cfg.vonModel(m.Service())
		if name == "" {
			continue
		}
		c := out[name]
		switch m.State {
		case "running":
			c.running++
		case "warm":
			c.warm++
		}
		out[name] = c
	}
	g.repCache, g.repAt = out, time.Now()
	return out
}

// ---- proxy OpenAI

// handleProxy reenvía una petición OpenAI a una réplica del modelo que nombra
// "model", con streaming. El cuerpo ya viene acotado; lo que vuelve del
// invitado se acota en bytes y en tiempo, y solo se copian las cabeceras que
// un cliente necesita (el invitado no decide cookies ni cabeceras propias).
func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body too large")
		return
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	var model string
	if raw, ok := req["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	cfg := g.config()
	name, mc := cfg.vonModel(model)
	if mc == nil && model == "" {
		// Sin "model" y con un solo modelo VON registrado, es ese.
		var vons []string
		for _, n := range sortedKeys(cfg.Models) {
			if cfg.Models[n].Kind == KindVON {
				vons = append(vons, n)
			}
		}
		if len(vons) == 1 {
			name, mc = vons[0], cfg.Models[vons[0]]
		}
	}
	if mc == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("model %q is not a von model of this gateway (see GET /v1/models)", model))
		return
	}
	if _, ok := req["seed"]; !ok {
		req["seed"] = json.RawMessage(strconv.FormatInt(seed(), 10))
	}
	stream := string(req["stream"]) == "true"
	out, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.opts.ProxyTimeout)
	defer cancel()
	resp, rep, err := g.postGuest(ctx, mc.Snapshot, r.URL.Path, out)
	if err != nil {
		var we *wakeError
		if errors.As(err, &we) {
			g.met.vonErr(name, "wake")
			g.met.inc(g.met.proxy, name, "503")
			writeError(w, http.StatusServiceUnavailable, "server_error", err.Error())
			return
		}
		g.met.vonErr(name, "request")
		g.met.inc(g.met.proxy, name, "502")
		writeError(w, http.StatusBadGateway, "server_error", "replica did not answer: "+err.Error())
		return
	}
	defer rep.Release()
	defer resp.Body.Close()

	code := resp.StatusCode
	if code < 200 || code > 599 {
		code = http.StatusBadGateway
	}
	g.met.inc(g.met.proxy, name, strconv.Itoa(code))
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	if stream {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(code)

	// Copia acotada. Con streaming se vacía el búfer tras cada lectura para
	// que cada token llegue cuando sale; sin él, al final.
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	var n int64
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			if n+int64(k) > g.opts.MaxProxyBytes {
				log.Printf("proxy %s: replica answer over %d bytes, cut", name, g.opts.MaxProxyBytes)
				g.met.vonErr(name, "too_large")
				return
			}
			n += int64(k)
			if _, werr := w.Write(buf[:k]); werr != nil {
				return // el cliente se fue; el contexto cancela la réplica
			}
			if stream && fl != nil {
				fl.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				g.met.vonErr(name, "stream")
			}
			return
		}
	}
}
