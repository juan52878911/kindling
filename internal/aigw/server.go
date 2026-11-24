package aigw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// ServeHandler es Handler para `kling ai serve`: en TCP sin autenticación
// (-no-auth, solo en loopback) atiende solo peticiones dirigidas a loopback y
// de un origen de loopback (scheduler.SoloLoopback), porque escuchar en
// loopback no basta frente a una página abierta en el navegador. En el socket
// Unix no hace falta, y el Host que mandan los clientes no es de loopback.
//
// En el socket Unix sin token, el modo sin autenticación del arranque se
// conserva para el dueño del socket aunque un reload traiga inquilinos: una
// petición sin Authorization entra como el dueño (lo protege el permiso del
// socket), y una con token se comprueba contra los inquilinos como siempre.
func (g *Gateway) ServeHandler(token string, tcp bool) http.Handler {
	rutas := g.rutas()
	h := g.sched.AuthHandler(rutas, token)
	if !g.sched.AuthDisabled(token) {
		return h
	}
	if tcp {
		return scheduler.SoloLoopback(h)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			rutas.ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Handler expone la API. token vacío y sin tenants = sin autenticación, que
// solo es aceptable en el socket Unix (lo decide `kling ai serve`).
func (g *Gateway) Handler(token string) http.Handler {
	return g.sched.AuthHandler(g.rutas(), token)
}

// rutas es la API sin autenticación: las rutas con sus topes (limits).
func (g *Gateway) rutas() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})
	// /metrics enseña nombres y volúmenes de todas las tareas y tenants: es del
	// operador (token principal), no de un inquilino.
	mux.HandleFunc("GET /metrics", g.admin(g.handleMetrics))
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", g.handleProxy)
	mux.HandleFunc("POST /v1/completions", g.handleProxy)
	mux.HandleFunc("POST /v1/embeddings", g.handleEmbeddings)
	mux.HandleFunc("POST /v1/m/{modelo}/{ruta...}", g.handleModelo)
	mux.HandleFunc("GET /v1/m/{modelo}/{ruta...}", g.handleModelo)
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
	g.alServir()
	return g.limits(mux)
}

// alServir es lo que toca al empezar a servir un registro (al montar el
// handler): avisar en el log de la frontera de las réplicas y lanzar la sonda
// de carga de los upstreams. Devuelve los avisos.
func (g *Gateway) alServir() []string {
	notas := g.avisosAlServir()
	for _, n := range notas {
		log.Print("warning: " + n)
	}
	return notas
}

// avisosAlServir es alServir sin el log: Reload (SIGHUP y /v1/admin/reload)
// devuelve los avisos a quien recarga, que los imprime él.
func (g *Gateway) avisosAlServir() []string {
	notas := g.config().BackendNotes(runtime.GOOS)
	g.prepararUpstreams()
	return notas
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
		// Todos los POST llevan JSON. Exigirlo deja fuera los envíos que un
		// navegador hace sin preguntar (formularios, text/plain).
		if r.Method == http.MethodPost {
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "invalid_request_error", "send the body as Content-Type: application/json")
				return
			}
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

// plazoEscritura: lo que puede tardar cada escritura de la respuesta del
// proxy al cliente. Sin él, un cliente que deja de leer retiene la réplica
// (y su sitio en la cuota) hasta que se cierre la conexión.
var plazoEscritura = 30 * time.Second

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

// plazoClasificar es el techo de un classify/decide: la escalada a VON
// (VONTimeout) más margen para despertar la réplica y clasificar en ella
// (readyTimeout del planificador: 20 s en frío).
func (g *Gateway) plazoClasificar() time.Duration {
	return g.opts.VONTimeout + 30*time.Second
}

func (g *Gateway) handleClassify(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ClassifyRequest
		if !decodeBody(w, r, &req) {
			return
		}
		// Techo de la petición entera: clasificar en la réplica (despertarla
		// incluido) no tenía plazo, solo la escalada a VON. Un invitado que
		// acepta y no contesta retenía al cliente hasta que este se rindiera.
		ctx, cancel := context.WithTimeout(r.Context(), g.plazoClasificar())
		defer cancel()
		if tc := g.config().Tasks[req.Task]; endpoint == "decide" && tc != nil && tc.Intent != nil {
			resp, err := g.Decide(ctx, req)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, resp)
			return
		}
		resp, err := g.Classify(ctx, endpoint, req)
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
	if tc := g.config().Tasks[req.Task]; tc != nil && tc.Intent != nil {
		rec, err := g.evalIntent(r.Context(), req)
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

// handleModels lista los modelos de contrato openai y openai-embeddings (de
// dorado o de upstream) que el token puede usar, sin despertar nada: una
// consulta de catálogo no debe costar un thaw. Los de otros contratos van por
// /v1/m/{modelo}/… y no son modelos de OpenAI.
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID       string `json:"id"`
		Object   string `json:"object"`
		Created  int64  `json:"created"`
		OwnedBy  string `json:"owned_by"`
		Snapshot string `json:"snapshot,omitempty"`
	}
	tenant := scheduler.TenantFrom(r.Context()).Name()
	data := []model{}
	for _, b := range g.catalogo().lista {
		if (b.Contract == ContractOpenAI || b.Contract == ContractOpenAIEmbeddings) && permitido(b, tenant) {
			data = append(data, model{ID: b.Name, Object: "model", OwnedBy: "kindling", Snapshot: b.Snapshot})
		}
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

// TaskInfo es una tarea tal como la ve `kling ai ls`.
type TaskInfo struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // classify | generate | intent
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
		if t.Intent != nil {
			ti.Kind, ti.Chispa = "intent", t.Intent.Model
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
	// "cascades" es la lista de avisos que imprime `kling ai reload`; Reload
	// ya trae los de alServir.
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

// labelSpare es la etiqueta con la que el planificador marca un repuesto
// (ronda 9, C9-08). Es el mismo texto que pone Z4; en la integración debe
// pasar a ser su constante.
const labelSpare = "kling.spare"

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
		if cfg.Models[n].isReplica() {
			out[n] = replicaCount{}
		}
	}
	for _, m := range ms {
		if m.Labels[LabelGateway] != g.opts.ID {
			continue
		}
		name, _ := cfg.replicaModel(m.Service())
		if name == "" {
			continue
		}
		c := out[name]
		if m.Labels[labelSpare] != "" {
			// Un repuesto no sirve tráfico todavía: aparte, no como réplica.
			c.spare++
			out[name] = c
			continue
		}
		switch m.State {
		case api.StateRunning:
			c.running++
		case api.StateWarm:
			c.warm++
		}
		out[name] = c
	}
	g.repCache, g.repAt = out, time.Now()
	return out
}

// ---- proxy OpenAI

// errorProxy contesta un fallo de reenvío sin el texto del error interno, que
// lleva direcciones de invitados y mensajes del daemon (R8-11): eso va al log.
func (g *Gateway) errorProxy(w http.ResponseWriter, name string, err error) {
	log.Printf("proxy %s: %v", name, err)
	var mbe *http.MaxBytesError
	var we *wakeError
	switch {
	case errors.As(err, &mbe):
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body too large")
	case errors.As(err, &we):
		g.met.vonErr(name, "wake")
		g.met.inc(g.met.proxy, name, "5xx")
		writeError(w, http.StatusServiceUnavailable, "server_error", fmt.Sprintf("model %q is not available; retry later", name))
	default:
		g.met.vonErr(name, "request")
		g.met.inc(g.met.proxy, name, "5xx")
		writeError(w, http.StatusBadGateway, "server_error", fmt.Sprintf("model %q did not answer", name))
	}
}

// handleProxy reenvía una petición OpenAI (chat o completions) al backend del
// modelo que nombra "model", con streaming. El cuerpo ya viene acotado; lo
// que vuelve se acota en bytes y en tiempo, y solo se copian las cabeceras
// que un cliente necesita (el backend no decide cookies ni cabeceras propias).
func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	g.proxyOpenAI(w, r, ContractOpenAI)
}

// handleEmbeddings es /v1/embeddings: lo mismo hacia los backends
// openai-embeddings.
func (g *Gateway) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	g.proxyOpenAI(w, r, ContractOpenAIEmbeddings)
}

func (g *Gateway) proxyOpenAI(w http.ResponseWriter, r *http.Request, contract string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body too large")
		return
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil || req == nil {
		// null decodifica sin error a un mapa nil (R8-05): también es 400.
		writeError(w, http.StatusBadRequest, "invalid_request_error", "the body must be a JSON object")
		return
	}
	var model string
	if raw, ok := req["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	b := g.catalogo().porModelo(model, contract)
	if b == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("model %q does not serve %s on this gateway (see GET /v1/models)", model, r.URL.Path))
		return
	}
	if !permitido(b, scheduler.TenantFrom(r.Context()).Name()) {
		writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("this token cannot use model %q", b.Name))
		return
	}
	if len(b.Routes) > 0 && !b.permite(r.Method, r.URL.Path) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("model %q does not serve %s", b.Name, r.URL.Path))
		return
	}
	if contract == ContractOpenAI {
		if err := aplicarLimites(req, b.Limits); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if _, ok := req["seed"]; !ok && b.Snapshot != "" {
			// Las réplicas nacen del mismo volcado: la semilla la pone el host.
			req["seed"] = json.RawMessage(strconv.FormatInt(seed(), 10))
		}
	}
	if b.UpstreamModel != "" {
		req["model"], _ = json.Marshal(b.UpstreamModel)
	}
	stream := string(req["stream"]) == "true"
	out, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}

	id := idPeticion(r)
	w.Header().Set("X-Request-Id", id)
	ctx, cancel := context.WithTimeout(r.Context(), g.plazoDe(b))
	defer cancel()
	s := &salida{method: http.MethodPost, path: r.URL.Path, bytes: out, hdr: cabecerasSalida(r, id)}
	s.hdr.Set("Content-Type", "application/json")
	resp, soltar, err := g.reenviar(ctx, b, s)
	if err != nil {
		g.errorProxy(w, b.Name, err)
		return
	}
	defer soltar()
	defer resp.Body.Close()
	g.devolver(w, resp, b.Name, stream)
}

// plazoDe es el plazo de una petición a b: su limits.timeout (validado al
// cargar) o el -proxy-timeout del gateway. Con un dorado, un plazo mayor que
// el -proxy-timeout no alarga la espera a las cabeceras de la réplica, que
// fija el transporte de las réplicas.
func (g *Gateway) plazoDe(b *Backend) time.Duration {
	if b.Limits.Timeout != "" {
		if d, err := time.ParseDuration(b.Limits.Timeout); err == nil && d > 0 {
			return d
		}
	}
	return g.opts.ProxyTimeout
}

// camposTokens son los que acotan cuánto genera una petición: los de OpenAI y
// el propio de llama-server, que manda sobre max_tokens si viene.
var camposTokens = []string{"max_tokens", "max_completion_tokens", "n_predict"}

// aplicarLimites aplica los topes del modelo (R8-04a) a una petición openai:
// cada campo de tokens se recorta al tope (y si no viene ninguno se pone
// max_tokens, porque sin él la generación solo acaba con el contexto); un n o
// best_of mayor que el tope es un 400. Un tope 0 no topa nada.
func aplicarLimites(req map[string]json.RawMessage, l Limits) error {
	entero := func(k string) (int64, bool, error) {
		raw, ok := req[k]
		if !ok || string(raw) == "null" {
			return 0, false, nil
		}
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return 0, false, fmt.Errorf("%s must be an integer", k)
		}
		return v, true, nil
	}
	if l.MaxTokens > 0 {
		tope := json.RawMessage(strconv.Itoa(l.MaxTokens))
		alguno := false
		for _, k := range camposTokens {
			v, ok, err := entero(k)
			if err != nil {
				return err
			}
			if ok {
				alguno = true
				// <1 es «sin tope» en llama-server (-1): también se recorta.
				if v < 1 || v > int64(l.MaxTokens) {
					req[k] = tope
				}
			}
		}
		if !alguno {
			req["max_tokens"] = tope
		}
	}
	// logprobs: top_logprobs de chat y logprobs entero de completions (en chat
	// es booleano y pasa); prompt_logprobs es de vLLM. Negativo también es
	// 400: en vLLM -1 pide el vocabulario entero.
	topar := func(k string, tope int) error {
		raw, ok := req[k]
		if tope <= 0 || !ok || string(raw) == "null" || string(raw) == "true" || string(raw) == "false" {
			return nil
		}
		v, _, err := entero(k)
		if err != nil {
			return err
		}
		if v < 0 || v > int64(tope) {
			return fmt.Errorf("%s is limited to 0..%d for this model", k, tope)
		}
		return nil
	}
	for _, c := range []struct {
		k    string
		tope int
	}{{"top_logprobs", l.TopLogprobs}, {"logprobs", l.TopLogprobs}, {"prompt_logprobs", l.PromptLogprobs}} {
		if err := topar(c.k, c.tope); err != nil {
			return err
		}
	}
	if l.N > 0 {
		for _, k := range []string{"n", "best_of"} {
			v, ok, err := entero(k)
			if err != nil {
				return err
			}
			if ok && v > int64(l.N) {
				return fmt.Errorf("%s is limited to %d for this model", k, l.N)
			}
		}
	}
	return nil
}
