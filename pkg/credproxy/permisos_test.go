package credproxy

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidarPermiso(t *testing.T) {
	for in, want := range map[string]string{
		"GET /v1/balance":      "GET /v1/balance",
		"get   /v1/balance":    "GET /v1/balance",
		"POST /v1/files/**":    "POST /v1/files/**",
		"DELETE /v1/*/items":   "DELETE /v1/*/items",
		"GET /":                "GET /",
		"GET /**":              "GET /**",
		"PATCH /v1/users/u-*":  "PATCH /v1/users/u-*",
		"OPTIONS /v1/[a-c]x/y": "OPTIONS /v1/[a-c]x/y",
	} {
		got, err := ValidarPermiso(in)
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; quería %q", in, got, err, want)
		}
	}
	for _, mal := range []string{
		"", "GET", "/v1/balance", "GET v1/balance", "GET /v1/balance extra",
		"CONNECT /", "TRACE /", "* /v1", "FETCH /v1",
		"GET /v1/../admin", "GET /v1/", "GET //v1", "GET /v1/./x",
		"GET /v1/**/x", "GET /v1/a**", "GET /v1/[", "GET /" + strings.Repeat("a", 300),
	} {
		if _, err := ValidarPermiso(mal); err == nil {
			t.Errorf("%q no debería valer", mal)
		}
	}
	if err := ValidarPermisos(make([]string, MaxAllow+1)); err == nil {
		t.Error("más de MaxAllow entradas deberían rechazarse")
	}
}

func TestReglaCasa(t *testing.T) {
	casos := []struct {
		patron, metodo, ruta string
		casa                 bool
	}{
		{"GET /v1/balance", "GET", "/v1/balance", true},
		{"GET /v1/balance", "HEAD", "/v1/balance", false}, // HEAD no es GET
		{"GET /v1/balance", "POST", "/v1/balance", false},
		{"GET /v1/balance", "GET", "/v1/balance/x", false},
		{"GET /v1/balance", "GET", "/v1", false},
		{"GET /v1/*", "GET", "/v1/x", true},
		{"GET /v1/*", "GET", "/v1/x/y", false}, // * no cruza /
		{"GET /v1/*", "GET", "/v1", false},
		{"GET /v1/*/items", "GET", "/v1/abc/items", true},
		{"GET /v1/u-*", "GET", "/v1/u-42", true},
		{"GET /v1/u-*", "GET", "/v1/x-42", false},
		{"GET /v1/**", "GET", "/v1", true}, // ** casa cero segmentos
		{"GET /v1/**", "GET", "/v1/a", true},
		{"GET /v1/**", "GET", "/v1/a/b/c", true},
		{"GET /v1/**", "GET", "/v2/a", false},
		{"GET /v1/**", "GET", "/v10", false},
		{"GET /**", "GET", "/", true},
		{"GET /**", "GET", "/cualquier/cosa", true},
		{"GET /", "GET", "/", true},
		{"GET /", "GET", "/x", false},
	}
	for _, c := range casos {
		r, _, err := parsearPermiso(c.patron)
		if err != nil {
			t.Fatalf("%q: %v", c.patron, err)
		}
		if got := r.casa(c.metodo, c.ruta); got != c.casa {
			t.Errorf("%q con %s %s: %v, quería %v", c.patron, c.metodo, c.ruta, got, c.casa)
		}
	}
}

// La ruta se compara normalizada: los .. (también escapados) y las // no
// sirven para salirse de lo permitido.
func TestRutaNormalizada(t *testing.T) {
	for raw, want := range map[string]string{
		"/v1/balance":               "/v1/balance",
		"/v1/../admin":              "/admin",
		"/v1/%2e%2e/admin":          "/admin",
		"/v1/balance/../../admin":   "/admin",
		"/v1//balance":              "/v1/balance",
		"/v1/balance/":              "/v1/balance",
		"/v1/./balance":             "/v1/balance",
		"/../../etc":                "/etc",
		"":                          "/",
		"/v1/a%2Fb":                 "/v1/a/b",
		"/v1/balance%2f..%2f..%2fx": "/x",
	} {
		u, err := url.ParseRequestURI(raw)
		if raw == "" {
			u, err = &url.URL{}, nil
		}
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if got := rutaNormalizada(u); got != want {
			t.Errorf("%q → %q, quería %q", raw, got, want)
		}
	}
}

// credsConPermisos: KEY solo para leer el saldo y subir ficheros; ORG, en el
// mismo dominio, sin restricciones pero con otro marcador.
func credsConPermisos(conOrg bool) []Credential {
	cs := []Credential{{Env: "KEY", Domain: "example.com", Placeholder: testPlace, Secret: testSecret,
		Allow: []string{"GET /v1/balance", "POST /v1/files/**"}}}
	if conOrg {
		cs = append(cs, Credential{Env: "ORG", Domain: "example.com", Placeholder: testPlace2, Secret: testSecret2})
	}
	return cs
}

// Con Allow, lo permitido sale con la clave y lo demás es 403 sin llegar al
// proveedor, también cuando se intenta colar con .. o escapes.
func TestProxyPermisosPorMetodoYRuta(t *testing.T) {
	var mu sync.Mutex
	var rutas []string
	srv, hits := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rutas = append(rutas, r.URL.RequestURI())
		mu.Unlock()
		io.WriteString(w, r.Header.Get("Authorization"))
	}, credsConPermisos(false), nil)

	pedir := func(metodo, ruta string) (int, string) {
		t.Helper()
		// Petición a mano: el cliente de Go limpiaría algunas rutas antes de
		// mandarlas, y aquí interesa justo lo que manda un invitado hostil.
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fmt.Fprintf(c, "%s %s HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer %s\r\nConnection: close\r\n\r\n", metodo, ruta, testPlace)
		b, _ := io.ReadAll(c)
		resp := string(b)
		code := 0
		fmt.Sscanf(resp, "HTTP/1.1 %d", &code)
		return code, resp
	}

	if code, resp := pedir("GET", "/v1/balance"); code != 200 || !strings.Contains(resp, "Bearer "+testPlace) {
		t.Fatalf("GET /v1/balance: %d %q", code, resp)
	}
	if code, _ := pedir("POST", "/v1/files/a/b.txt"); code != 200 {
		t.Errorf("POST /v1/files/a/b.txt: %d", code)
	}
	antes := atomic.LoadInt32(hits)
	for _, mal := range [][2]string{
		{"GET", "/v1/charges"},
		{"POST", "/v1/balance"},
		{"HEAD", "/v1/balance"},
		{"DELETE", "/v1/files/a"},
		{"GET", "/v1/balance/../../admin"},
		{"GET", "/v1/files/../../admin"},
		{"POST", "/v1/files/%2e%2e/%2e%2e/admin"},
		{"POST", "/v1/files/..%2f..%2fadmin"},
		{"GET", "/admin?x=/v1/balance"},
	} {
		if code, resp := pedir(mal[0], mal[1]); code != http.StatusForbidden {
			t.Errorf("%s %s: %d, quería 403 (%q)", mal[0], mal[1], code, resp)
		}
	}
	if n := atomic.LoadInt32(hits) - antes; n != 0 {
		t.Errorf("el proveedor recibió %d peticiones no permitidas", n)
	}

	// Lo permitido tras normalizar sale con la ruta normalizada: el proveedor ve
	// lo mismo que se comprobó, no lo que mandó el invitado.
	mu.Lock()
	rutas = nil
	mu.Unlock()
	if code, _ := pedir("GET", "/v1/x/../balance?q=1"); code != 200 {
		t.Fatalf("GET /v1/x/../balance: %d", code)
	}
	if code, _ := pedir("POST", "/v1/files/dir/"); code != 200 {
		t.Fatalf("POST /v1/files/dir/: %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(rutas) != 2 || rutas[0] != "/v1/balance?q=1" || rutas[1] != "/v1/files/dir/" {
		t.Errorf("el proveedor vio %q", rutas)
	}
}

// El 403 llega antes de leer el cuerpo: un invitado que anuncia una subida a
// una ruta no permitida no llega a mandarla.
func TestProxyPermisos403AntesDeLeerElCuerpo(t *testing.T) {
	srv, hits := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {}, credsConPermisos(false), nil)
	pr, pw := io.Pipe()
	defer pw.Close() // nunca se escribe nada
	req, _ := http.NewRequest("POST", srv.URL+"/v1/charges", pr)
	req.Host = "example.com"
	hecho := make(chan *http.Response, 1)
	go func() {
		resp, err := (&http.Transport{}).RoundTrip(req)
		if err != nil {
			hecho <- nil
			return
		}
		hecho <- resp
	}()
	select {
	case resp := <-hecho:
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("respuesta %+v, quería 403", resp)
		}
		resp.Body.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("el proxy esperó al cuerpo para contestar 403")
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Errorf("el proveedor recibió %d peticiones", *hits)
	}
}

// Dos credenciales del mismo dominio: en una ruta que solo una permite, solo
// esa se sustituye; el marcador de la otra sale tal cual (no es un secreto), y
// la respuesta se redacta igualmente con las dos.
func TestProxyPermisosSoloSustituyeLaCredencialQueCasa(t *testing.T) {
	var key, org string
	srv, _ := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {
		key, org = r.Header.Get("Authorization"), r.Header.Get("X-Org")
		io.WriteString(w, testSecret+" "+testSecret2)
	}, credsConPermisos(true), nil)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/charges", nil)
	req.Host = "example.com"
	req.Header.Set("Authorization", "Bearer "+testPlace)
	req.Header.Set("X-Org", testPlace2)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if key != "Bearer "+testPlace || org != testSecret2 {
		t.Errorf("el proveedor recibió KEY=%q ORG=%q; KEY no permite POST /v1/charges", key, org)
	}
	if bytes.Contains(body, []byte(testSecret)) || bytes.Contains(body, []byte(testSecret2)) {
		t.Errorf("la respuesta llevó una clave: %s", body)
	}
}

// Una credencial sin Allow (lo de antes) lo permite todo y reenvía la ruta
// tal cual, sin normalizar.
func TestProxySinPermisosEsTodoYNoTocaLaRuta(t *testing.T) {
	var ruta string
	srv, _ := proxyCon(t, func(w http.ResponseWriter, r *http.Request) {
		ruta = r.URL.EscapedPath()
	}, credsUna(), nil)
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "DELETE /v1/a%%2Fb/ HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
	b, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(b), "HTTP/1.1 200") || ruta != "/v1/a%2Fb/" {
		t.Errorf("respuesta %q, el proveedor vio %q", b, ruta)
	}
}

func TestValidarCredencialesConPermisos(t *testing.T) {
	c := []Credential{{Domain: "a.example.com", Placeholder: testPlace, Secret: "x", Allow: []string{"get /v1/x"}}}
	if err := ValidarCredenciales(c); err != nil || c[0].Allow[0] != "GET /v1/x" {
		t.Fatalf("%v, %q", err, c[0].Allow)
	}
	c[0].Allow = []string{"GET /v1/../x"}
	if err := ValidarCredenciales(c); err == nil {
		t.Error("una ruta sin limpiar debería rechazarse")
	}
	p := New(Options{})
	if _, err := p.SetCredentials(c); err == nil {
		t.Error("SetCredentials debería rechazar un Allow inválido")
	}
}
