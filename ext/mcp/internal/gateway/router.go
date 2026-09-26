package gateway

// EL PROBLEMA DE VARIOS HOSTS.
//
// Un snapshot no viaja entre daemons: el catálogo de un Gateway solo conoce lo
// que SU daemon aloja. Antes de este fichero, `kling gateway` solo podía
// hablar con uno —el del contexto activo—, así que cada servicio quedaba
// atado al host donde se importó y no había forma de sumar capacidad de varias
// máquinas sin levantar un gateway por host y repartir a mano qué agente habla
// con cuál.
//
// Router resuelve eso por delante de N Gateway completos, uno por host, cada
// uno con su propio scheduler, catálogo, sesiones y memoria intactos. La regla
// es deliberadamente estrecha: Router decide A CUÁL de ellos va cada petición
// y le delega TODO lo demás (proxy, cuotas, reintentos de sesión, salud) por
// la misma ruta que ya tenía un solo host. Reimplementar esa maquinaria aquí
// habría duplicado exactamente lo que ya está probado en gateway.go.
import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// RouterHost es un daemon con nombre, tal como lo declaran -hosts o mcp.hosts.
type RouterHost struct {
	Name string
	GW   *Gateway
}

// Router enruta /mcp/<servicio> al host que tiene ese servicio en su catálogo.
type Router struct {
	hosts []*RouterHost

	PprofEnabled bool // igual que Gateway.PprofEnabled; lo decide quien compone el servidor

	agg *routerAgg

	// sessionHost fija una sesión MCP al host que la abrió, igual que un
	// Gateway fija una sesión a una instancia: el estado de la conversación
	// vive en el proceso del servidor MCP dentro de ESE host, y mandar la
	// segunda petición a otro la rompería aunque el servicio exista en varios.
	sessMu      sync.Mutex
	sessionHost map[string]*fijada
}

// fijada es a qué host va una sesión y cuándo se usó por última vez. La hora es
// lo que permite olvidarla: sin ella el mapa crecía con cada sesión que se
// abría y nunca se vaciaba, que en un gateway de larga vida es una fuga.
type fijada struct {
	host string
	uso  time.Time
}

// olvidarFijadas borra las fijaciones sin uso desde hace más de idle.
func (r *Router) olvidarFijadas(idle time.Duration) {
	r.sessMu.Lock()
	defer r.sessMu.Unlock()
	for sid, f := range r.sessionHost {
		if time.Since(f.uso) > idle {
			delete(r.sessionHost, sid)
		}
	}
}

// NewRouter construye un Router sobre hosts ya inicializados (con su propio
// New() ya llamado: Reap, PrewarmAll, etc. los arranca quien compone el
// servidor, igual que hace hoy con un solo Gateway).
func NewRouter(hosts []RouterHost) *Router {
	r := &Router{sessionHost: map[string]*fijada{}}
	for i := range hosts {
		h := hosts[i]
		r.hosts = append(r.hosts, &h)
	}
	r.agg = newRouterAgg(r)
	return r
}

// Handler expone las mismas rutas que un Gateway, repartidas entre hosts.
//
// El token se comprueba UNA vez, aquí, y no en cada host: todos comparten la
// misma configuración (mismo gateway.token, mismos tenants con nombre), así
// que basta con reutilizar el AuthHandler de cualquiera de ellos. Los hosts
// delegados exponen sus rutas SIN envolver (routes(), no Handler()) para que
// el token no se compruebe dos veces.
func (r *Router) Handler(token string) http.Handler {
	if len(r.hosts) == 0 {
		panic("gateway: Router sin hosts")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/services", r.handleServices)
	// El agregador va ANTES que el comodín de servicio, por lo mismo que en un
	// solo host: "_all" no debe interpretarse como un nombre de servicio.
	mux.HandleFunc("/mcp/"+AggregatePath, r.agg.handle)
	mux.HandleFunc("/mcp/"+AggregatePath+"/", r.agg.handle)
	mux.HandleFunc("/mcp/{service}/", r.handleProxy)
	mux.HandleFunc("/mcp/{service}", r.handleProxy)

	if r.PprofEnabled {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
		mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
		mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
		mux.Handle("/debug/pprof/block", pprof.Handler("block"))
		mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
		mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	}

	return logging(r.hosts[0].GW.AuthHandler(mux, token))
}

// Reap libera las sesiones del agregador combinado que llevan ociosas más de
// cuatro veces el idle configurado. Es el equivalente, a nivel de Router, de
// lo que g.OnTick ya hace para un solo host (ver New): las sesiones del
// agregador son baratas, pero no gratis, y sin esto se acumularían para
// siempre.
func (r *Router) Reap(ctx context.Context) {
	idle := 5 * time.Minute
	if len(r.hosts) > 0 {
		if i := r.hosts[0].GW.Idle(); i > 0 {
			idle = i
		}
	}
	t := time.NewTicker(idle / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.agg.reap(idle * 4)
			r.olvidarFijadas(idle * 4)
		}
	}
}

func (r *Router) hostByName(name string) *RouterHost {
	for _, h := range r.hosts {
		if h.Name == name {
			return h
		}
	}
	return nil
}

// handleServices lista los servicios de TODOS los hosts, cada uno marcado con
// el suyo. Un host que no contesta no tumba a los demás: se anota y se sigue.
func (r *Router) handleServices(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, h := range r.hosts {
		snaps, err := h.GW.Client().Snapshots(req.Context())
		if err != nil {
			fmt.Fprintf(w, "# host=%s unreachable: %v\n", h.Name, err)
			continue
		}
		for _, s := range snaps {
			name := s.Name
			if svc := s.Service(); svc != "" {
				name = svc
			}
			st := h.GW.Status(name)
			status := "cold"
			if st.Prewarmed > 0 {
				status = fmt.Sprintf("%d prewarmed instance(s)", st.Prewarmed)
			}
			if st.Warm {
				// st.Addr en vez de st.IP: ver la misma nota en gateway.go.
				status = fmt.Sprintf("warm at %s · %d session(s) · idle %s",
					st.Addr, st.Sessions, st.Idle.Round(time.Second))
			}
			fmt.Fprintf(w, "%-24s snapshot=%-20s host=%-10s %s\n", name, s.Name, h.Name, status)
		}
	}
}

// hostsFor devuelve los hosts que tienen `service` en su catálogo (microVM o
// enlace externo). Un host inalcanzable no es candidato: no hay a quién
// preguntarle si lo tiene.
func (r *Router) hostsFor(ctx context.Context, service string) []*RouterHost {
	var out []*RouterHost
	for _, h := range r.hosts {
		names, err := h.GW.agg.cat.services(ctx)
		if err != nil {
			continue
		}
		if contains(names, service) {
			out = append(out, h)
		}
	}
	return out
}

// byAvailableMemory ordena los candidatos por memoria disponible del host
// (GET /procstats), de más a menos. Un host que no contesta va al final: sigue
// siendo candidato —puede que el propio /mcp/<servicio> sí funcione— pero no se
// prueba primero.
func (r *Router) byAvailableMemory(ctx context.Context, hosts []*RouterHost) []*RouterHost {
	if len(hosts) <= 1 {
		return hosts
	}
	type ranked struct {
		h   *RouterHost
		mib int64
		ok  bool
	}
	rs := make([]ranked, len(hosts))
	for i, h := range hosts {
		ps, err := h.GW.Client().ProcStats(ctx)
		rs[i] = ranked{h: h, ok: err == nil}
		if err == nil {
			rs[i].mib = ps.AvailableMiB
		}
	}
	// SliceStable: entre empatados (o igual de inalcanzables), se conserva el
	// orden de -hosts, que es lo único predecible que se le puede ofrecer al
	// operador cuando el desempate por memoria no dice nada.
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].ok != rs[j].ok {
			return rs[i].ok
		}
		return rs[i].mib > rs[j].mib
	})
	out := make([]*RouterHost, len(rs))
	for i, x := range rs {
		out[i] = x.h
	}
	return out
}

// handleProxy decide el host y le delega la petición ENTERA (routes(), el
// mismo mux que usaría ese host solo): cuotas, sesión pegajosa, reintento de
// escalado, todo sigue viviendo en gateway.go.
func (r *Router) handleProxy(w http.ResponseWriter, req *http.Request) {
	service := req.PathValue("service")
	if service == "" {
		http.Error(w, "missing service in path", http.StatusBadRequest)
		return
	}
	ctx := req.Context()

	// Sesión ya fijada: se respeta aunque el servicio exista en más de un
	// host, por la misma razón de siempre: el estado de la conversación vive
	// en el proceso del servidor MCP de ESA instancia concreta.
	if sid := req.Header.Get(SessionHeader); sid != "" {
		r.sessMu.Lock()
		pinned := ""
		if f := r.sessionHost[sid]; f != nil {
			pinned, f.uso = f.host, time.Now()
		}
		r.sessMu.Unlock()
		if pinned != "" {
			if h := r.hostByName(pinned); h != nil {
				h.GW.routes().ServeHTTP(w, req)
				// Cerrar la sesión es el momento natural de olvidarla.
				if req.Method == http.MethodDelete {
					r.sessMu.Lock()
					delete(r.sessionHost, sid)
					r.sessMu.Unlock()
				}
				return
			}
			// El host al que estaba fijada ya no está en -hosts: se olvida y se
			// resuelve otra vez más abajo como sesión desconocida.
			r.sessMu.Lock()
			delete(r.sessionHost, sid)
			r.sessMu.Unlock()
		}
	}

	candidates := r.hostsFor(ctx, service)
	if len(candidates) == 0 {
		http.Error(w, fmt.Sprintf("service %q not found on any configured host", service), http.StatusNotFound)
		return
	}
	candidates = r.byAvailableMemory(ctx, candidates)

	// Un GET sin sesión (la sonda de stream independiente) o un DELETE de una
	// sesión que el router no conoce no van a despertar nada: se sirven tal
	// cual desde el primer candidato, igual que un solo host se serviría a sí
	// mismo. El reintento por memoria solo tiene sentido para lo que SÍ
	// despierta una instancia.
	if req.Method != http.MethodPost {
		candidates[0].GW.routes().ServeHTTP(w, req)
		return
	}

	// El bucle SIEMPRE termina sirviendo desde algún host: el último candidato
	// nunca se salta a sí mismo (ver más abajo), así que no hace falta un
	// error final "ningún host pudo" — el que atiende de verdad es quien
	// traduce su propio fallo, exactamente como con un solo host.
	for i, h := range candidates {
		// El ÚLTIMO candidato no se prueba por separado: se le delega
		// directamente y que su propio camino traduzca el error de siempre.
		// Probarlo aparte solo serviría para despertar la instancia DOS veces
		// (una en el sondeo, otra dentro de routes()) sin nada que ganar, ya
		// que no hay un candidato más al que pasar si también falla.
		if i < len(candidates)-1 {
			if _, err := h.GW.Ensure(ctx, service); err != nil {
				// Falta de sitio en este host (memoria, tope de máquinas o, desde
				// kindling v0.8, disco): otro host puede tener.
				if api.IsInsufficientMemory(err) || api.IsMachineLimit(err) || api.IsDiskFull(err) {
					log.Printf("router: %s: host %q no lo pudo tomar (%v); pruebo el siguiente", service, h.Name, err)
					continue
				}
				// Un fallo de otra clase no es un problema de sitio: no tiene
				// sentido probar otro host, se sirve tal cual desde éste para
				// que traduzca el error de siempre (mismos códigos que con un
				// solo host).
			}
		}
		h.GW.routes().ServeHTTP(w, req)
		// Sesión NUEVA: se fija al host que la sirvió, leyendo la cabecera que
		// puso handleProxy/serveNewSession de ESE host en la respuesta real.
		if req.Header.Get(SessionHeader) == "" {
			if sid := w.Header().Get(SessionHeader); sid != "" {
				r.sessMu.Lock()
				r.sessionHost[sid] = &fijada{host: h.Name, uso: time.Now()}
				r.sessMu.Unlock()
			}
		}
		return
	}
}

// ── /mcp/_all combinado ────────────────────────────────────────────────────
//
// Variante "más simple aceptable" de un agregador multi-host: en vez de
// fusionar de verdad la maquinaria de cada host (sesiones, memoria de uso,
// coerción de tipos…), reutiliza tal cual el forward() del *aggregator de
// CADA host —el mismo que ya usa /mcp/_all de un solo host— y sólo decide a
// cuál de ellos preguntar. La sesión que ve el cliente es del Router; por
// detrás abre, perezosamente, una sesión "sombra" por host tocado, para que el
// estado del servidor MCP real sobreviva entre llamadas de la misma
// conversación.
type routerAgg struct {
	r *Router

	mu       sync.Mutex
	sessions map[string]*routerSession
}

type routerSession struct {
	mode mode
	// perHost: nombre de host -> sesión sombra abierta contra el *aggregator
	// de ese host. Se crea la primera vez que la conversación necesita hablar
	// con ese host, y se reutiliza después para que backing (la sesión MCP de
	// verdad) no se pierda entre llamadas.
	perHost map[string]*aggSession
	lastUse time.Time
}

func newRouterAgg(r *Router) *routerAgg {
	return &routerAgg{r: r, sessions: map[string]*routerSession{}}
}

func (ra *routerAgg) handle(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodDelete {
		ra.drop(req.Header.Get(SessionHeader))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.Method != http.MethodPost {
		http.Error(w, "only POST and DELETE", http.StatusMethodNotAllowed)
		return
	}

	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(req.Body).Decode(&rpc); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	ctx := req.Context()
	s, sid, created := ra.session(req)
	if created {
		w.Header().Set(SessionHeader, sid)
	}
	if len(rpc.ID) == 0 || string(rpc.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, fault := ra.dispatch(ctx, s, rpc.Method, rpc.Params)
	if fault != nil {
		writeRPCError(w, rpc.ID, fault.code, fault.msg)
		return
	}
	writeRPCResult(w, rpc.ID, result)
}

func (ra *routerAgg) session(req *http.Request) (*routerSession, string, bool) {
	if sid := req.Header.Get(SessionHeader); sid != "" {
		ra.mu.Lock()
		s, ok := ra.sessions[sid]
		if ok {
			s.lastUse = time.Now()
		}
		ra.mu.Unlock()
		if ok {
			return s, sid, false
		}
	}
	m := modeProxy
	if mode(req.URL.Query().Get("mode")) == modeExpand {
		m = modeExpand
	}
	sid := newSessionID()
	s := &routerSession{mode: m, perHost: map[string]*aggSession{}, lastUse: time.Now()}
	ra.mu.Lock()
	ra.sessions[sid] = s
	ra.mu.Unlock()
	return s, sid, true
}

func (ra *routerAgg) drop(sid string) {
	if sid == "" {
		return
	}
	ra.mu.Lock()
	defer ra.mu.Unlock()
	delete(ra.sessions, sid)
}

func (ra *routerAgg) reap(idle time.Duration) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	for id, s := range ra.sessions {
		if time.Since(s.lastUse) > idle {
			delete(ra.sessions, id)
		}
	}
}

func (ra *routerAgg) dispatch(ctx context.Context, s *routerSession, method string, params json.RawMessage) (any, *rpcFault) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "kindling",
				"version": "1.0.0",
			},
			"instructions": ra.instructions(ctx, s),
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return ra.listTools(ctx, s)
	case "tools/call":
		return ra.callTool(ctx, s, params)
	default:
		return nil, &rpcFault{-32601, "unsupported method: " + method}
	}
}

// serviceHosts combina el catálogo de todos los hosts en un único mapa
// servicio -> host dueño. Un mismo nombre en dos hosts —importado dos veces
// por error— se queda con el primero por orden de -hosts, que es predecible,
// en vez de con el que respondiera antes por azar.
func (ra *routerAgg) serviceHosts(ctx context.Context) map[string]*RouterHost {
	out := map[string]*RouterHost{}
	for _, h := range ra.r.hosts {
		names, err := h.GW.agg.cat.services(ctx)
		if err != nil {
			continue
		}
		for _, n := range names {
			if _, dup := out[n]; !dup {
				out[n] = h
			}
		}
	}
	return out
}

func (ra *routerAgg) instructions(ctx context.Context, s *routerSession) string {
	if s.mode == modeExpand {
		return "Tools from several MCP servers across hosts, with service.tool names."
	}

	byHost := ra.serviceHosts(ctx)
	services := make([]string, 0, len(byHost))
	for n := range byHost {
		services = append(services, n)
	}
	sort.Strings(services)

	var b strings.Builder
	b.WriteString("Available tools, grouped by service:\n\n")
	for _, svc := range services {
		h := byHost[svc]
		tools, err := h.GW.agg.cat.toolsOf(ctx, svc)
		if err != nil {
			fmt.Fprintf(&b, "%s (host=%s, unavailable: %v)\n", svc, h.Name, err)
			continue
		}
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Name)
		}
		fmt.Fprintf(&b, "%s (host=%s): %s\n", svc, h.Name, strings.Join(names, ", "))
	}
	b.WriteString("\nCall them with call_tool and the full service.tool name " +
		"(e.g. filesystem.read_text_file). If you don't know its arguments, ask " +
		"describe_tool first. find_tools is for searching by keyword.")
	return b.String()
}

func (ra *routerAgg) listTools(ctx context.Context, s *routerSession) (any, *rpcFault) {
	if s.mode == modeProxy {
		return map[string]any{"tools": metaToolsList()}, nil
	}

	byHost := ra.serviceHosts(ctx)
	var out []map[string]any
	for svc, h := range byHost {
		tools, err := h.GW.agg.cat.toolsOf(ctx, svc)
		if err != nil {
			continue
		}
		for _, t := range tools {
			schema := t.Schema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			out = append(out, map[string]any{
				"name":        t.Qualified,
				"description": fmt.Sprintf("[%s@%s] %s", t.Service, h.Name, t.Description),
				"inputSchema": schema,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["name"].(string) < out[j]["name"].(string)
	})
	return map[string]any{"tools": out}, nil
}

func (ra *routerAgg) callTool(ctx context.Context, s *routerSession, params json.RawMessage) (any, *rpcFault) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)

	if s.mode == modeProxy {
		switch p.Name {
		case "find_tools":
			return ra.doFindTools(ctx, p.Arguments)
		case "describe_tool":
			return ra.doDescribeTool(ctx, p.Arguments)
		case "call_tool":
			var inner struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(p.Arguments, &inner)
			return ra.forward(ctx, s, inner.Name, inner.Arguments)
		default:
			if strings.Contains(p.Name, ".") {
				return ra.forward(ctx, s, p.Name, p.Arguments)
			}
			return nil, &rpcFault{-32602, "unknown tool: " + p.Name}
		}
	}
	return ra.forward(ctx, s, p.Name, p.Arguments)
}

func (ra *routerAgg) doFindTools(ctx context.Context, args json.RawMessage) (any, *rpcFault) {
	var p struct {
		Query   string `json:"query"`
		Service string `json:"service"`
	}
	_ = json.Unmarshal(args, &p)

	byHost := ra.serviceHosts(ctx)
	if p.Service != "" {
		h, ok := byHost[p.Service]
		if !ok {
			return nil, &rpcFault{-32602, "service not available: " + p.Service}
		}
		byHost = map[string]*RouterHost{p.Service: h}
	}

	terms := expandTerms(p.Query)
	type scored struct {
		t    Tool
		host string
		n    int
	}
	collect := func(scoreOnly bool) []scored {
		var hits []scored
		for svc, h := range byHost {
			tools, err := h.GW.agg.cat.toolsOf(ctx, svc)
			if err != nil {
				continue
			}
			for _, t := range tools {
				n := 0
				for _, term := range terms {
					if strings.Contains(t.haystack, term) {
						n++
					}
				}
				if !scoreOnly || n > 0 {
					hits = append(hits, scored{t, h.Name, n})
				}
			}
		}
		return hits
	}

	hits := collect(true)
	if len(hits) == 0 {
		// Sin coincidencias se devuelve todo: es mejor que el modelo vea el
		// catálogo compacto a que concluya que no hay herramientas.
		hits = collect(false)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return hits[i].t.Qualified < hits[j].t.Qualified
	})

	var sb strings.Builder
	for i, h := range hits {
		if i >= 25 {
			fmt.Fprintf(&sb, "... and %d more; narrow your search\n", len(hits)-i)
			break
		}
		fmt.Fprintf(&sb, "%-28s host=%-10s %s\n", h.t.Qualified, h.host, h.t.Description)
	}
	if sb.Len() == 0 {
		return textResult("No tools found."), nil
	}
	sb.WriteString("\nUse describe_tool to see the arguments, or call_tool to run it.")
	return textResult(sb.String()), nil
}

func (ra *routerAgg) doDescribeTool(ctx context.Context, args json.RawMessage) (any, *rpcFault) {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(args, &p)

	t, _, fault := ra.lookup(ctx, p.Name)
	if fault != nil {
		return nil, fault
	}
	schema := "{}"
	if len(t.Schema) > 0 {
		schema = string(t.Schema)
	}
	return textResult(fmt.Sprintf("%s\n\n%s\n\nArguments:\n%s",
		t.Qualified, t.Description, schema)), nil
}

func (ra *routerAgg) lookup(ctx context.Context, name string) (*Tool, *RouterHost, *rpcFault) {
	service, tool, ok := strings.Cut(name, ".")
	if !ok {
		return nil, nil, &rpcFault{-32602, "use the qualified name service.tool, not " + name}
	}
	byHost := ra.serviceHosts(ctx)
	h, ok := byHost[service]
	if !ok {
		return nil, nil, &rpcFault{-32602, "service not available: " + service}
	}
	tools, err := h.GW.agg.cat.toolsOf(ctx, service)
	if err != nil {
		return nil, nil, &rpcFault{-32000, err.Error()}
	}
	for i := range tools {
		if tools[i].Name == tool {
			return &tools[i], h, nil
		}
	}
	return nil, nil, &rpcFault{-32602, "no such tool: " + name}
}

// forward busca a qué host pertenece la herramienta y le delega la llamada
// real a través de SU PROPIO aggregator.forward: la coerción de tipos, el
// candado de inicialización por servicio, el modo efímero y el reintento de
// sesión caducada quedan exactamente como en un solo host.
func (ra *routerAgg) forward(ctx context.Context, s *routerSession, name string, args json.RawMessage) (any, *rpcFault) {
	t, h, fault := ra.lookup(ctx, name)
	if fault != nil {
		return nil, fault
	}
	sub := ra.subSession(s, h, t.Service)
	return h.GW.agg.forward(ctx, sub, name, args)
}

// subSession devuelve la sesión "sombra" que esta conversación mantiene contra
// un host concreto, creándola la primera vez. Es lo que hace que el estado del
// servidor MCP real —backing, en aggSession— sobreviva entre llamadas de la
// misma conversación, aunque esta hable con varios hosts a la vez.
func (ra *routerAgg) subSession(s *routerSession, h *RouterHost, service string) *aggSession {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	sub, ok := s.perHost[h.Name]
	if !ok {
		sub = &aggSession{
			id: newSessionID(), mode: modeProxy,
			backing: map[string]string{}, lastUse: time.Now(),
		}
		s.perHost[h.Name] = sub
	}
	if !contains(sub.services, service) {
		sub.services = append(sub.services, service)
	}
	return sub
}
