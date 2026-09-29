package main

// kling db ask-web: la misma garantía de kling db ask en una página web mínima,
// para quien no usa la terminal. Ver docs/db-ask.md.
//
// Lo que NO cambia respecto a ask: al modelo solo va el esquema y la pregunta;
// la SQL pasa por sqlguard, se enseña y solo se ejecuta cuando el usuario pulsa
// el botón (la petición de ejecutar lleva el id de la propuesta, no una SQL:
// lo que se ejecuta es lo que se enseñó), con el rol de solo lectura en
// BEGIN TRANSACTION READ ONLY con statement_timeout; -explain exige -send-data.
//
// Lo propio de la web: escucha solo en loopback; un token aleatorio en la URL
// impresa (pasa a una cookie SameSite=Strict HttpOnly); un token CSRF aparte en
// cada POST; comprobación de Host (el de la dirección de escucha, también con
// -allow-remote) y Origin; CSP sin inline (el JS y el CSS son
// ficheros propios); ni CORS ni redirecciones a otro sitio; vida acotada y tope
// de peticiones.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/db/internal/askllm"
	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/ext/db/internal/sqlguard"
)

//go:embed web/index.html
var webIndex string

//go:embed web/app.js
var webJS []byte

//go:embed web/app.css
var webCSS []byte

const (
	webCookie       = "kling_ask"
	webMaxBody      = 16 << 10
	webMaxPending   = 20
	webPendingTTL   = 10 * time.Minute
	webMaxAPICalls  = 500
	webPerMinute    = 30
	webMaxErrorLen  = 600
	webDefaultTTL   = 30 * time.Minute
	webCSP          = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	webDefaultAddr  = "127.0.0.1:0"
	webRedactedMark = "[redacted]"
)

type webOpts struct {
	askOpts
	listen      string
	allowRemote bool
	ttl         time.Duration
}

func cmdAskWeb(args []string) error {
	fs, host, owner := newFlags("ask-web")
	var o webOpts
	fs.StringVar(&o.role, "role", "", "read-only role to run as (default: "+defaultRORole+", created if missing)")
	fs.StringVar(&o.provider, "provider", "", "model provider: anthropic or opencode (default as in ask)")
	fs.StringVar(&o.model, "model", "", "model (default: "+askllm.DefaultModel+" for anthropic, "+askllm.DefaultOpenCodeModel+" for opencode)")
	fs.DurationVar(&o.llmTime, "llm-timeout", askllm.DefaultLLMTimeout, "how long to wait for the model (opencode provider)")
	fs.IntVar(&o.limit, "limit", askDefaultLimit, fmt.Sprintf("maximum rows returned (1-%d)", askMaxLimit))
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "statement_timeout of the query")
	fs.BoolVar(&o.explain, "explain", false, "summarize each result with the model (needs -send-data)")
	fs.BoolVar(&o.sendData, "send-data", false, "allow -explain to send up to 50 result rows to the model provider")
	fs.StringVar(&o.listen, "listen", webDefaultAddr, "address to listen on (loopback only unless -allow-remote)")
	fs.BoolVar(&o.allowRemote, "allow-remote", false, "accept a non-loopback -listen address (plain HTTP: anyone who sees the URL can query the copy)")
	fs.DurationVar(&o.ttl, "ttl", webDefaultTTL, "how long the page stays up (1m-8h)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr(`usage: kling db ask-web <copy> [-listen 127.0.0.1:PORT] [-role R] [-provider P] [-model M] [-limit N] [-ttl D] [-explain -send-data]`)
	}
	if err := o.check("placeholder"); err != nil {
		return usageErr("%v", err)
	}
	if o.ttl < time.Minute || o.ttl > 8*time.Hour {
		return usageErr("-ttl must be between 1m and 8h")
	}
	addr, err := checkListen(o.listen, o.allowRemote)
	if err != nil {
		return usageErr("%v", err)
	}
	prov, err := newProvider(o.provider, o.model, o.llmTime)
	if err != nil {
		return err
	}
	a, err := newApp(*host)
	if err != nil {
		return err
	}
	ctx, stop := signalCtx()
	defer stop()
	sess, err := a.askPrepare(ctx, pos[0], *owner, o.role)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	w, err := newAskWeb(a, sess, prov, o, ln.Addr().String())
	if err != nil {
		ln.Close()
		return err
	}
	if o.allowRemote && !isLoopbackAddr(ln.Addr().String()) {
		fmt.Fprintf(a.stderr, "WARNING: listening on %s without TLS: anyone who reaches the port AND has the URL can query %s. Prefer an SSH tunnel to the loopback address.\n", ln.Addr(), sess.mc.Name)
	}
	fmt.Fprintf(a.stderr, "kling db ask-web: the model gets the schema and the question, no data; every query is shown and runs only when you press the button, read-only as %s.\n", sess.ro)
	if o.explain {
		fmt.Fprintf(a.stderr, "-explain -send-data: up to %d rows of each result go to %s.\n", askExplainRows, prov.Name())
	}
	fmt.Fprintf(a.stderr, "open this address (it carries a secret token; it expires in %s, Ctrl-C stops it):\n", o.ttl)
	fmt.Fprintf(a.stdout, "http://%s/?t=%s\n", ln.Addr(), w.token)
	return w.serve(ctx, ln)
}

// checkListen valida -listen: solo loopback salvo allowRemote.
func checkListen(listen string, allowRemote bool) (string, error) {
	h, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("invalid -listen %q: want HOST:PORT", listen)
	}
	if h == "localhost" {
		h = "127.0.0.1"
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return "", fmt.Errorf("invalid -listen %q: the host must be an IP address (or localhost)", listen)
	}
	if !ip.IsLoopback() && !allowRemote {
		return "", fmt.Errorf("-listen %s is not a loopback address: the page has no TLS; use 127.0.0.1 (and an SSH tunnel) or add -allow-remote", listen)
	}
	// La página solo contesta al Host por el que escucha (contra DNS
	// rebinding, también con -allow-remote): con 0.0.0.0 o :: no hay uno.
	if ip.IsUnspecified() {
		return "", fmt.Errorf("-listen %s listens on every interface: name the address the browser will use (the page only answers to that Host)", listen)
	}
	return net.JoinHostPort(h, p), nil
}

func isLoopbackAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

type proposal struct {
	question, sql string
	at            time.Time
}

type askWeb struct {
	a    *app
	sess *askSession
	prov askllm.Provider
	o    webOpts

	token, csrf string
	pw          string // solo para tapar cualquier aparición en las respuestas
	hosts       map[string]bool
	now         func() time.Time
	expires     time.Time

	mu      sync.Mutex // serializa modelo y ejecución: una a la vez
	pending map[string]*proposal
	calls   int
	recent  []time.Time
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newAskWeb(a *app, sess *askSession, prov askllm.Provider, o webOpts, listenAddr string) (*askWeb, error) {
	tok, err := randHex(24)
	if err != nil {
		return nil, err
	}
	csrf, err := randHex(24)
	if err != nil {
		return nil, err
	}
	w := &askWeb{a: a, sess: sess, prov: prov, o: o, token: tok, csrf: csrf,
		pending: map[string]*proposal{}, now: time.Now, hosts: map[string]bool{}}
	w.expires = w.now().Add(o.ttl)
	// La clave de la copia no sale nunca; si se puede leer, se tapa en lo que
	// sale por si un dato, un error o el modelo la dijeran.
	if pw, err := dbstate.ReadPassword(sess.mc.ID); err == nil && len(pw) >= 4 {
		w.pw = pw
	}
	// Contra DNS rebinding: solo se sirve con el Host por el que se escucha
	// (con loopback, cualquiera de sus nombres; con -allow-remote, esa IP).
	h, port, _ := net.SplitHostPort(listenAddr)
	if isLoopbackAddr(listenAddr) {
		for _, n := range []string{"127.0.0.1", "localhost", "[::1]"} {
			w.hosts[n+":"+port] = true
		}
	} else {
		w.hosts[strings.ToLower(net.JoinHostPort(h, port))] = true
	}
	return w, nil
}

func (w *askWeb) serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           w.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      w.o.llmTime + w.o.timeout + time.Minute,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	ctx, cancel := context.WithDeadline(ctx, w.expires)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sh, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = srv.Shutdown(sh)
	if w.now().Before(w.expires) {
		return nil // Ctrl-C
	}
	fmt.Fprintln(w.a.stderr, "kling db ask-web: the time is up, stopped.")
	return nil
}

// Handler es el servidor entero; los tests lo usan con httptest.
func (w *askWeb) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", w.index)
	mux.HandleFunc("GET /app.js", w.static("text/javascript; charset=utf-8", webJS))
	mux.HandleFunc("GET /app.css", w.static("text/css; charset=utf-8", webCSS))
	mux.HandleFunc("GET /api/info", w.info)
	mux.HandleFunc("POST /api/propose", w.propose)
	mux.HandleFunc("POST /api/run", w.run)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h := rw.Header()
		h.Set("Content-Security-Policy", webCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if !w.now().Before(w.expires) {
			w.fail(rw, http.StatusGone, "this page has expired")
			return
		}
		if !w.hostOK(r) {
			w.fail(rw, http.StatusForbidden, "forbidden")
			return
		}
		mux.ServeHTTP(rw, r)
	})
}

func (w *askWeb) hostOK(r *http.Request) bool { return w.hosts[strings.ToLower(r.Host)] }

func eq(a, b string) bool { return len(a) > 0 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// authed: la cookie con el token, o (solo la primera vez) ?t=.
func (w *askWeb) authed(r *http.Request) bool {
	c, err := r.Cookie(webCookie)
	return err == nil && eq(c.Value, w.token)
}

// authedPost añade lo de las peticiones que cambian algo: el token CSRF en la
// cabecera, JSON, y que el navegador diga que la petición es del propio sitio.
func (w *askWeb) authedPost(r *http.Request) bool {
	if !w.authed(r) || !eq(r.Header.Get("X-CSRF"), w.csrf) {
		return false
	}
	if mt, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(strings.ToLower(mt)) != "application/json" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" {
		return false
	}
	return true
}

func (w *askWeb) fail(rw http.ResponseWriter, code int, msg string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(map[string]string{"error": w.clean(msg)})
}

func (w *askWeb) ok(rw http.ResponseWriter, v any) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(v)
}

// clean deja un texto sin caracteres de control, acotado y sin la clave.
func (w *askWeb) clean(s string) string {
	s = printable(s)
	if w.pw != "" {
		s = strings.ReplaceAll(s, w.pw, webRedactedMark)
	}
	if len(s) > webMaxErrorLen {
		s = strings.ToValidUTF8(s[:webMaxErrorLen], "") + "..."
	}
	return s
}

// redact tapa la clave en un valor que puede venir de la base (celdas, resumen).
func (w *askWeb) redact(s string) string {
	if w.pw == "" {
		return s
	}
	return strings.ReplaceAll(s, w.pw, webRedactedMark)
}

// index entrega la página. Con ?t= correcto pone la cookie y redirige a /, para
// que el token no se quede en la barra de direcciones ni en el historial.
func (w *askWeb) index(rw http.ResponseWriter, r *http.Request) {
	if t := r.URL.Query().Get("t"); t != "" {
		if !eq(t, w.token) {
			w.fail(rw, http.StatusForbidden, "forbidden")
			return
		}
		http.SetCookie(rw, &http.Cookie{Name: webCookie, Value: w.token, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, Expires: w.expires, MaxAge: max(1, int(w.expires.Sub(w.now()).Seconds()))})
		http.Redirect(rw, r, "/", http.StatusSeeOther)
		return
	}
	if !w.authed(r) {
		w.fail(rw, http.StatusForbidden, "forbidden")
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(rw, strings.Replace(webIndex, "{{CSRF}}", w.csrf, 1))
}

func (w *askWeb) static(ct string, body []byte) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !w.authed(r) {
			w.fail(rw, http.StatusForbidden, "forbidden")
			return
		}
		rw.Header().Set("Content-Type", ct)
		_, _ = rw.Write(body)
	}
}

func (w *askWeb) info(rw http.ResponseWriter, r *http.Request) {
	if !w.authed(r) {
		w.fail(rw, http.StatusForbidden, "forbidden")
		return
	}
	w.ok(rw, map[string]any{"copy": w.sess.mc.Name, "role": w.sess.ro, "provider": w.prov.Name(),
		"explain": w.o.explain, "explain_rows": askExplainRows})
}

// gate: autenticación, tope de peticiones y cuerpo JSON acotado. Devuelve false
// si ya contestó.
func (w *askWeb) gate(rw http.ResponseWriter, r *http.Request, v any) bool {
	if !w.authedPost(r) {
		w.fail(rw, http.StatusForbidden, "forbidden")
		return false
	}
	w.mu.Lock()
	now := w.now()
	keep := w.recent[:0]
	for _, t := range w.recent {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	w.recent = keep
	limited := w.calls >= webMaxAPICalls || len(w.recent) >= webPerMinute
	if !limited {
		w.calls++
		w.recent = append(w.recent, now)
	}
	w.mu.Unlock()
	if limited {
		rw.Header().Set("Retry-After", "60")
		w.fail(rw, http.StatusTooManyRequests, "too many requests")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(rw, r.Body, webMaxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		w.fail(rw, http.StatusBadRequest, "bad request")
		return false
	}
	return true
}

// propose: pregunta -> modelo -> SQL validada, guardada con un id. No ejecuta nada.
func (w *askWeb) propose(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Question string `json:"question"`
	}
	if !w.gate(rw, r, &req) {
		return
	}
	q := strings.TrimSpace(req.Question)
	if err := (askOpts{limit: 1, timeout: time.Second, llmTime: time.Second}).check(q); err != nil {
		w.fail(rw, http.StatusBadRequest, err.Error())
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	fmt.Fprintf(w.a.stderr, "ask-web: asking %s (schema and question, no data)...\n", w.prov.Name())
	answer, err := w.prov.Complete(r.Context(), sqlSystemPrompt, sqlPrompt(w.sess.schema, q))
	if err != nil {
		w.fail(rw, http.StatusBadGateway, err.Error())
		return
	}
	sql, err := sqlguard.Extract(answer)
	if err == nil {
		err = validateSQL(sql)
	}
	if err == nil {
		_, err = readOnlyQuery(w.sess.ro, sql, w.o.limit, w.o.timeout) // la barra invertida, también aquí
	}
	if err != nil {
		w.fail(rw, http.StatusUnprocessableEntity, err.Error())
		return
	}
	id, err := randHex(16)
	if err != nil {
		w.fail(rw, http.StatusInternalServerError, "internal error")
		return
	}
	w.gc()
	w.pending[id] = &proposal{question: q, sql: sql, at: w.now()}
	fmt.Fprintf(w.a.stderr, "ask-web: proposed\n%s\n", indent(printable(sql)))
	w.ok(rw, map[string]string{"id": id, "sql": w.redact(printable(sql)), "role": w.sess.ro})
}

// gc suelta las propuestas caducadas y, si hay demasiadas, las más viejas.
// Con w.mu.
func (w *askWeb) gc() {
	now := w.now()
	for id, p := range w.pending {
		if now.Sub(p.at) > webPendingTTL {
			delete(w.pending, id)
		}
	}
	for len(w.pending) >= webMaxPending {
		oldest, oid := now, ""
		for id, p := range w.pending {
			if oid == "" || p.at.Before(oldest) {
				oldest, oid = p.at, id
			}
		}
		delete(w.pending, oid)
	}
}

// run: ejecuta la propuesta id, y solo con confirm:true. La SQL es la guardada,
// nunca una que llegue en la petición. Cada propuesta vale una vez.
func (w *askWeb) run(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string `json:"id"`
		Confirm bool   `json:"confirm"`
	}
	if !w.gate(rw, r, &req) {
		return
	}
	if !req.Confirm {
		w.fail(rw, http.StatusBadRequest, "not confirmed: nothing was run")
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.pending[req.ID]
	delete(w.pending, req.ID)
	if p == nil || w.now().Sub(p.at) > webPendingTTL {
		w.fail(rw, http.StatusNotFound, "unknown or expired query: ask again")
		return
	}
	// Se revalida: el coste es nulo y no depende de lo que hubiera al guardarla.
	if err := validateSQL(p.sql); err != nil {
		w.fail(rw, http.StatusUnprocessableEntity, err.Error())
		return
	}
	fmt.Fprintf(w.a.stderr, "ask-web: confirmed, running as %s\n", w.sess.ro)
	res, err := w.a.runReadOnly(r.Context(), w.sess.mc, w.sess.db, w.sess.ro, p.sql, w.o.limit, w.o.timeout)
	if err != nil {
		w.fail(rw, http.StatusBadGateway, err.Error())
		return
	}
	res.SQL, res.Role = p.sql, w.sess.ro
	if w.o.explain {
		if err := w.a.askExplain(r.Context(), w.prov, p.question, res); err != nil {
			// Las filas ya se leyeron: se devuelven sin resumen.
			fmt.Fprintf(w.a.stderr, "ask-web: %v\n", err)
		}
	}
	for i, c := range res.Columns {
		res.Columns[i] = w.redact(printable(c))
	}
	for _, row := range res.Rows {
		for i, c := range row {
			row[i] = w.redact(printable(c))
		}
	}
	res.SQL, res.Summary = w.redact(res.SQL), w.redact(printable(res.Summary))
	w.ok(rw, res)
}
