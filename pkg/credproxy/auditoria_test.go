package credproxy

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// proxyAuditado monta un proxy con registro en un directorio temporal cuyo
// proveedor es rt. Devuelve el proxy y la ruta del registro.
func proxyAuditado(t *testing.T, rt roundTripFunc, creds []Credential, enabled func() bool) (*Proxy, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), AuditFile)
	p := New(Options{Transport: rt, AuditPath: path, Enabled: enabled})
	t.Cleanup(func() { _ = p.Close() })
	if _, err := p.SetCredentials(creds); err != nil {
		t.Fatal(err)
	}
	return p, path
}

// eco es un proveedor que contesta 200 con el Authorization que recibió.
func eco(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r,
		Body: io.NopCloser(strings.NewReader("echo " + r.Header.Get("Authorization")))}, nil
}

// servir llama al proxy como lo haría el servidor, incluido el pánico con que
// aborta una respuesta a medias.
func servir(p *Proxy, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				panic(v)
			}
		}()
		p.ServeHTTP(rec, req)
	}()
	return rec
}

// leerRegistro cierra el proxy (vacía la escritora) y devuelve las líneas del
// registro ya decodificadas y el fichero en crudo.
func leerRegistro(t *testing.T, p *Proxy, path string) ([]Record, string) {
	t.Helper()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []Record
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("línea que no es JSON: %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	return out, string(b)
}

func credsAuditoria() []Credential {
	return []Credential{
		{Env: "KEY", Domain: "example.com", Placeholder: testPlace, Secret: testSecret},
		{Env: "ORG", Domain: "example.com", Placeholder: testPlace2, Secret: testSecret2},
		{Env: "RO", Domain: "ro.example.com", Placeholder: "kling-cred-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Secret: "sk_ro_otra",
			Allow: []string{"GET /v1/balance"}},
	}
}

// Un registro por cada forma de terminar, con su motivo, su estado y si fue
// una denegación de política.
func TestAuditoriaUnRegistroPorResultado(t *testing.T) {
	var fallar, codificar, cortar bool
	rt := func(r *http.Request) (*http.Response, error) {
		switch {
		case fallar:
			return nil, errors.New("dial tcp: " + testSecret + " upstream text")
		case codificar:
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"br"}}, Request: r,
				Body: io.NopCloser(strings.NewReader("x"))}, nil
		case cortar:
			return &http.Response{StatusCode: 200, Header: http.Header{}, Request: r,
				Body: io.NopCloser(io.MultiReader(strings.NewReader("parte"), iotest.ErrReader(errors.New("cortado"))))}, nil
		}
		return eco(r)
	}
	activo := true
	p, path := proxyAuditado(t, rt, credsAuditoria(), func() bool { return activo })

	type caso struct {
		nombre string
		req    func() *http.Request
		antes  func()
		status int
		reason string
		denied bool
		creds  []string
	}
	get := func(host, target string) func() *http.Request {
		return func() *http.Request {
			r := httptest.NewRequest("GET", target, nil)
			r.Host = host
			return r
		}
	}
	casos := []caso{
		{nombre: "ok con cabecera", req: func() *http.Request {
			r := get("example.com", "/v1/charges?limit=1")()
			r.Header.Set("Authorization", "Bearer "+testPlace)
			return r
		}, status: 200, creds: []string{"KEY"}},
		{nombre: "ok con cuerpo y Basic", req: func() *http.Request {
			r := httptest.NewRequest("POST", "/v1/x", strings.NewReader(`{"org":"`+testPlace2+`"}`))
			r.Host = "example.com"
			r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(testPlace+":")))
			return r
		}, status: 200, creds: []string{"KEY", "ORG"}},
		{nombre: "ok sin marcador", req: get("example.com", "/v1/nada"), status: 200},
		{nombre: "sin credencial", req: get("otro.com", "/"), status: 403, reason: ReasonNoCredential, denied: true},
		{nombre: "no permitido", req: func() *http.Request { r := get("ro.example.com", "/v1/charges")(); return r },
			status: 403, reason: ReasonNotAllowed, denied: true},
		{nombre: "ruta ambigua", req: get("ro.example.com", "/v1/a%2Fb"), status: 403, reason: ReasonAmbiguousPath, denied: true},
		{nombre: "connect", req: func() *http.Request { r := get("example.com", "/")(); r.Method = http.MethodConnect; return r },
			status: 405, reason: ReasonConnect},
		{nombre: "cuerpo enorme", req: func() *http.Request {
			r := httptest.NewRequest("POST", "/v1/x", strings.NewReader("x"))
			r.Host, r.ContentLength = "example.com", MaxBody+1
			return r
		}, status: 413, reason: ReasonBodyTooLarge},
		{nombre: "cuerpo ilegible", req: func() *http.Request {
			r := httptest.NewRequest("POST", "/v1/x", iotest.ErrReader(errors.New("roto")))
			r.Host, r.ContentLength = "example.com", -1
			return r
		}, status: 400, reason: ReasonBadBody},
		{nombre: "proveedor caído", req: get("example.com", "/v1/x"), antes: func() { fallar = true },
			status: 502, reason: ReasonUpstreamError},
		{nombre: "codificación", req: get("example.com", "/v1/x"), antes: func() { fallar, codificar = false, true },
			status: 502, reason: ReasonBadEncoding},
		{nombre: "cortada", req: get("example.com", "/v1/x"), antes: func() { codificar, cortar = false, true },
			status: 200, reason: ReasonAborted},
		{nombre: "inactivo", req: get("example.com", "/v1/x"), antes: func() { cortar, activo = false, false },
			status: 403, reason: ReasonDisabled, denied: true},
		{nombre: "ocupado", req: get("example.com", "/v1/x"), antes: func() {
			activo = true
			for range MaxInFlight {
				p.sem <- struct{}{}
			}
		}, status: 503, reason: ReasonBusy},
	}
	for _, c := range casos {
		if c.antes != nil {
			c.antes()
		}
		if got := servir(p, c.req()).Code; got != c.status {
			t.Fatalf("%s: status %d, quería %d", c.nombre, got, c.status)
		}
	}
	recs, _ := leerRegistro(t, p, path)
	if len(recs) != len(casos) {
		t.Fatalf("%d registros para %d peticiones: %+v", len(recs), len(casos), recs)
	}
	for i, c := range casos {
		r := recs[i]
		if r.Kind != KindHTTP || r.Status != c.status || r.Reason != c.reason || r.Denied != c.denied ||
			!slices.Equal(r.Creds, c.creds) || r.TS.IsZero() || r.Host == "" || r.Method == "" {
			t.Errorf("%s: registro %+v; quería status %d reason %q denied %v creds %v",
				c.nombre, r, c.status, c.reason, c.denied, c.creds)
		}
	}
	if r := recs[0]; r.Path != "/v1/charges" || !r.Query || r.RespBytes == 0 || r.Method != "GET" || r.Host != "example.com" {
		t.Errorf("el registro correcto: %+v", r)
	}
	if r := recs[1]; r.ReqBytes != int64(len(`{"org":"`+testPlace2+`"}`)) {
		t.Errorf("req_bytes = %d", r.ReqBytes)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("permisos del registro: %v %v", fi.Mode(), err)
	}
}

// Nada de la clave, el marcador, la query, cabeceras, cuerpos ni el texto de
// un error del proveedor llega al fichero, en ninguna de sus formas.
func TestAuditoriaNoEscribeSecretosNiContenido(t *testing.T) {
	fallar := false
	rt := func(r *http.Request) (*http.Response, error) {
		if fallar {
			return nil, errors.New("upstream said SECRETO_DEL_ERROR " + testSecret)
		}
		return eco(r)
	}
	p, path := proxyAuditado(t, rt, credsAuditoria(), nil)
	pedir := func(metodo, target string, cuerpo string) {
		var body io.Reader
		if cuerpo != "" {
			body = strings.NewReader(cuerpo)
		}
		r := httptest.NewRequest(metodo, target, body)
		r.Host = "example.com"
		r.Header.Set("Authorization", "Bearer "+testPlace)
		r.Header.Set("X-Cabecera", "VALOR_DE_CABECERA")
		servir(p, r)
	}
	for _, v := range variantes(testSecret2) {
		pedir("GET", "/v1/"+url.PathEscape(v)+"/x?key="+testPlace+"&q=CONTENIDO_DE_QUERY", "")
	}
	pedir("GET", "/v1/"+testSecret+"/y", "")
	pedir("GET", "/v1/"+testPlace+"/z", "")
	pedir("POST", "/v1/w", "CONTENIDO_DEL_CUERPO "+testPlace2)
	fallar = true
	pedir("GET", "/v1/fallo", "")

	recs, crudo := leerRegistro(t, p, path)
	if len(recs) == 0 {
		t.Fatal("registro vacío")
	}
	var prohibidas []string
	for _, s := range []string{testSecret, testSecret2, testPlace, testPlace2} {
		prohibidas = append(prohibidas, variantes(s)...)
	}
	prohibidas = append(prohibidas, "kling-cred-", "CONTENIDO_DE_QUERY", "VALOR_DE_CABECERA", "CONTENIDO_DEL_CUERPO",
		"SECRETO_DEL_ERROR", "key=", "sec+ret", "sec%2Bret")
	for _, s := range prohibidas {
		if strings.Contains(crudo, s) {
			t.Errorf("el registro contiene %q:\n%s", s, crudo)
		}
	}
	if !strings.Contains(crudo, `"/v1/:cred/z"`) || !strings.Contains(crudo, `"/v1/:cred/y"`) {
		t.Errorf("rutas enmascaradas que faltan:\n%s", crudo)
	}
}

func TestRutaAuditada(t *testing.T) {
	ocultar := variantes("abc/def")
	for _, c := range []struct{ in, want string }{
		{"/v1/charges", "/v1/charges"},
		{"/v1/../v2/./x", "/v2/x"},
		{"/v1/" + testPlace, "/v1/:cred"},
		{"/v1/pre-KLING-CRED-x", "/v1/:cred"},
		{"/v1/abc/def/tail", "/v1/:cred/tail"},
		{"/v1/abc%2Fdef", "/v1/:cred"},
		{"/v1/0123456789abcdef0123456789abcdef", "/v1/:tok"},
		{"/v1/sk_live-ABCDEFGHIJKLMNOPQRSTUVWXYZ012345/x", "/v1/:tok/x"},
		{"/v1/0123456789abcdef0123456789abcde", "/v1/0123456789abcdef0123456789abcde"}, // 31: no
		{"/v1/a.b.c.d.e.f.g.h.i.j.k.l.m.n.o.p.q", "/v1/a.b.c.d.e.f.g.h.i.j.k.l.m.n.o.p.q"},
	} {
		u, err := url.Parse(c.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := rutaAuditada(u, ocultar); got != c.want {
			t.Errorf("rutaAuditada(%q) = %q, quería %q", c.in, got, c.want)
		}
	}
	largo, _ := url.Parse("/" + strings.Repeat("a/", 400))
	if got := rutaAuditada(largo, nil); len(got) != maxRutaAuditada {
		t.Errorf("ruta larga: %d bytes", len(got))
	}
	if hostAuditado(strings.Repeat("a", 400)) != strings.Repeat("a", maxHostAuditado) || hostAuditado("x"+testPlace+".com") != ":cred" {
		t.Error("hostAuditado no acota o no enmascara")
	}
}

// Rotación: al pasar de AuditMaxBytes el fichero pasa a .1 (una generación) y
// no se pierde ninguna línea de las que caben.
func TestAuditoriaRota(t *testing.T) {
	path := filepath.Join(t.TempDir(), AuditFile)
	a := &Auditor{path: path}
	rec := Record{Kind: KindHTTP, Method: "GET", Host: "example.com", Path: "/" + strings.Repeat("x", 200)}
	linea, _ := json.Marshal(Record{TS: time.Now().UTC(), Kind: rec.Kind, Method: rec.Method, Host: rec.Host, Path: rec.Path})
	porFichero := AuditMaxBytes / (len(linea) + 1)
	total := porFichero*2 + 10 // rota dos veces
	for range total {
		a.escribir(rec)
	}
	a.vaciar()
	a.cerrarFichero()
	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	viejo, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cur) > AuditMaxBytes || len(viejo) > AuditMaxBytes {
		t.Fatalf("tamaños %d y %d por encima del tope", len(cur), len(viejo))
	}
	// El .1 se rotó casi lleno (le faltaba menos de una línea); la longitud
	// de cada línea varía un poco con los dígitos de ts.
	nc, nv := strings.Count(string(cur), "\n"), strings.Count(string(viejo), "\n")
	if len(viejo) < AuditMaxBytes-2*len(linea) || nc == 0 || nc+nv >= total {
		t.Fatalf("líneas: actual %d, .1 %d, escritas %d (una generación: la primera se pierde)", nc, nv, total)
	}
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Fatal("solo debe haber una generación")
	}
}

// Con la escritora atascada, mandar registros no espera: se descartan con la
// cola llena y la cuenta llega al fichero en el siguiente registro. El
// ServeHTTP tampoco espera.
func TestAuditoriaDescartaSinBloquearYLoCuenta(t *testing.T) {
	path := filepath.Join(t.TempDir(), AuditFile)
	dentro, soltar := make(chan struct{}), make(chan struct{})
	var una sync.Once
	a := nuevoAuditor(path, nil, func() {
		una.Do(func() { close(dentro) })
		<-soltar
	})
	a.Record(Record{Kind: KindHTTP})
	<-dentro // la escritora tiene el primero y está atascada
	p := New(Options{Transport: roundTripFunc(eco)})
	p.aud = a
	if _, err := p.SetCredentials(credsAuditoria()); err != nil {
		t.Fatal(err)
	}
	const extra = 50
	inicio := time.Now()
	for range auditCola + extra {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = "otro.com"
		servir(p, r)
	}
	if d := time.Since(inicio); d > 5*time.Second {
		t.Fatalf("%d peticiones con la escritora atascada tardaron %v", auditCola+extra, d)
	}
	if got := a.dropped.Load(); got != extra {
		t.Fatalf("descartados = %d, quería %d", got, extra)
	}
	close(soltar)
	a.Record(Record{Kind: KindHTTP}) // cabe: la cola se está vaciando o se descarta y cuenta
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lineas, descartados int
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind == KindHTTP {
			lineas++
		}
		descartados += int(r.Dropped)
	}
	if lineas+descartados != 1+auditCola+extra+1 || descartados < extra {
		t.Fatalf("escritas %d + descartadas %d; mandadas %d", lineas, descartados, 1+auditCola+extra+1)
	}
}

// Los descartados sin registro detrás que los lleve salen en uno propio.
func TestAuditoriaDescartadosAlCerrar(t *testing.T) {
	path := filepath.Join(t.TempDir(), AuditFile)
	a := NewAuditor(path, nil)
	a.dropped.Add(3)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a.Record(Record{Kind: KindHTTP}) // tras Close: no hace nada ni bloquea
	b, _ := os.ReadFile(path)
	var r Record
	if err := json.Unmarshal(b, &r); err != nil || r.Kind != KindDropped || r.Dropped != 3 {
		t.Fatalf("registro al cerrar: %q (%v)", b, err)
	}
}

// Un enlace plantado en lugar del registro no se sigue: el destino queda
// intacto y los registros se cuentan como descartados.
func TestAuditoriaNoSigueEnlaces(t *testing.T) {
	dir := t.TempDir()
	destino := filepath.Join(dir, "ajeno")
	if err := os.WriteFile(destino, []byte("intacto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, AuditFile)
	if err := os.Symlink(destino, path); err != nil {
		t.Fatal(err)
	}
	a := &Auditor{path: path}
	a.escribir(Record{Kind: KindHTTP})
	a.vaciar()
	if b, _ := os.ReadFile(destino); string(b) != "intacto\n" {
		t.Fatalf("se escribió a través del enlace: %q", b)
	}
	if a.dropped.Load() != 1 {
		t.Fatalf("descartados = %d", a.dropped.Load())
	}
}

// El medidor no esconde el ResponseWriter del servidor: los plazos y el Flush
// de http.ResponseController siguen llegando.
func TestMedidorUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	m := &medidor{w: rec}
	if err := http.NewResponseController(m).Flush(); err != nil || !rec.Flushed {
		t.Fatalf("Flush a través del medidor: %v", err)
	}
	errs := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errs <- http.NewResponseController(&medidor{w: w}).SetWriteDeadline(time.Now().Add(time.Minute))
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := <-errs; err != nil {
		t.Fatalf("SetWriteDeadline a través del medidor: %v", err)
	}
}

// Con el servidor de verdad (el medidor delante del ResponseWriter de
// net/http) la petición llega entera y queda registrada.
func TestAuditoriaConServidorReal(t *testing.T) {
	path := filepath.Join(t.TempDir(), AuditFile)
	var p *Proxy
	srv2, _ := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hola "+r.Header.Get("Authorization"))
	}, credsUna(), func(px *Proxy) { px.aud = NewAuditor(path, nil); p = px })
	resp := peticion(t, srv2, "example.com", "Bearer "+testPlace)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hola Bearer "+testPlace {
		t.Fatalf("respuesta %q", b)
	}
	srv2.Close()
	recs, _ := leerRegistro(t, p, path)
	if len(recs) != 1 || recs[0].Status != 200 || !slices.Equal(recs[0].Creds, []string{"KEY"}) ||
		recs[0].RespBytes != int64(len(b)) {
		t.Fatalf("registro: %+v", recs)
	}
}

// Sin Close: con la cola vacía el registro llega al fichero enseguida, antes
// del vaciado de cada segundo. Es lo que sobrevive a un SIGKILL (kling-vz).
func TestAuditoriaVaciaSinEsperarAlCierre(t *testing.T) {
	path := filepath.Join(t.TempDir(), AuditFile)
	a := NewAuditor(path, nil)
	defer a.Close()
	a.Record(Record{Kind: KindHTTP, Path: "/ya"})
	limite := time.Now().Add(auditCada / 2)
	for time.Now().Before(limite) {
		if b, _ := os.ReadFile(path); strings.Contains(string(b), `"/ya"`) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("el registro no llegó al fichero en %v", auditCada/2)
}
