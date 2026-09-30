package credproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests del proxy de credenciales: sin root ni red real. El «proveedor» es un
// servidor TLS de httptest (certificado válido para example.com) y el
// transporte del proxy se desvía a él.

const (
	testSecret = "sk_test_SECRETO_REAL_0123456789"
	testPlace  = "kling-cred-00112233445566778899aabbccddeeff0011"
	// Una segunda credencial para el MISMO dominio, con caracteres que los
	// codificadores escapan (/ + & <).
	testSecret2 = "org/sec+ret&<id>"
	testPlace2  = "kling-cred-ffeeddccbbaa99887766554433221100ffee"
)

// proxyContra monta un proxy con dos credenciales para example.com cuyo
// proveedor es h. Devuelve el servidor del proxy y cuántas peticiones llegaron
// al proveedor.
func proxyContra(t *testing.T, h http.HandlerFunc) (*httptest.Server, *int32) {
	t.Helper()
	return proxyCon(t, h, []Credential{
		{Env: "KEY", Domain: "example.com", Placeholder: testPlace, Secret: testSecret, Query: true, Body: true},
		{Env: "ORG", Domain: "example.com", Placeholder: testPlace2, Secret: testSecret2, Headers: []string{"x-org"}, Body: true},
	}, nil)
}

// proxyCon es proxyContra con las credenciales que se quieran y, si ajustar
// no es nil, la ocasión de tocar el proxy (los plazos) antes de servirlo.
func proxyCon(t *testing.T, h http.HandlerFunc, creds []Credential, ajustar func(*Proxy)) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		h(w, r)
	}))
	t.Cleanup(up.Close)
	tr := up.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, up.Listener.Addr().String())
	}
	p := New(Options{Transport: tr})
	if _, err := p.SetCredentials(creds); err != nil {
		t.Fatal(err)
	}
	if ajustar != nil {
		ajustar(p)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func peticion(t *testing.T, srv *httptest.Server, host, auth string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/charges?limit=1", nil)
	req.Host = host
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// El proveedor recibe la clave real; el invitado nunca la ve, ni en el cuerpo ni
// en las cabeceras de una respuesta que la devuelve en eco.
func TestProxySustituyeElMarcadorYRedactaElEco(t *testing.T) {
	var recibida string
	srv, _ := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {
		recibida = r.Header.Get("Authorization")
		w.Header().Set("X-Echo", r.Header.Get("Authorization"))
		io.WriteString(w, `{"auth":"`+r.Header.Get("Authorization")+`","path":"`+r.URL.RequestURI()+`"}`)
	})
	resp := peticion(t, srv, "example.com", "Bearer "+testPlace)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if recibida != "Bearer "+testSecret {
		t.Fatalf("el proveedor recibió %q, quería la clave real", recibida)
	}
	if bytes.Contains(body, []byte(testSecret)) || strings.Contains(resp.Header.Get("X-Echo"), testSecret) {
		t.Fatalf("la clave llegó al invitado: cuerpo %s, cabecera %q", body, resp.Header.Get("X-Echo"))
	}
	if !bytes.Contains(body, []byte(testPlace)) || !bytes.Contains(body, []byte("/v1/charges?limit=1")) {
		t.Errorf("el eco debería llevar el marcador y la ruta: %s", body)
	}
}

// Dos credenciales para el mismo dominio: cada cabecera lleva la suya y las dos
// se redactan en la vuelta.
func TestProxyVariasCredencialesPorDominio(t *testing.T) {
	var key, org string
	srv, _ := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {
		key, org = r.Header.Get("Authorization"), r.Header.Get("X-Org")
		io.WriteString(w, key+" "+org)
	})
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "example.com"
	req.Header.Set("Authorization", "Bearer "+testPlace)
	req.Header.Set("X-Org", testPlace2)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if key != "Bearer "+testSecret || org != testSecret2 {
		t.Fatalf("el proveedor recibió %q / %q", key, org)
	}
	if want := "Bearer " + testPlace + " " + testPlace2; string(body) != want {
		t.Errorf("eco %q, quería %q", body, want)
	}
}

// Un invitado que pide gzip no consigue que el eco de la clave llegue comprimido
// (y por tanto sin redactar): el proxy no reenvía su Accept-Encoding.
func TestProxyNoDejaQueGzipEsquiveLaRedaccion(t *testing.T) {
	// El proveedor comprime siempre que se lo piden, como haría uno de verdad.
	srv, _ := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			io.WriteString(w, r.Header.Get("Authorization"))
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		io.WriteString(zw, r.Header.Get("Authorization"))
		zw.Close()
	})
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "example.com"
	req.Header.Set("Authorization", "Bearer "+testPlace)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Encoding") != "" || bytes.Contains(body, []byte(testSecret)) {
		t.Fatalf("respuesta comprimida o con la clave: %q %q", resp.Header.Get("Content-Encoding"), body)
	}
}

// Un proveedor que impone una codificación que nadie le pidió (brotli) no se
// entrega: el redactor no vería la clave dentro, así que 502.
func TestProxyRechazaUnaCodificacionQueNoPuedeInspeccionar(t *testing.T) {
	srv, _ := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		io.WriteString(w, "no-es-brotli-de-verdad "+r.Header.Get("Authorization"))
	})
	resp := peticion(t, srv, "example.com", "Bearer "+testPlace)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || bytes.Contains(body, []byte(testSecret)) {
		t.Fatalf("status %d cuerpo %q; quería 502 sin la clave", resp.StatusCode, body)
	}
}

// Authorization: Basic lleva usuario:clave en base64: el marcador tampoco
// aparece en claro y hay que sustituirlo dentro. Y el eco de esa cabecera —el
// base64 con la clave real dentro— también vuelve redactado: antes se escapaba,
// porque la clave no aparecía en claro en la respuesta.
func TestProxySustituyeDentroDeBasicYRedactaSuEco(t *testing.T) {
	var recibida string
	srv, _ := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {
		recibida = r.Header.Get("Authorization")
		// Eco con prefijo y sin él, como hace httpbin /anything y como podría
		// hacer un proveedor que devuelva solo el token.
		io.WriteString(w, `{"headers":{"Authorization":"`+recibida+`"},"raw":"`+strings.TrimPrefix(recibida, "Basic ")+`"}`)
	})
	enc := base64.StdEncoding.EncodeToString([]byte("demo:" + testPlace))
	resp := peticion(t, srv, "example.com", "Basic "+enc)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("demo:"+testSecret))
	if recibida != want {
		t.Fatalf("Basic: el proveedor recibió %q, quería %q", recibida, want)
	}
	if bytes.Contains(body, []byte(strings.TrimPrefix(want, "Basic "))) {
		t.Fatalf("el base64 con la clave real llegó al invitado: %s", body)
	}
	if n := bytes.Count(body, []byte(enc)); n != 2 {
		t.Errorf("el eco debería llevar el base64 con el marcador dos veces, lleva %d: %s", n, body)
	}
}

// La clave también se cambia en la query (?key=) y en un cuerpo pequeño (un
// client_secret de OAuth), y el cuerpo sale con su longitud nueva.
func TestProxySustituyeEnLaQueryYEnElCuerpo(t *testing.T) {
	var uri, cuerpo string
	var declarada int64
	srv, _ := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {
		uri = r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		cuerpo, declarada = string(b), r.ContentLength
		io.WriteString(w, r.URL.RawQuery+" "+cuerpo)
	})
	form := "grant_type=client_credentials&client_secret=" + testPlace2
	req, _ := http.NewRequest("POST", srv.URL+"/token?key="+testPlace+"&x=1", strings.NewReader(form))
	req.Host = "example.com"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if uri != "/token?key="+testSecret+"&x=1" {
		t.Errorf("query recibida %q", uri)
	}
	wantBody := "grant_type=client_credentials&client_secret=" + testSecret2
	if cuerpo != wantBody || declarada != int64(len(wantBody)) {
		t.Errorf("cuerpo recibido %q (Content-Length %d)", cuerpo, declarada)
	}
	if bytes.Contains(body, []byte(testSecret)) || bytes.Contains(body, []byte(testSecret2)) ||
		bytes.Contains(body, []byte("org%2Fsec%2Bret")) {
		t.Errorf("la clave (o su forma percent-encoded) volvió en el eco: %s", body)
	}
}

// Un host sin credencial no sale: 403 y el proveedor no ve nada.
func TestProxyRechazaUnHostSinCredencial(t *testing.T) {
	srv, hits := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {})
	resp := peticion(t, srv, "evil.example.org", "Bearer "+testPlace)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, quería 403", resp.StatusCode)
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Errorf("el proveedor recibió %d peticiones", *hits)
	}
}

// Una redirección del proveedor no se sigue: la clave no viaja al destino de un
// Location que el proxy no controla.
func TestProxyNoSigueRedirecciones(t *testing.T) {
	srv, hits := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.example.net/steal", http.StatusFound)
	})
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "example.com"
	req.Header.Set("Authorization", "Bearer "+testPlace)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || atomic.LoadInt32(hits) != 1 {
		t.Errorf("status %d con %d peticiones al proveedor; quería la 302 devuelta tal cual y 1", resp.StatusCode, *hits)
	}
}

// El redactor sustituye la clave aunque llegue partida entre dos escrituras, y
// también sus variantes escapadas (\/ de PHP, & de Go, percent-encoding).
func TestRedactorClavePartidaYVariantes(t *testing.T) {
	var out bytes.Buffer
	cs := []Credential{
		{Placeholder: testPlace, Secret: testSecret},
		{Placeholder: testPlace2, Secret: testSecret2},
	}
	r := nuevoRedactor(&out, cs)
	full := "antes " + testSecret + " después " + testSecret +
		` json:"org\/sec+ret&<id>" php:"org\/sec+ret&<id>" url:org%2Fsec%2Bret%26%3Cid%3E html:org/sec+ret&amp;&lt;id&gt;`
	for i := 0; i < len(full); i += 5 {
		r.Write([]byte(full[i:min(i+5, len(full))]))
	}
	r.ReadFrom(strings.NewReader(""))
	got := out.String()
	for _, mal := range []string{testSecret, `org\/sec+ret&`, `org\/sec+ret&<id>`, "org%2Fsec%2Bret", "org/sec+ret&amp;"} {
		if strings.Contains(got, mal) {
			t.Errorf("sobrevivió %q en: %s", mal, got)
		}
	}
	if strings.Count(got, testPlace) != 2 || strings.Count(got, testPlace2) != 4 {
		t.Errorf("marcadores: %d y %d en %s", strings.Count(got, testPlace), strings.Count(got, testPlace2), got)
	}
}

// Un flujo de eventos no se queda con la cola retenida: si lo escrito no puede
// ser el comienzo de una clave, sale entero en el acto.
func TestRedactorNoRetieneLoQueNoPuedeSerClave(t *testing.T) {
	var out bytes.Buffer
	r := nuevoRedactor(&out, []Credential{{Placeholder: testPlace, Secret: testSecret}})
	ev := "data: {\"delta\":\"hola\"}\n\n"
	r.Write([]byte(ev))
	if out.String() != ev {
		t.Fatalf("se retuvo parte del evento: %q", out.String())
	}
	// Y sí retiene justo lo que podría serlo: un prefijo de la clave al final.
	out.Reset()
	r.Write([]byte("x sk_test_SEC"))
	if out.String() != "x " {
		t.Fatalf("debería retener el posible comienzo de la clave: %q", out.String())
	}
	r.Write([]byte("RETO_REAL_0123456789 y"))
	if out.String() != "x "+testPlace+" y" {
		t.Fatalf("la clave partida no se redactó: %q", out.String())
	}
}

// Una respuesta en flujo (SSE) llega al invitado evento a evento, no al final.
func TestProxyEntregaUnFlujoSSESinEsperarAlFinal(t *testing.T) {
	segundo := make(chan struct{})
	srv, _ := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: uno "+r.Header.Get("Authorization")+"\n\n")
		w.(http.Flusher).Flush()
		<-segundo
		io.WriteString(w, "data: dos\n\n")
	})
	req, _ := http.NewRequest("GET", srv.URL+"/stream", nil)
	req.Host = "example.com"
	req.Header.Set("Authorization", "Bearer "+testPlace)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 256)
	type leido struct {
		n   int
		err error
	}
	ch := make(chan leido, 1)
	go func() { n, err := resp.Body.Read(buf); ch <- leido{n, err} }()
	select {
	case l := <-ch:
		if l.err != nil || !strings.Contains(string(buf[:l.n]), "Bearer "+testPlace) || !strings.HasSuffix(string(buf[:l.n]), "\n\n") {
			t.Fatalf("primer evento incompleto o sin redactar: %q (%v)", buf[:l.n], l.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("el primer evento no llegó hasta que el proveedor terminó")
	}
	close(segundo)
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "data: dos") {
		t.Errorf("faltó el segundo evento: %q", rest)
	}
}

func TestValidarDominio(t *testing.T) {
	for _, ok := range []string{"api.stripe.com", "API.Stripe.com.", "httpbin.org"} {
		if _, err := ValidarDominio(ok); err != nil {
			t.Errorf("%q debería valer: %v", ok, err)
		}
	}
	for _, mal := range []string{"*.stripe.com", "1.2.3.4", "api.stripe.com:443", "localhost", "", "a..b.com"} {
		if _, err := ValidarDominio(mal); err == nil {
			t.Errorf("%q no debería valer", mal)
		}
	}
}

func TestValidarCredenciales(t *testing.T) {
	ok := []Credential{{Domain: "A.example.com", Placeholder: testPlace, Secret: "x-clave-de-prueba"}}
	if err := ValidarCredenciales(ok); err != nil || ok[0].Domain != "a.example.com" {
		t.Fatalf("válida: %v, dominio %q", err, ok[0].Domain)
	}
	for nombre, c := range map[string][]Credential{
		"marcador repetido": {{Domain: "a.example.com", Placeholder: testPlace, Secret: "x-clave-de-prueba"},
			{Domain: "b.example.com", Placeholder: testPlace, Secret: "y-clave-de-prueba"}},
		"sin secreto":        {{Domain: "a.example.com", Placeholder: testPlace}},
		"marcador de otro":   {{Domain: "a.example.com", Placeholder: "sk_live_x", Secret: "x-clave-de-prueba"}},
		"prefijo a secas":    {{Domain: "a.example.com", Placeholder: PlaceholderPrefix, Secret: "x-clave-de-prueba"}},
		"dominio con puerto": {{Domain: "a.example.com:443", Placeholder: testPlace, Secret: "x-clave-de-prueba"}},
	} {
		if err := ValidarCredenciales(c); err == nil {
			t.Errorf("%s: debería rechazarse", nombre)
		}
	}
}

// Un cuerpo declarado por encima del tope se rechaza en el acto, sin llegar al
// proveedor ni esperar al plazo.
func TestProxyRechazaUnCuerpoDemasiadoGrande(t *testing.T) {
	srv, hits := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {})
	req, _ := http.NewRequest("POST", srv.URL+"/", strings.NewReader("x"))
	req.Host = "example.com"
	req.ContentLength = MaxBody + 1
	resp, err := (&http.Transport{}).RoundTrip(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("status %d, quería 413", resp.StatusCode)
		}
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Errorf("el proveedor recibió %d peticiones", *hits)
	}
}

// Una clave corta corrompía las respuestas: con "abc", un "abcdef" del
// proveedor llegaba como "kling-cred-…def". Se rechaza al registrarla.
func TestValidarCredencialesClaveCorta(t *testing.T) {
	for _, secreto := range []string{"a", "abc", "1234567"} {
		c := []Credential{{Domain: "a.example.com", Placeholder: testPlace, Secret: secreto}}
		if err := ValidarCredenciales(c); err == nil || !strings.Contains(err.Error(), "8-4096 bytes") {
			t.Errorf("%q: %v", secreto, err)
		}
		p := New(Options{})
		if _, err := p.SetCredentials(c); err == nil {
			t.Errorf("%q: el proxy la aceptó", secreto)
		}
	}
	c := []Credential{{Domain: "a.example.com", Placeholder: testPlace, Secret: "12345678"}}
	if err := ValidarCredenciales(c); err != nil {
		t.Errorf("8 bytes: %v", err)
	}
}
