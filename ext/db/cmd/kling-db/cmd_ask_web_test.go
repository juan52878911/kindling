package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

const webTestAddr = "127.0.0.1:8765"

type webRig struct {
	t   *testing.T
	w   *askWeb
	k   *askKling
	p   *fakeProvider
	pw  string
	h   http.Handler
	now time.Time
}

func newWebRig(t *testing.T, answers ...string) *webRig {
	t.Helper()
	ta, k, pw := newAskApp(t)
	sess, err := ta.app.askPrepare(ctx, "c1", "local", "")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{answers: answers}
	o := webOpts{askOpts: defaultAskOpts(), listen: webTestAddr, ttl: time.Hour}
	o.yes = false
	w, err := newAskWeb(ta.app, sess, p, o, webTestAddr)
	if err != nil {
		t.Fatal(err)
	}
	r := &webRig{t: t, w: w, k: k, p: p, pw: pw, now: time.Now()}
	w.now = func() time.Time { return r.now }
	r.h = w.Handler()
	return r
}

// do hace una petición como el navegador: con Host de loopback y, si auth,
// con la cookie (y el CSRF en los POST).
func (r *webRig) do(method, path, body string, auth bool, mut ...func(*http.Request)) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = webTestAddr
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.AddCookie(&http.Cookie{Name: webCookie, Value: r.w.token})
		if method == http.MethodPost {
			req.Header.Set("X-CSRF", r.w.csrf)
		}
	}
	for _, m := range mut {
		m(req)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *webRig) propose(q string) (id, sql string, code int) {
	r.t.Helper()
	rec := r.do("POST", "/api/propose", `{"question":`+jsonStr(q)+`}`, true)
	var j struct{ ID, SQL string }
	_ = json.Unmarshal(rec.Body.Bytes(), &j)
	return j.ID, j.SQL, rec.Code
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

const webSQL = "SELECT name FROM customers ORDER BY name"

func TestWebSinTokenEs403(t *testing.T) {
	r := newWebRig(t, webSQL)
	for _, c := range []struct{ m, p string }{
		{"GET", "/"}, {"GET", "/app.js"}, {"GET", "/app.css"}, {"GET", "/api/info"},
		{"POST", "/api/propose"}, {"POST", "/api/run"}, {"GET", "/?t=malo"},
	} {
		rec := r.do(c.m, c.p, `{}`, false)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s sin token: %d", c.m, c.p, rec.Code)
		}
	}
	// Con la cookie pero sin CSRF, o con otro CSRF: 403 y nada llega al modelo.
	for _, mut := range []func(*http.Request){
		func(q *http.Request) { q.Header.Del("X-CSRF") },
		func(q *http.Request) { q.Header.Set("X-CSRF", "x") },
		func(q *http.Request) { q.Header.Set("X-CSRF", r.w.token) },
		func(q *http.Request) { q.Header.Set("Content-Type", "text/plain") },
		func(q *http.Request) { q.Header.Set("Origin", "http://evil.example") },
		func(q *http.Request) { q.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		if rec := r.do("POST", "/api/propose", `{"question":"q"}`, true, mut); rec.Code != http.StatusForbidden {
			t.Fatalf("post sin csrf o de otro origen: %d", rec.Code)
		}
	}
	// Host ajeno (DNS rebinding): 403 aun con todo lo demás.
	if rec := r.do("GET", "/", "", true, func(q *http.Request) { q.Host = "evil.example:8765" }); rec.Code != http.StatusForbidden {
		t.Fatalf("host ajeno: %d", rec.Code)
	}
	if len(r.p.prompts) != 0 || len(r.k.execs) != 4 { // 4: preparar la copia
		t.Fatalf("something reached the model (%d) or the copy (%d execs)", len(r.p.prompts), len(r.k.execs))
	}
}

func TestWebTokenEnLaURLPasaACookie(t *testing.T) {
	r := newWebRig(t)
	rec := r.do("GET", "/?t="+r.w.token, "", false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
	ck := rec.Result().Cookies()
	if len(ck) != 1 || ck[0].Name != webCookie || ck[0].Value != r.w.token || !ck[0].HttpOnly ||
		ck[0].SameSite != http.SameSiteStrictMode || ck[0].Path != "/" {
		t.Fatalf("cookie %+v", ck)
	}
	page := r.do("GET", "/", "", true)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `content="`+r.w.csrf+`"`) {
		t.Fatalf("page %d", page.Code)
	}
	if strings.Contains(page.Body.String(), r.w.token) {
		t.Fatal("the URL token is in the page")
	}
}

func TestWebCabecerasYSinInline(t *testing.T) {
	r := newWebRig(t)
	rec := r.do("GET", "/", "", true)
	h := rec.Header()
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("csp %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe") {
		t.Fatalf("csp %q", csp)
	}
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY",
		"Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
		if h.Get(k) != v {
			t.Fatalf("%s = %q", k, h.Get(k))
		}
	}
	if h.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS header")
	}
	body := rec.Body.String()
	// Sin JS ni estilos en línea, ni manejadores on*, ni recursos externos.
	if regexp.MustCompile(`(?i)<script[^>]*>[^<]`).MatchString(body) || strings.Contains(strings.ToLower(body), "<style") ||
		regexp.MustCompile(`(?i)\son[a-z]+=`).MatchString(body) || strings.Contains(body, "style=") ||
		strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Fatalf("inline code or external resources in the page:\n%s", body)
	}
	if strings.Contains(string(webJS), "innerHTML") || strings.Contains(string(webJS), "eval(") {
		t.Fatal("the JS builds HTML")
	}
	// Una petición de otro origen (preflight) no tiene ruta.
	if pre := r.do("OPTIONS", "/api/propose", "", true); pre.Code == 200 || pre.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("OPTIONS: %d", pre.Code)
	}
}

func TestWebProponerNoEjecutaYConfirmarEs(t *testing.T) {
	r := newWebRig(t, "```sql\n"+webSQL+"\n```")
	id, sql, code := r.propose("¿clientes?")
	if code != 200 || id == "" || sql != webSQL {
		t.Fatalf("propose %d %q %q", code, id, sql)
	}
	if len(queryExecs(r.k, webSQL)) != 0 {
		t.Fatal("proposing ran the query")
	}
	// Al modelo: esquema y pregunta, ni clave ni datos.
	if len(r.p.prompts) != 1 || !strings.Contains(r.p.prompts[0], testSchema) ||
		strings.Contains(r.p.prompts[0], r.pw) || strings.Contains(r.p.prompts[0], "ana") {
		t.Fatalf("prompts %q", r.p.prompts)
	}
	// Sin confirmación (o confirm:false), o con id desconocido: no se ejecuta.
	for _, body := range []string{`{"id":"` + id + `"}`, `{"id":"` + id + `","confirm":false}`, `{"id":"nope","confirm":true}`,
		`{"sql":"DELETE FROM customers","confirm":true}`, `{}`} {
		if rec := r.do("POST", "/api/run", body, true); rec.Code < 400 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
		if len(queryExecs(r.k, webSQL)) != 0 {
			t.Fatalf("%s ran the query", body)
		}
	}
	// Confirmada: se ejecuta la guardada, encerrada, y devuelve la tabla.
	rec := r.do("POST", "/api/run", `{"id":"`+id+`","confirm":true}`, true)
	if rec.Code != 200 {
		t.Fatalf("run %d %s", rec.Code, rec.Body)
	}
	var res askResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Rows) != 2 || res.Rows[0][0] != "ana" || res.Role != "kling_db_ro" {
		t.Fatalf("result %s (%v)", rec.Body, err)
	}
	assertEncerrada(t, r.k, "kling_db_ro", webSQL, 200)
	// Vale una vez.
	if rec := r.do("POST", "/api/run", `{"id":"`+id+`","confirm":true}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("second run: %d", rec.Code)
	}
	if len(queryExecs(r.k, webSQL)) != 1 {
		t.Fatal("the query ran twice")
	}
}

func TestWebSQLRechazadaNoSeEjecuta(t *testing.T) {
	for _, evil := range []string{
		"DELETE FROM customers",
		"SELECT 1; DROP TABLE customers",
		"SELECT pg_read_file('/etc/passwd')",
		"SELECT 1\n\\! rm -rf /",
	} {
		r := newWebRig(t, evil)
		id, _, code := r.propose("q")
		if code != http.StatusUnprocessableEntity || id != "" {
			t.Fatalf("%q: %d %q", evil, code, id)
		}
		if len(r.w.pending) != 0 || len(queryExecs(r.k, "DROP")) != 0 || len(queryExecs(r.k, "DELETE FROM customers")) != 0 {
			t.Fatalf("%q was kept or sent to the copy", evil)
		}
	}
	// Aun si el validador dejara pasar algo al proponer, /run lo vuelve a mirar.
	r := newWebRig(t, webSQL)
	id, _, _ := r.propose("q")
	orig := validateSQL
	validateSQL = func(string) error { return errStub }
	t.Cleanup(func() { validateSQL = orig })
	if rec := r.do("POST", "/api/run", `{"id":"`+id+`","confirm":true}`, true); rec.Code != http.StatusUnprocessableEntity ||
		len(queryExecs(r.k, webSQL)) != 0 {
		t.Fatalf("run with a failing validator: %d", rec.Code)
	}
}

type stubErr string

func (e stubErr) Error() string { return string(e) }

const errStub = stubErr("rejected SQL: test")

// Ni una respuesta lleva la clave de la copia, aunque el modelo, un error o los
// datos la dijeran.
func TestWebNingunaRespuestaLlevaLaClave(t *testing.T) {
	r := newWebRig(t, webSQL, "```sql\nSELECT 'PW' AS c\n```")
	pw := r.pw
	// Datos con la clave en una celda; un error del proveedor con ella también.
	r.k.csv = "name\n" + pw + "\nana\n"
	var all []string
	rec := r.do("GET", "/", "", true)
	all = append(all, rec.Body.String(), rec.Header().Get("Set-Cookie"))
	all = append(all, r.do("GET", "/api/info", "", true).Body.String())
	all = append(all, r.do("GET", "/app.js", "", true).Body.String(), r.do("GET", "/app.css", "", true).Body.String())
	id, sql, _ := r.propose("q")
	all = append(all, sql)
	run := r.do("POST", "/api/run", `{"id":"`+id+`","confirm":true}`, true)
	all = append(all, run.Body.String())
	if !strings.Contains(run.Body.String(), webRedactedMark) {
		t.Fatalf("the password in a cell was not covered: %s", run.Body)
	}
	r.p.err = stubErr("upstream said " + pw)
	all = append(all, r.do("POST", "/api/propose", `{"question":"q"}`, true).Body.String())
	all = append(all, r.do("POST", "/api/run", `{"id":"x","confirm":true}`, false).Body.String())
	for i, s := range all {
		if strings.Contains(s, pw) {
			t.Fatalf("response %d carries the password: %q", i, s)
		}
	}
	// Tampoco en lo que ve el usuario en el propio servidor de estado.
	if strings.Contains(r.w.a.stderr.(interface{ String() string }).String(), pw) {
		t.Fatal("password in stderr")
	}
}

func TestWebExplainSoloConSendData(t *testing.T) {
	// -explain sin -send-data se rechaza al arrancar (misma comprobación que ask).
	o := defaultAskOpts()
	o.explain = true
	if err := o.check("q"); err == nil {
		t.Fatal("explain without send-data accepted")
	}
	r := newWebRig(t, webSQL, "resumen: dos clientes")
	r.w.o.explain, r.w.o.sendData = true, true
	id, _, _ := r.propose("q")
	rec := r.do("POST", "/api/run", `{"id":"`+id+`","confirm":true}`, true)
	var res askResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != 200 || res.Summary != "resumen: dos clientes" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(r.p.prompts) != 2 || !strings.Contains(r.p.prompts[1], "ana") {
		t.Fatalf("prompts %q", r.p.prompts)
	}
	// Sin -explain el modelo nunca ve filas.
	r = newWebRig(t, webSQL)
	id, _, _ = r.propose("q")
	r.do("POST", "/api/run", `{"id":"`+id+`","confirm":true}`, true)
	if len(r.p.prompts) != 1 {
		t.Fatalf("prompts %d", len(r.p.prompts))
	}
}

func TestWebLimitesYCaducidad(t *testing.T) {
	answers := make([]string, 0, webPerMinute+2)
	for i := 0; i < webPerMinute+2; i++ {
		answers = append(answers, webSQL)
	}
	r := newWebRig(t, answers...)
	var limited bool
	for i := 0; i < webPerMinute+2; i++ {
		if _, _, code := r.propose("q"); code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("no rate limit")
	}
	if len(r.w.pending) > webMaxPending {
		t.Fatalf("%d pending", len(r.w.pending))
	}
	// Cuerpo enorme: 400 (no se lee entero).
	r.now = r.now.Add(2 * time.Minute)
	if rec := r.do("POST", "/api/propose", `{"question":"`+strings.Repeat("a", webMaxBody)+`"}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("big body: %d", rec.Code)
	}
	// Una propuesta vieja no se ejecuta.
	r2 := newWebRig(t, webSQL)
	id, _, _ := r2.propose("q")
	r2.now = r2.now.Add(webPendingTTL + time.Second)
	if rec := r2.do("POST", "/api/run", `{"id":"`+id+`","confirm":true}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("expired proposal: %d", rec.Code)
	}
	// La página caduca entera.
	r2.now = r2.w.expires.Add(time.Second)
	if rec := r2.do("GET", "/", "", true); rec.Code != http.StatusGone {
		t.Fatalf("expired page: %d", rec.Code)
	}
}

func TestWebCheckListen(t *testing.T) {
	for _, c := range []struct {
		in     string
		remote bool
		ok     bool
	}{
		{"127.0.0.1:8080", false, true}, {"localhost:0", false, true}, {"[::1]:9", false, true},
		{"0.0.0.0:8080", false, false}, {"192.168.1.5:80", false, false}, {":8080", false, false},
		{"example.com:80", true, false}, {"0.0.0.0:8080", true, true}, {"nada", false, false},
	} {
		_, err := checkListen(c.in, c.remote)
		if (err == nil) != c.ok {
			t.Fatalf("%q remote=%v: err %v", c.in, c.remote, err)
		}
	}
}

// La página embebida tiene que cumplir la CSP (default-src 'none', script-src
// 'self'): nada de <script> en línea ni manejadores on*=, el JS es /app.js, y el
// token CSRF va en <meta name="csrf">, que es de donde lo lee app.js.
func TestWebPaginaEmbebida(t *testing.T) {
	if !strings.Contains(webIndex, `<meta name="csrf" content="{{CSRF}}">`) {
		t.Fatal(`the page has no <meta name="csrf" content="{{CSRF}}">`)
	}
	scripts := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(webIndex, -1)
	if len(scripts) == 0 {
		t.Fatal("the page loads no script")
	}
	for _, s := range scripts {
		if strings.TrimSpace(s[2]) != "" || !regexp.MustCompile(`\bsrc="/[a-z]+\.js"`).MatchString(s[1]) {
			t.Fatalf("inline script (only <script src=\"/x.js\"></script> passes the CSP): %q", s[0])
		}
	}
	if m := regexp.MustCompile(`(?i)\son[a-z]+\s*=`).FindString(webIndex); m != "" {
		t.Fatalf("inline event handler %q", m)
	}
	if strings.Contains(webIndex, "style=") || strings.Contains(strings.ToLower(webIndex), "<style") {
		t.Fatal("inline style (style-src 'self')")
	}
	if strings.Contains(string(webJS), "innerHTML") || strings.Contains(string(webJS), "insertAdjacentHTML") {
		t.Fatal("app.js must render with textContent, never as HTML")
	}
	// Y servida: el marcador sustituido por el token de la sesión.
	r := newWebRig(t)
	rec := r.do("GET", "/", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<meta name="csrf" content="`+r.w.csrf+`">`) {
		t.Fatalf("GET / = %d %q", rec.Code, rec.Body.String())
	}
}
