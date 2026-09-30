package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El servidor MCP de los teléfonos (`kling phone mcp`).
//
// DISEÑO. Habla MCP por HTTP (streamable HTTP, respuestas JSON) en el
// anfitrión, y cada herramienta es una llamada a la API de kling-phoned por el
// proxy del daemon, con el token del clon. La extensión `mcp` lo importa como
// servidor externo enlazado:
//
//	kling mcp link phone http://127.0.0.1:8095/<secreto>/mcp
//
// y su gateway lo expone como phone.screen, phone.tree, phone.tap... Las dos
// extensiones no comparten código: se hablan por el núcleo (el enlace vive en
// el store del daemon) y por MCP.
//
// UN TELÉFONO POR SESIÓN. El gateway abre una sesión propia contra un enlace
// por cada conversación (aggregate.go, callLink), así que aquí cada
// Mcp-Session-Id recibe SU teléfono, perezosamente: la primera herramienta que
// lo necesita reclama un repuesto del fondo (`kling phone pool`: resume de ms)
// o restaura uno nuevo del dorado con identidad y token propios (~2-7 s). Sin
// uso durante -idle se pausa (no gasta CPU; vuelve en ms) y, a los
// -session-ttl, se borra: lo que una conversación hizo en su teléfono no lo
// hereda otra. initialize y tools/list no crean nada (es lo que hace `kling
// mcp link` para leer el catálogo).
//
// Por qué no kling-phoned sirviendo MCP dentro del invitado e importado con
// `kling mcp import -isolation session`: el gateway habla MCP solo con el
// 8080 del invitado (el agente de kindling, no kling-phoned), y su máquina por
// sesión es un `run -from` sin la identidad ni el token de cada clon, que son
// cosa de esta extensión. Y por qué no pkg/scheduler para el fondo: sus
// instancias nacen con runFresh (sin gancho de identidad) y su segador congela
// por el tráfico que él mismo reenvía; aquí el tráfico es la API del teléfono.

const mcpProtocol = "2025-06-18"

type mcpServer struct {
	a       *app
	golden  string
	fixed   string // -phone: todas las sesiones con ese teléfono
	idle    time.Duration
	ttl     time.Duration
	pool    int
	claimMu sync.Mutex

	mu       sync.Mutex
	sessions map[string]*mcpSession
	filling  bool
}

type mcpSession struct {
	id string

	mu      sync.Mutex // una herramienta a la vez por sesión
	phone   *api.Machine
	how     string // spare | new | fixed
	paused  bool
	lastUse time.Time
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// owner es la etiqueta de dueño de una sesión (sin el id entero: la etiqueta
// la ve cualquiera con `kling ps`).
func (s *mcpSession) owner() string { return "mcp-" + s.id[:12] }

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ── herramientas ─────────────────────────────────────────────────────────────

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

var mcpTools = []mcpTool{
	{Name: "screen", Description: "Screenshot of this conversation's Android phone (PNG, 720x1280 by default). The first call gives the conversation its own phone.",
		InputSchema: obj(map[string]any{})},
	{Name: "tree", Description: "The UI hierarchy of the phone's screen as XML (uiautomator format): texts, resource ids, bounds [x1,y1][x2,y2] to tap.",
		InputSchema: obj(map[string]any{"compressed": map[string]any{"type": "boolean", "description": "only the meaningful nodes (default true)"}})},
	{Name: "tap", Description: "Taps the screen at x,y (screen pixels, as in the screenshot and the bounds of tree).",
		InputSchema: obj(map[string]any{"x": intProp("x in pixels"), "y": intProp("y in pixels")}, "x", "y")},
	{Name: "swipe", Description: "Swipes from x1,y1 to x2,y2 in ms milliseconds (scroll, open the app drawer...).",
		InputSchema: obj(map[string]any{"x1": intProp("start x"), "y1": intProp("start y"), "x2": intProp("end x"), "y2": intProp("end y"),
			"ms": intProp("duration in ms (default 300)")}, "x1", "y1", "x2", "y2")},
	{Name: "text", Description: "Types a single line of text into the focused field (tap the field first; use key ENTER to submit).",
		InputSchema: obj(map[string]any{"text": strProp("the text, one line")}, "text")},
	{Name: "key", Description: "Presses a key: BACK, HOME, ENTER, APP_SWITCH, DEL, WAKEUP... or an Android key code number.",
		InputSchema: obj(map[string]any{"key": strProp("key name or code")}, "key")},
	{Name: "install", Description: "Installs (or updates) an APK on the phone. The APK goes base64-encoded.",
		InputSchema: obj(map[string]any{"apk_base64": strProp("the APK file, base64")}, "apk_base64")},
	{Name: "launch", Description: "Opens an installed app by its package name (its launcher activity), e.g. com.android.settings.",
		InputSchema: obj(map[string]any{"package": strProp("the package name")}, "package")},
}

func textContent(s string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": s}}}
}

func toolError(err error) map[string]any {
	r := textContent(err.Error())
	r["isError"] = true
	return r
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

func (s *mcpServer) handler(path string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "loopback host names only", http.StatusForbidden)
			return
		}
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		sid := r.Header.Get("Mcp-Session-Id")
		switch r.Method {
		case http.MethodPost:
		case http.MethodDelete:
			if sid == "" || !s.closeSession(r.Context(), sid) {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			// Sin flujo SSE de servidor a cliente: nada que empujar.
			w.Header().Set("Allow", "POST, DELETE")
			http.Error(w, "POST or DELETE", http.StatusMethodNotAllowed)
			return
		}
		// application/json: un formulario de otra web no puede mandarlo sin
		// que el navegador pregunte antes.
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req rpcReq
		if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
			writeRPC(w, nil, nil, &rpcErr{-32700, "parse error: one JSON-RPC request per POST"})
			return
		}
		if req.Method == "initialize" {
			ns := &mcpSession{id: newSessionID(), lastUse: time.Now()}
			s.mu.Lock()
			s.sessions[ns.id] = ns
			s.mu.Unlock()
			w.Header().Set("Mcp-Session-Id", ns.id)
			writeRPC(w, req.ID, s.initialize(req.Params), nil)
			return
		}
		s.mu.Lock()
		sess := s.sessions[sid]
		s.mu.Unlock()
		if sess == nil {
			// 404: el cliente (el gateway) rehace el initialize.
			http.Error(w, "unknown or expired Mcp-Session-Id", http.StatusNotFound)
			return
		}
		if len(req.ID) == 0 { // notificación
			w.WriteHeader(http.StatusAccepted)
			return
		}
		res, rerr := s.dispatch(r.Context(), sess, req)
		writeRPC(w, req.ID, res, rerr)
	})
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *rpcErr) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		out["error"] = e
	} else {
		out["result"] = result
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *mcpServer) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	v := p.ProtocolVersion
	if v == "" {
		v = mcpProtocol
	}
	return map[string]any{
		"protocolVersion": v,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "kling-phone", "version": manifest().Version},
		"instructions": "Each conversation gets its own Android phone (a kindling microVM). Look with screen and tree, " +
			"act with tap, swipe, text and key; coordinates are screen pixels (the bounds in tree).",
	}
}

func (s *mcpServer) dispatch(ctx context.Context, sess *mcpSession, req rpcReq) (any, *rpcErr) {
	switch req.Method {
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcErr{-32602, "bad params"}
		}
		known := false
		for _, t := range mcpTools {
			known = known || t.Name == p.Name
		}
		if !known {
			return nil, &rpcErr{-32602, "no such tool: " + p.Name}
		}
		return s.call(ctx, sess, p.Name, p.Arguments), nil
	}
	return nil, &rpcErr{-32601, "method not found: " + req.Method}
}

// phoneOf da el teléfono de la sesión, reclamándolo o despertándolo.
func (s *mcpServer) phoneOf(ctx context.Context, sess *mcpSession) (*api.Machine, error) {
	if sess.phone == nil {
		t0 := time.Now()
		var m *api.Machine
		var how string
		var err error
		if s.fixed != "" {
			m, err = s.a.phone(ctx, s.fixed)
			how = "fixed"
			if err == nil && m.State != api.StateRunning {
				m, err = s.a.resume(ctx, m)
			}
		} else {
			m, how, err = s.a.claim(ctx, s.golden, sess.owner(), &s.claimMu)
		}
		if err != nil {
			return nil, err
		}
		sess.phone, sess.how = m, how
		log.Printf("session %s: %s (%s, %s)", sess.id[:8], m.Name, how, time.Since(t0).Round(time.Millisecond))
		s.refill()
		return m, nil
	}
	if sess.paused {
		t0 := time.Now()
		m, err := s.a.d.Get(ctx, sess.phone.ID)
		if err != nil {
			return nil, fmt.Errorf("this conversation's phone is gone (%v): start a new conversation", err)
		}
		if m, err = s.a.resume(ctx, m); err != nil {
			return nil, err
		}
		sess.phone, sess.paused = m, false
		log.Printf("session %s: %s resumed in %s", sess.id[:8], m.Name, time.Since(t0).Round(time.Millisecond))
	}
	return sess.phone, nil
}

func (s *mcpServer) call(ctx context.Context, sess *mcpSession, tool string, args json.RawMessage) map[string]any {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.lastUse = time.Now()
	defer func() { sess.lastUse = time.Now() }()
	m, err := s.phoneOf(ctx, sess)
	if err != nil {
		return toolError(err)
	}
	var a map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return toolError(errors.New("arguments must be a JSON object"))
		}
	}
	post := func(path string, body any) (*phoneResp, error) {
		b, _ := json.Marshal(body)
		return s.a.callPhone(ctx, m, "POST", path, b, false)
	}
	num := func(k string, def int) (int, error) {
		v, ok := a[k]
		if !ok {
			if def >= 0 {
				return def, nil
			}
			return 0, fmt.Errorf("%s is required", k)
		}
		f, ok := v.(float64)
		if !ok {
			return 0, fmt.Errorf("%s must be a number", k)
		}
		return int(f), nil
	}
	str := func(k string) (string, error) {
		v, ok := a[k].(string)
		if !ok || v == "" {
			return "", fmt.Errorf("%s is required (a string)", k)
		}
		return v, nil
	}
	done := func(r *phoneResp, err error) map[string]any {
		if err != nil {
			return toolError(err)
		}
		return textContent(m.Name + ": " + strings.TrimSpace(string(r.Body)))
	}
	switch tool {
	case "screen":
		r, err := s.a.callPhone(ctx, m, "GET", "/v1/screen", nil, false)
		if err != nil {
			return toolError(err)
		}
		return map[string]any{"content": []map[string]any{
			{"type": "image", "data": base64.StdEncoding.EncodeToString(r.Body), "mimeType": "image/png"},
			{"type": "text", "text": m.Name},
		}}
	case "tree":
		path := "/v1/tree?compressed=1"
		if c, ok := a["compressed"].(bool); ok && !c {
			path = "/v1/tree"
		}
		r, err := s.a.callPhone(ctx, m, "GET", path, nil, false)
		if err != nil {
			return toolError(err)
		}
		return textContent(string(r.Body))
	case "tap":
		x, err1 := num("x", -1)
		y, err2 := num("y", -1)
		if err := errors.Join(err1, err2); err != nil {
			return toolError(err)
		}
		return done(post("/v1/tap", map[string]int{"x": x, "y": y}))
	case "swipe":
		var v [5]int
		for i, k := range []string{"x1", "y1", "x2", "y2", "ms"} {
			def := -1
			if k == "ms" {
				def = 300
			}
			n, err := num(k, def)
			if err != nil {
				return toolError(err)
			}
			v[i] = n
		}
		return done(post("/v1/swipe", map[string]int{"x1": v[0], "y1": v[1], "x2": v[2], "y2": v[3], "ms": v[4]}))
	case "text":
		t, err := str("text")
		if err != nil {
			return toolError(err)
		}
		return done(post("/v1/text", map[string]string{"text": t}))
	case "key":
		var k string
		switch v := a["key"].(type) {
		case string:
			k = v
		case float64:
			k = fmt.Sprint(int(v))
		default:
			return toolError(errors.New("key is required"))
		}
		return done(post("/v1/key", map[string]string{"key": k}))
	case "install":
		b64, err := str("apk_base64")
		if err != nil {
			return toolError(err)
		}
		apk, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return toolError(errors.New("apk_base64 is not valid base64"))
		}
		return done(s.a.callPhone(ctx, m, "POST", "/v1/install", apk, true))
	case "launch":
		p, err := str("package")
		if err != nil {
			return toolError(err)
		}
		return done(post("/v1/launch", map[string]string{"package": p}))
	}
	return toolError(fmt.Errorf("no such tool %s", tool))
}

// ── ciclo de vida de las sesiones ────────────────────────────────────────────

// closeSession suelta la sesión y su teléfono (DELETE del cliente).
func (s *mcpServer) closeSession(ctx context.Context, sid string) bool {
	s.mu.Lock()
	sess := s.sessions[sid]
	delete(s.sessions, sid)
	s.mu.Unlock()
	if sess == nil {
		return false
	}
	s.release(ctx, sess, "closed")
	return true
}

// release borra el teléfono de la sesión (no el de -phone): lo que hizo una
// conversación en su teléfono no lo hereda otra.
func (s *mcpServer) release(ctx context.Context, sess *mcpSession, why string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.phone == nil || sess.how == "fixed" {
		return
	}
	if err := s.a.rm(ctx, sess.phone); err != nil {
		log.Printf("session %s: removing %s: %v", sess.id[:8], sess.phone.Name, err)
		return
	}
	log.Printf("session %s: %s removed (%s)", sess.id[:8], sess.phone.Name, why)
	sess.phone = nil
}

// reap pausa los teléfonos ociosos y cierra las sesiones caducadas.
func (s *mcpServer) reap(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	var expired, idle []*mcpSession
	for id, sess := range s.sessions {
		if !sess.mu.TryLock() {
			continue // una herramienta en curso: no está ociosa
		}
		age := now.Sub(sess.lastUse)
		switch {
		case age > s.ttl:
			expired = append(expired, sess)
			delete(s.sessions, id)
		case age > s.idle && sess.phone != nil && !sess.paused && sess.how != "fixed":
			idle = append(idle, sess)
		}
		sess.mu.Unlock()
	}
	s.mu.Unlock()
	for _, sess := range expired {
		s.release(ctx, sess, "session expired")
	}
	for _, sess := range idle {
		sess.mu.Lock()
		if sess.phone != nil && !sess.paused {
			if _, err := s.a.d.Pause(ctx, sess.phone.ID); err == nil {
				sess.paused = true
				log.Printf("session %s: %s paused (idle %s)", sess.id[:8], sess.phone.Name, s.idle)
			}
		}
		sess.mu.Unlock()
	}
}

// refill repone el fondo en segundo plano tras reclamar un repuesto.
func (s *mcpServer) refill() {
	if s.pool <= 0 {
		return
	}
	s.mu.Lock()
	if s.filling {
		s.mu.Unlock()
		return
	}
	s.filling = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			s.filling = false
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		a := *s.a
		a.out = io.Discard
		if err := a.poolFill(ctx, s.golden, s.pool); err != nil {
			log.Printf("pool: %v", err)
		}
	}()
}

func (s *mcpServer) shutdown(ctx context.Context) {
	s.mu.Lock()
	all := make([]*mcpSession, 0, len(s.sessions))
	for id, sess := range s.sessions {
		all = append(all, sess)
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	for _, sess := range all {
		s.release(ctx, sess, "server stopping")
	}
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	host := hostFlag(fs)
	listen := fs.String("listen", "127.0.0.1:8095", "where the MCP server listens (loopback: it controls phones)")
	golden := fs.String("golden", "", "golden of the per-session phones (default: phone.golden)")
	fixed := fs.String("phone", "", "one existing phone for every session (no per-session phones)")
	idle := fs.Duration("idle", 2*time.Minute, "a session's phone unused this long is paused (resume in ms)")
	ttl := fs.Duration("session-ttl", 15*time.Minute, "a session unused this long is closed and its phone removed")
	pool := fs.Int("pool", 0, "keep N paused spare phones for new sessions (kling phone pool)")
	secretPath := fs.Bool("secret-path", true, "serve under a random path (/<secret>/mcp): other local processes can't guess it")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	ctx, stop := ctxWithSignals()
	defer stop()
	a := newApp(*host)
	s := &mcpServer{a: a, golden: or(*golden, a.s.Golden), fixed: *fixed, idle: *idle, ttl: *ttl, pool: *pool,
		sessions: map[string]*mcpSession{}}
	if s.fixed != "" {
		if _, err := a.phone(ctx, s.fixed); err != nil {
			return err
		}
	} else if err := a.ensureGolden(ctx, s.golden); err != nil {
		return err
	}
	path := "/mcp"
	if *secretPath {
		path = "/" + newSessionID() + "/mcp"
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.handler(path), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reap(ctx)
			}
		}
	}()
	if s.pool > 0 {
		s.refill()
	}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	u := "http://" + ln.Addr().String() + path
	fmt.Fprintf(a.out, "kling phone mcp: %s\n", u)
	fmt.Fprintf(a.out, "import it into the MCP gateway (one phone per session):\n  kling mcp link phone %s\n", u)
	log.SetFlags(log.LstdFlags)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	c, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s.shutdown(c)
	return nil
}
