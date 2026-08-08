package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// AggregatePath es el servicio virtual que reúne a todos los demás.
const AggregatePath = "_all"

// EL PROBLEMA DEL CONTEXTO.
//
// Un cliente MCP carga las definiciones de TODAS las herramientas al conectarse.
// Con veinte servicios de diez herramientas son doscientos esquemas JSON en el
// contexto del modelo antes de que empiece a trabajar — decenas de miles de
// tokens gastados en describir herramientas que quizá no use.
//
// Este agregador expone en su lugar CUATRO meta-herramientas. El modelo busca lo
// que necesita, pide el esquema de lo que va a usar, y llama. El coste fijo pasa
// de doscientas definiciones a cuatro.
//
// Para clientes que prefieran el catálogo entero está `mode=expand`, que aplana
// todas las herramientas con nombres `servicio.herramienta`.
type mode string

const (
	modeProxy  mode = "proxy"  // meta-herramientas (por defecto)
	modeExpand mode = "expand" // catálogo completo aplanado
)

// aggregator implementa un servidor MCP que enruta a los demás.
type aggregator struct {
	gw        *Gateway
	cat       *catalog
	ephemeral bool // una microVM por acción, que muere al terminar

	mu       sync.Mutex
	sessions map[string]*aggSession

	// snapOf cachea servicio -> snapshot. Está en el camino caliente del modo
	// efímero y solo cambia al importar un servicio.
	snapMu   sync.RWMutex
	snapOf   map[string]string
	stateful map[string]bool

	// initLocks serializa la creación de sesión por servicio.
	initLocks sync.Map // servicio -> *sync.Mutex
}

// serviceLock devuelve el candado de creación de sesión de un servicio.
func (a *aggregator) serviceLock(service string) *sync.Mutex {
	v, _ := a.initLocks.LoadOrStore(service, &sync.Mutex{})
	return v.(*sync.Mutex)
}

type aggSession struct {
	id       string
	services []string
	mode     mode
	// Sesiones abiertas contra los servicios de detrás: se reutilizan para que
	// el estado de una conversación sobreviva entre llamadas.
	backing map[string]string
	lastUse time.Time
}

func newAggregator(gw *Gateway, ephemeral bool) *aggregator {
	return &aggregator{
		gw: gw, cat: newCatalog(gw, 10*time.Minute),
		ephemeral: ephemeral, sessions: map[string]*aggSession{},
		snapOf: map[string]string{}, stateful: map[string]bool{},
	}
}

// handleAggregate atiende el endpoint virtual /mcp/_all.
func (g *Gateway) handleAggregate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		g.agg.drop(r.Header.Get(SessionHeader))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "solo POST y DELETE", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "JSON inválido", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	s, created, err := g.agg.session(ctx, r)
	if err != nil {
		writeRPCError(w, req.ID, -32000, err.Error())
		return
	}
	if created {
		w.Header().Set(SessionHeader, s.id)
	}
	// Notificación: sin id, sin respuesta.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := g.agg.dispatch(ctx, s, req.Method, req.Params)
	if rpcErr != nil {
		writeRPCError(w, req.ID, rpcErr.code, rpcErr.msg)
		return
	}
	writeRPCResult(w, req.ID, result)
}

type rpcFault struct {
	code int
	msg  string
}

func (a *aggregator) dispatch(ctx context.Context, s *aggSession, method string, params json.RawMessage) (any, *rpcFault) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "kindling",
				"version": "1.0.0",
			},
			"instructions": a.instructions(ctx, s),
		}, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		return a.listTools(ctx, s)

	case "tools/call":
		return a.callTool(ctx, s, params)

	default:
		return nil, &rpcFault{-32601, "método no soportado: " + method}
	}
}

// instructions incluye el INVENTARIO COMPLETO de nombres.
//
// Los nombres son baratos (27 herramientas ocupan ~100 tokens); lo caro son los
// esquemas de argumentos. Poniéndolos aquí, el modelo sabe qué hay disponible
// desde el primer momento y se ahorra una llamada de descubrimiento: pasa
// directamente a describe_tool o a call_tool.
func (a *aggregator) instructions(ctx context.Context, s *aggSession) string {
	if s.mode == modeExpand {
		return "Herramientas de varios servidores MCP, con nombres servicio.herramienta."
	}

	var b strings.Builder
	b.WriteString("Herramientas disponibles, agrupadas por servicio:\n\n")

	tools, _ := a.cat.all(ctx, s.services)
	bySvc := map[string][]string{}
	var order []string
	for _, t := range tools {
		if _, seen := bySvc[t.Service]; !seen {
			order = append(order, t.Service)
		}
		bySvc[t.Service] = append(bySvc[t.Service], t.Name)
	}
	for _, svc := range order {
		mark := ""
		if a.ephemeral && a.isStateful(ctx, svc) {
			mark = " [recuerda entre llamadas]"
		}
		fmt.Fprintf(&b, "%s%s: %s\n", svc, mark, strings.Join(bySvc[svc], ", "))
	}

	b.WriteString("\nLlámalas con call_tool y el nombre completo servicio.herramienta " +
		"(p. ej. filesystem.read_text_file). Si no conoces sus argumentos, pide primero " +
		"describe_tool. find_tools sirve para buscar por palabras clave.")

	if a.ephemeral {
		b.WriteString("\n\nCada llamada corre en una máquina aislada que se destruye al " +
			"terminar, así que las herramientas marcadas como sin estado NO recuerdan nada " +
			"entre llamadas.")
	}
	return b.String()
}

// ── modo proxy: cuatro meta-herramientas ──────────────────────────────────────

func (a *aggregator) metaTools() []map[string]any {
	// Tres, no cuatro: el inventario va en las instrucciones del initialize, así
	// que `list_services` sobraba y obligaba a una llamada de más.
	return []map[string]any{
		{
			"name": "find_tools",
			"description": "Busca herramientas por palabras clave en todos los servicios. Devuelve nombres " +
				"y descripciones, SIN los esquemas de argumentos, para no gastar contexto.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":   map[string]any{"type": "string", "description": "qué necesitas hacer"},
					"service": map[string]any{"type": "string", "description": "limitar a un servicio (opcional)"},
				},
				"required": []string{"query"},
			},
		},
		{
			"name": "describe_tool",
			"description": "Devuelve el esquema completo de argumentos de una herramienta. " +
				"Úsalo antes de call_tool si no conoces sus parámetros.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "nombre servicio.herramienta"},
				},
				"required": []string{"name"},
			},
		},
		{
			"name":        "call_tool",
			"description": "Ejecuta una herramienta de cualquier servicio.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":      map[string]any{"type": "string", "description": "nombre servicio.herramienta"},
					"arguments": map[string]any{"type": "object", "description": "argumentos de la herramienta"},
				},
				"required": []string{"name"},
			},
		},
	}
}

func (a *aggregator) listTools(ctx context.Context, s *aggSession) (any, *rpcFault) {
	if s.mode == modeProxy {
		return map[string]any{"tools": a.metaTools()}, nil
	}

	// modeExpand: catálogo completo aplanado.
	tools, errs := a.cat.all(ctx, s.services)
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, map[string]any{
			"name":        t.Qualified,
			"description": fmt.Sprintf("[%s] %s", t.Service, t.Description),
			"inputSchema": schema,
		})
	}
	res := map[string]any{"tools": out}
	if len(errs) > 0 {
		res["_errors"] = errs
	}
	return res, nil
}

func (a *aggregator) callTool(ctx context.Context, s *aggSession, params json.RawMessage) (any, *rpcFault) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)

	if s.mode == modeProxy {
		switch p.Name {
		case "list_services":
			return a.doListServices(ctx, s)
		case "find_tools":
			return a.doFindTools(ctx, s, p.Arguments)
		case "describe_tool":
			return a.doDescribeTool(ctx, s, p.Arguments)
		case "call_tool":
			var inner struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(p.Arguments, &inner)
			return a.forward(ctx, s, inner.Name, inner.Arguments)
		default:
			// Un nombre cualificado directo también vale: si el modelo ya lo sabe,
			// no tiene sentido obligarle a envolverlo en call_tool.
			if strings.Contains(p.Name, ".") {
				return a.forward(ctx, s, p.Name, p.Arguments)
			}
			return nil, &rpcFault{-32602, "herramienta desconocida: " + p.Name}
		}
	}
	return a.forward(ctx, s, p.Name, p.Arguments)
}

func textResult(s string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
}

func (a *aggregator) doListServices(ctx context.Context, s *aggSession) (any, *rpcFault) {
	var sb strings.Builder
	for _, svc := range s.services {
		tools, err := a.cat.toolsOf(ctx, svc)
		if err != nil {
			fmt.Fprintf(&sb, "%-20s (no disponible: %v)\n", svc, err)
			continue
		}
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Name)
		}
		fmt.Fprintf(&sb, "%-20s %d herramienta(s): %s\n", svc, len(tools), strings.Join(names, ", "))
	}
	if sb.Len() == 0 {
		return textResult("No hay servicios disponibles."), nil
	}
	return textResult(sb.String()), nil
}

func (a *aggregator) doFindTools(ctx context.Context, s *aggSession, args json.RawMessage) (any, *rpcFault) {
	var p struct {
		Query   string `json:"query"`
		Service string `json:"service"`
	}
	_ = json.Unmarshal(args, &p)

	services := s.services
	if p.Service != "" {
		if !contains(services, p.Service) {
			return nil, &rpcFault{-32602, "servicio no disponible: " + p.Service}
		}
		services = []string{p.Service}
	}

	tools, _ := a.cat.all(ctx, services)
	terms := strings.Fields(strings.ToLower(p.Query))

	type scored struct {
		t Tool
		n int
	}
	var hits []scored
	for _, t := range tools {
		hay := strings.ToLower(t.Qualified + " " + t.Description)
		n := 0
		for _, term := range terms {
			if strings.Contains(hay, term) {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, scored{t, n})
		}
	}
	// Sin coincidencias se devuelve todo: es mejor que el modelo vea el catálogo
	// compacto a que concluya que no hay herramientas.
	if len(hits) == 0 {
		for _, t := range tools {
			hits = append(hits, scored{t, 0})
		}
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
			fmt.Fprintf(&sb, "... y %d más; afina la búsqueda\n", len(hits)-i)
			break
		}
		fmt.Fprintf(&sb, "%-28s %s\n", h.t.Qualified, h.t.Description)
	}
	sb.WriteString("\nUsa describe_tool para ver los argumentos, o call_tool para ejecutar.")
	return textResult(sb.String()), nil
}

func (a *aggregator) doDescribeTool(ctx context.Context, s *aggSession, args json.RawMessage) (any, *rpcFault) {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(args, &p)

	t, fault := a.lookup(ctx, s, p.Name)
	if fault != nil {
		return nil, fault
	}
	schema := "{}"
	if len(t.Schema) > 0 {
		schema = string(t.Schema)
	}
	return textResult(fmt.Sprintf("%s\n\n%s\n\nArgumentos:\n%s",
		t.Qualified, t.Description, schema)), nil
}

func (a *aggregator) lookup(ctx context.Context, s *aggSession, name string) (*Tool, *rpcFault) {
	service, tool, ok := strings.Cut(name, ".")
	if !ok {
		return nil, &rpcFault{-32602, "usa el nombre cualificado servicio.herramienta, no " + name}
	}
	if !contains(s.services, service) {
		return nil, &rpcFault{-32602, "servicio no disponible: " + service}
	}
	tools, err := a.cat.toolsOf(ctx, service)
	if err != nil {
		return nil, &rpcFault{-32000, err.Error()}
	}
	for i := range tools {
		if tools[i].Name == tool {
			return &tools[i], nil
		}
	}
	return nil, &rpcFault{-32602, "no existe la herramienta " + name}
}

// forward ejecuta la llamada contra el servicio real.
func (a *aggregator) forward(ctx context.Context, s *aggSession, name string, args json.RawMessage) (any, *rpcFault) {
	t, fault := a.lookup(ctx, s, name)
	if fault != nil {
		return nil, fault
	}

	// Antes de nada, reparar los tipos que el cliente pudiera haber estropeado.
	// Se hace aquí, con el esquema declarado por la herramienta a mano.
	before := string(args)
	args = coerceArgs(args, t.Schema)
	if s := string(args); s != before {
		log.Printf("%s: tipos reparados\n  recibido: %s\n  enviado:  %s",
			t.Qualified, trunc(before, 300), trunc(s, 300))
	}

	// Modo efímero: la acción se ejecuta en una microVM propia que muere después.
	//
	// Salvo que el servicio esté marcado como stateful: destruir su máquina se
	// llevaría por delante lo que acumula —un grafo de conocimiento, una cadena
	// de razonamiento— y cada llamada empezaría de cero. Esos van por la ruta
	// persistente, que congela la instancia al quedar ociosa en vez de matarla.
	if a.ephemeral && !a.isStateful(ctx, t.Service) {
		return a.callEphemeral(ctx, t, args)
	}

	e, err := a.gw.ensure(ctx, t.Service)
	if err != nil {
		return nil, &rpcFault{-32000, err.Error()}
	}
	base := "http://" + e.ip + ":" + fmt.Sprint(GuestPort)

	// Una sesión por servicio y por conversación: el estado del servidor MCP debe
	// persistir entre llamadas del mismo cliente.
	//
	// La creación va bajo candado por servicio. Sin él, N llamadas concurrentes
	// que llegan antes de existir la sesión crean N sesiones, y cada sesión lanza
	// un proceso del servidor MCP dentro de la microVM: ocho peticiones paralelas
	// arrancaban ocho procesos de node en 384 MiB y la máquina se ahogaba.
	initLock := a.serviceLock(t.Service)
	initLock.Lock()
	a.mu.Lock()
	sid := s.backing[t.Service]
	a.mu.Unlock()
	if sid == "" {
		newSid, ierr := mcpInit(ctx, base)
		if ierr != nil {
			initLock.Unlock()
			return nil, &rpcFault{-32000, fmt.Sprintf("%s: %v", t.Service, ierr)}
		}
		sid = newSid
		a.mu.Lock()
		s.backing[t.Service] = sid
		a.mu.Unlock()
	}
	initLock.Unlock()

	call := func(sid string) (json.RawMessage, error) {
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
			t.Name, args)
		return mcpCall(ctx, base, sid, body)
	}

	raw, err := call(sid)
	if err != nil {
		// La sesión caducó: pasa siempre que la microVM se congeló entre llamadas,
		// porque sus procesos mueren con ella. Se rehace, también bajo candado
		// para que no se dupliquen.
		initLock.Lock()
		a.mu.Lock()
		cur := s.backing[t.Service]
		a.mu.Unlock()
		if cur == sid { // nadie la rehízo mientras esperábamos
			newSid, ierr := mcpInit(ctx, base)
			if ierr != nil {
				initLock.Unlock()
				return nil, &rpcFault{-32000, fmt.Sprintf("%s: %v", t.Service, ierr)}
			}
			cur = newSid
			a.mu.Lock()
			s.backing[t.Service] = cur
			a.mu.Unlock()
		}
		initLock.Unlock()
		if raw, err = call(cur); err != nil {
			return nil, &rpcFault{-32000, fmt.Sprintf("%s: %v", t.Service, err)}
		}
	}

	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, &rpcFault{-32000, "respuesta ilegible de " + t.Service}
	}
	if resp.Error != nil {
		return nil, &rpcFault{resp.Error.Code, resp.Error.Message}
	}
	return json.RawMessage(resp.Result), nil
}

// ── sesiones del agregador ────────────────────────────────────────────────────

// session resuelve la sesión, tomando la selección de servicios de la URL.
//
//	/mcp/_all                          todos, con meta-herramientas
//	/mcp/_all?services=eco,files       solo esos dos
//	/mcp/_all?mode=expand              catálogo completo aplanado
func (a *aggregator) session(ctx context.Context, r *http.Request) (*aggSession, bool, error) {
	if sid := r.Header.Get(SessionHeader); sid != "" {
		a.mu.Lock()
		s, ok := a.sessions[sid]
		if ok {
			s.lastUse = time.Now()
		}
		a.mu.Unlock()
		if ok {
			return s, false, nil
		}
	}

	q := r.URL.Query()
	m := modeProxy
	if mode(q.Get("mode")) == modeExpand {
		m = modeExpand
	}

	available, err := a.cat.services(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("no puedo listar los servicios: %w", err)
	}
	services := available
	if sel := q.Get("services"); sel != "" {
		services = nil
		for _, want := range strings.Split(sel, ",") {
			want = strings.TrimSpace(want)
			if want == "" {
				continue
			}
			if !contains(available, want) {
				return nil, false, fmt.Errorf("servicio %q no existe (hay: %s)", want, strings.Join(available, ", "))
			}
			services = append(services, want)
		}
	}
	if len(services) == 0 {
		return nil, false, fmt.Errorf("no hay servicios disponibles: crea uno con `kling commit`")
	}

	s := &aggSession{
		id: newSessionID(), services: services, mode: m,
		backing: map[string]string{}, lastUse: time.Now(),
	}
	a.mu.Lock()
	a.sessions[s.id] = s
	a.mu.Unlock()
	return s, true, nil
}

func (a *aggregator) drop(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, sid)
}

func (a *aggregator) reap(idle time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, s := range a.sessions {
		if time.Since(s.lastUse) > idle {
			delete(a.sessions, id)
		}
	}
}

// ── utilidades ────────────────────────────────────────────────────────────────

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "result": result,
	})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
}

// isStateful consulta si el servicio conserva estado entre llamadas.
//
// Se cachea junto al resto de metadatos del snapshot: está en el camino caliente
// y solo cambia al reimportar el servicio.
func (a *aggregator) isStateful(ctx context.Context, service string) bool {
	a.snapMu.RLock()
	v, ok := a.stateful[service]
	a.snapMu.RUnlock()
	if ok {
		return v
	}

	snaps, err := a.gw.client.Snapshots(ctx)
	if err != nil {
		return false
	}
	a.snapMu.Lock()
	for _, s := range snaps {
		if svc := s.Service(); svc != "" {
			a.stateful[svc] = s.Stateful()
		}
		a.stateful[s.Name] = s.Stateful()
	}
	v = a.stateful[service]
	a.snapMu.Unlock()
	return v
}
