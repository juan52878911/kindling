package frontal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

const dominioTest = "sbx.test"

// ingresoFalso es lo que el daemon falso sabe de publicar puertos y de /guest.
type ingresoFalso struct {
	mu         sync.Mutex
	pubs       map[string][]api.PortPublish // máquina -> puertos publicados
	backend    map[int]int                  // puerto del invitado -> puerto real del servidor de pruebas
	thaws      int
	publicadas int
	guestVisto *api.GuestRequest
}

func (f *falso) rutasIngreso(m *http.ServeMux) {
	m.HandleFunc("POST /machines/{ref}/thaw", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		mc, ok := f.maquinas[r.PathValue("ref")]
		if ok {
			mc.State = api.StateRunning
		}
		f.mu.Unlock()
		if !ok {
			failDaemon(w, 404, fmt.Errorf("no machine"))
			return
		}
		f.ing.mu.Lock()
		f.ing.thaws++
		f.ing.mu.Unlock()
		writeJSON(w, 200, mc)
	})
	m.HandleFunc("GET /publish", func(w http.ResponseWriter, r *http.Request) {
		f.ing.mu.Lock()
		defer f.ing.mu.Unlock()
		out := []api.Publication{}
		for ref, ps := range f.ing.pubs {
			out = append(out, api.Publication{Machine: ref, Ports: ps})
		}
		writeJSON(w, 200, out)
	})
	m.HandleFunc("POST /machines/{ref}/publish", func(w http.ResponseWriter, r *http.Request) {
		var req api.PublishRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.ing.mu.Lock()
		defer f.ing.mu.Unlock()
		ref := r.PathValue("ref")
		var out []api.PortPublish
		for _, p := range req.Ports {
			real, ok := f.ing.backend[p.Guest]
			if !ok {
				real = 1 // nadie escucha ahí
			}
			p.Host = real
			out = append(out, p)
		}
		if f.ing.pubs == nil {
			f.ing.pubs = map[string][]api.PortPublish{}
		}
		f.ing.pubs[ref] = append(f.ing.pubs[ref], out...)
		f.ing.publicadas++
		writeJSON(w, 200, api.Publication{Machine: ref, Ports: out})
	})
	m.HandleFunc("POST /machines/{ref}/guest", func(w http.ResponseWriter, r *http.Request) {
		var req api.GuestRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.ing.mu.Lock()
		f.ing.guestVisto = &req
		f.ing.mu.Unlock()
		if req.Port != 3000 {
			failDaemon(w, 403, fmt.Errorf("port %d is not exposed", req.Port))
			return
		}
		// Como el daemon real: solo GuestPort o los de kling.ports.
		if !f.puertoExpuesto(r.PathValue("ref"), req.Port) {
			failDaemon(w, 403, fmt.Errorf("port %d is not exposed: list it in its %s label", req.Port, api.LabelPorts))
			return
		}
		writeJSON(w, 200, api.GuestResponse{Status: 201, Body: "via guest " + req.Path, Headers: map[string]string{"Content-Type": "text/plain"}})
	})
}

// puertoExpuesto aplica la regla del daemon real (tunel.go, server.go): solo
// api.GuestPort o los puertos de la etiqueta kling.ports de la máquina.
func (f *falso) puertoExpuesto(ref string, puerto int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	mc, ok := f.maquinas[ref]
	return ok && mc.Exposes(puerto)
}

// servicioInvitado es el servidor que "escucha dentro del sandbox": enseña las
// cabeceras que le llegaron y hace de eco por WebSocket (a mano: sin
// dependencias, solo el apretón y bytes de ida y vuelta).
func servicioInvitado(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			c, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer c.Close()
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			rw.Flush()
			buf := make([]byte, 64)
			for {
				n, err := rw.Read(buf)
				if err != nil {
					return
				}
				c.Write([]byte("eco:" + string(buf[:n])))
			}
		}
		w.Header().Set("Set-Cookie", "app=1; Path=/")
		fmt.Fprintf(w, "path=%s auth=%q cookie=%q fwd=%q", r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("X-Forwarded-Host"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type bancoIngreso struct {
	f       *falso
	srv     *httptest.Server
	s       *Servidor
	sb      *api.Machine
	id      string
	guest   *httptest.Server
	puertoG int
}

func montarIngreso(t *testing.T, mod func(*Opciones)) *bancoIngreso {
	t.Helper()
	f := nuevoFalso(t)
	guest := servicioInvitado(t)
	_, p, _ := net.SplitHostPort(guest.Listener.Addr().String())
	pg, _ := strconv.Atoi(p)
	f.ing.backend = map[int]int{3000: pg}
	sb := f.sembrar(&api.Machine{Labels: map[string]string{LabelTenant: "alice"}})
	o := Opciones{
		Hosts:   hosts.Nuevo(map[string]string{"a": f.endpoint}),
		Tenants: []Tenant{{Nombre: "alice", Token: tokenAlice}, {Nombre: "bob", Token: tokenBob}},
		Log:     testLog(t),
		Ingreso: IngresoOpciones{Dominio: dominioTest, Esquema: "http", Secreto: []byte("secreto-de-prueba-123")},
	}
	if mod != nil {
		mod(&o)
	}
	s, err := Nuevo(o)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &bancoIngreso{f: f, srv: srv, s: s, sb: sb, id: "a/" + sb.ID, guest: guest, puertoG: pg}
}

func (b *bancoIngreso) host(puerto int) string {
	return fmt.Sprintf("%d-%s.%s", puerto, hosts.IDaDNS(b.id), dominioTest)
}

// get hace GET al subdominio del puerto con las cabeceras dadas.
func (b *bancoIngreso) get(t *testing.T, puerto int, ruta string, cab map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", b.srv.URL+ruta, nil)
	req.Host = b.host(puerto)
	for k, v := range cab {
		req.Header.Set(k, v)
	}
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (b *bancoIngreso) endpoint(t *testing.T, tok string, pet EndpointPeticion) (int, Endpoint) {
	t.Helper()
	resp := pide(t, b.srv, tok, "POST", "/v1/sandboxes/"+b.id+"/endpoints", pet)
	code := resp.StatusCode
	if code != 201 {
		resp.Body.Close()
		return code, Endpoint{}
	}
	return code, decodifica[Endpoint](t, resp)
}

func TestIngresoHTTPConTokenDelTenantYCabecerasLimpias(t *testing.T) {
	b := montarIngreso(t, nil)
	resp, body := b.get(t, 3000, "/hola?x=1", map[string]string{
		"Authorization": "Bearer " + tokenAlice,
		"Cookie":        "app=1; " + cookieEndpoint + "=zzz",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `path=/hola?x=1`) || !strings.Contains(body, `auth=""`) || !strings.Contains(body, `cookie="app=1"`) {
		t.Fatalf("el invitado vio algo del gateway: %s", body)
	}
	if strings.Contains(body, tokenAlice) {
		t.Fatalf("el token del tenant llegó al invitado: %s", body)
	}
	if got := resp.Header.Get("Set-Cookie"); got != "app=1; Path=/" {
		t.Fatalf("la cookie del invitado no volvió: %q", got)
	}
	// La primera vez publicó el puerto; la segunda reutiliza la publicación.
	b.get(t, 3000, "/", map[string]string{"Authorization": "Bearer " + tokenAlice})
	if b.f.ing.publicadas != 1 {
		t.Fatalf("publicaciones = %d, quería 1", b.f.ing.publicadas)
	}
}

func TestIngresoSinTokenOTokenAjeno(t *testing.T) {
	b := montarIngreso(t, nil)
	if resp, _ := b.get(t, 3000, "/", nil); resp.StatusCode != 401 {
		t.Fatalf("sin token = %d, quería 401", resp.StatusCode)
	}
	if resp, _ := b.get(t, 3000, "/", map[string]string{"Authorization": "Bearer nada-que-ver-1234567"}); resp.StatusCode != 401 {
		t.Fatalf("token inválido = %d, quería 401", resp.StatusCode)
	}
	// Bob existe pero el sandbox no es suyo: igual que si no existiera.
	if resp, _ := b.get(t, 3000, "/", map[string]string{"Authorization": "Bearer " + tokenBob}); resp.StatusCode != 404 {
		t.Fatalf("tenant ajeno = %d, quería 404", resp.StatusCode)
	}
	// Un sandbox inexistente sin token no se distingue de uno que existe.
	b.id = "a/0000000000000000"
	if resp, _ := b.get(t, 3000, "/", nil); resp.StatusCode != 401 {
		t.Fatalf("inexistente sin token = %d, quería 401", resp.StatusCode)
	}
	if b.f.ing.publicadas != 0 {
		t.Fatal("se publicó un puerto sin autorización")
	}
}

func TestIngresoTokenDeEndpoint(t *testing.T) {
	b := montarIngreso(t, nil)
	if code, _ := b.endpoint(t, tokenBob, EndpointPeticion{Port: 3000}); code != 404 {
		t.Fatalf("endpoint de sandbox ajeno = %d, quería 404", code)
	}
	if code, _ := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 0}); code != 400 {
		t.Fatalf("puerto 0 = %d, quería 400", code)
	}
	code, ep := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 3000, TTLSeconds: 60})
	if code != 201 || ep.Token == "" || ep.Public {
		t.Fatalf("endpoint: %d %+v", code, ep)
	}
	if want := "http://" + b.host(3000) + "." + ""; !strings.HasPrefix(ep.URL, "http://3000-") || !strings.HasSuffix(ep.URL, "."+dominioTest+"/") {
		t.Fatalf("url %q (esperaba algo como %s)", ep.URL, want)
	}

	// Por Authorization (el de endpoint), por header propio y por query.
	for _, cab := range []map[string]string{
		{"Authorization": "Bearer " + ep.Token}, {cabeceraEndpoint: ep.Token},
	} {
		resp, body := b.get(t, 3000, "/", cab)
		if resp.StatusCode != 200 || !strings.Contains(body, `auth=""`) {
			t.Fatalf("%v: %d %s", cab, resp.StatusCode, body)
		}
	}
	// Otro puerto con el mismo token: no.
	if resp, _ := b.get(t, 4000, "/", map[string]string{cabeceraEndpoint: ep.Token}); resp.StatusCode != 401 {
		t.Fatalf("token de otro puerto = %d, quería 401", resp.StatusCode)
	}
	// Por query una vez: redirige sin el token y deja cookie HttpOnly.
	resp, _ := b.get(t, 3000, "/p?a=1&"+paramEndpoint+"="+ep.Token, nil)
	if resp.StatusCode != 303 || strings.Contains(resp.Header.Get("Location"), paramEndpoint) || resp.Header.Get("Location") != "/p?a=1" {
		t.Fatalf("query: %d Location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieEndpoint {
			ck = c
		}
	}
	if ck == nil || !ck.HttpOnly || ck.Value != ep.Token || ck.MaxAge <= 0 {
		t.Fatalf("cookie: %+v", ck)
	}
	resp, body := b.get(t, 3000, "/p", map[string]string{"Cookie": ck.Name + "=" + ck.Value})
	if resp.StatusCode != 200 || strings.Contains(body, ep.Token) || strings.Contains(body, cookieEndpoint) {
		t.Fatalf("con cookie: %d %s", resp.StatusCode, body)
	}
}

func TestIngresoTokenCaducadoOFirmaAjena(t *testing.T) {
	b := montarIngreso(t, nil)
	g := b.s.ingreso
	caducado := g.firmar(cargaEndpoint{Tenant: "alice", ID: b.id, Puerto: 3000, Exp: time.Now().Add(-time.Second).Unix()})
	if resp, _ := b.get(t, 3000, "/", map[string]string{cabeceraEndpoint: caducado}); resp.StatusCode != 401 {
		t.Fatalf("caducado = %d, quería 401", resp.StatusCode)
	}
	vigente := g.firmar(cargaEndpoint{Tenant: "alice", ID: b.id, Puerto: 3000, Exp: time.Now().Add(time.Minute).Unix()})
	if resp, _ := b.get(t, 3000, "/", map[string]string{cabeceraEndpoint: vigente}); resp.StatusCode != 200 {
		t.Fatalf("vigente = %d", resp.StatusCode)
	}
	// Mismo contenido con otro secreto (otro gateway) o manipulado.
	otro := montarIngreso(t, func(o *Opciones) { o.Ingreso.Secreto = []byte("otro-secreto-distinto-1") })
	ajeno := otro.s.ingreso.firmar(cargaEndpoint{Tenant: "alice", ID: b.id, Puerto: 3000, Exp: time.Now().Add(time.Minute).Unix()})
	if resp, _ := b.get(t, 3000, "/", map[string]string{cabeceraEndpoint: ajeno}); resp.StatusCode != 401 {
		t.Fatalf("firma ajena = %d, quería 401", resp.StatusCode)
	}
	partes := strings.Split(vigente, ".")
	partes[1] = partes[1][:len(partes[1])-2] + "AA"
	if resp, _ := b.get(t, 3000, "/", map[string]string{cabeceraEndpoint: strings.Join(partes, ".")}); resp.StatusCode != 401 {
		t.Fatalf("manipulado = %d, quería 401", resp.StatusCode)
	}
}

func TestIngresoTokenDeOtroSandbox(t *testing.T) {
	b := montarIngreso(t, nil)
	otro := b.f.sembrar(&api.Machine{Labels: map[string]string{LabelTenant: "alice"}})
	tok := b.s.ingreso.firmar(cargaEndpoint{Tenant: "alice", ID: "a/" + otro.ID, Puerto: 3000, Exp: time.Now().Add(time.Minute).Unix()})
	if resp, _ := b.get(t, 3000, "/", map[string]string{cabeceraEndpoint: tok}); resp.StatusCode != 401 {
		t.Fatalf("token de otro sandbox = %d, quería 401", resp.StatusCode)
	}
	// Y si el tenant del token ya no es dueño (el sandbox cambió de manos), 404.
	b.f.mu.Lock()
	b.sb.Labels[LabelTenant] = "bob"
	b.f.mu.Unlock()
	b.s.ingreso.olvidar(b.id)
	tok = b.s.ingreso.firmar(cargaEndpoint{Tenant: "alice", ID: b.id, Puerto: 3000, Exp: time.Now().Add(time.Minute).Unix()})
	if resp, _ := b.get(t, 3000, "/", map[string]string{cabeceraEndpoint: tok}); resp.StatusCode != 404 {
		t.Fatalf("token de quien ya no es dueño = %d, quería 404", resp.StatusCode)
	}
}

func TestIngresoEndpointPublico(t *testing.T) {
	b := montarIngreso(t, nil)
	if resp, _ := b.get(t, 3000, "/", nil); resp.StatusCode != 401 {
		t.Fatalf("antes de hacerlo público = %d", resp.StatusCode)
	}
	code, ep := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 3000, Public: true, TTLSeconds: 120})
	if code != 201 || !ep.Public || ep.Token != "" {
		t.Fatalf("público: %d %+v", code, ep)
	}
	if resp, body := b.get(t, 3000, "/", nil); resp.StatusCode != 200 {
		t.Fatalf("público sin token = %d %s", resp.StatusCode, body)
	}
	// El público es de ese puerto, no de todos.
	if resp, _ := b.get(t, 4000, "/", nil); resp.StatusCode != 401 {
		t.Fatalf("otro puerto = %d, quería 401", resp.StatusCode)
	}
	// Caducado: la etiqueta ya no vale.
	b.f.mu.Lock()
	b.sb.Labels[LabelPublico] = "3000:" + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	b.f.mu.Unlock()
	b.s.ingreso.olvidar(b.id)
	if resp, _ := b.get(t, 3000, "/", nil); resp.StatusCode != 401 {
		t.Fatalf("público caducado = %d, quería 401", resp.StatusCode)
	}
	// La etiqueta es interna: no sale en la vista del sandbox.
	resp := pide(t, b.srv, tokenAlice, "GET", "/v1/sandboxes/"+b.id, nil)
	if raw, _ := io.ReadAll(resp.Body); strings.Contains(string(raw), LabelPublico) {
		t.Fatalf("la etiqueta interna se filtra: %s", raw)
	}
}

func TestIngresoDespiertaAlPrimerAcceso(t *testing.T) {
	b := montarIngreso(t, nil)
	b.f.mu.Lock()
	b.sb.State = api.StateWarm
	b.f.mu.Unlock()
	resp, body := b.get(t, 3000, "/", map[string]string{"Authorization": "Bearer " + tokenAlice})
	if resp.StatusCode != 200 {
		t.Fatalf("congelado = %d %s", resp.StatusCode, body)
	}
	if b.f.ing.thaws != 1 || b.f.maquina(b.sb.ID).State != api.StateRunning {
		t.Fatalf("thaws=%d estado=%s", b.f.ing.thaws, b.f.maquina(b.sb.ID).State)
	}
	// El segundo acceso ya no despierta nada.
	b.get(t, 3000, "/", map[string]string{"Authorization": "Bearer " + tokenAlice})
	if b.f.ing.thaws != 1 {
		t.Fatalf("thaws = %d tras el segundo acceso", b.f.ing.thaws)
	}
}

func TestIngresoNadieEscucha502(t *testing.T) {
	b := montarIngreso(t, nil)
	resp, body := b.get(t, 5555, "/", map[string]string{"Authorization": "Bearer " + tokenAlice})
	if resp.StatusCode != 502 || !strings.Contains(body, "5555") || strings.Contains(body, "127.0.0.1") {
		t.Fatalf("sin servicio = %d %s", resp.StatusCode, body)
	}
}

func TestIngresoWebSocketDeIdaYVuelta(t *testing.T) {
	b := montarIngreso(t, nil)
	c, err := net.Dial("tcp", strings.TrimPrefix(b.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nAuthorization: Bearer %s\r\n\r\n", b.host(3000), tokenAlice)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("apretón: %v %v", resp, err)
	}
	for _, msg := range []string{"uno", "dos"} {
		fmt.Fprint(c, msg)
		buf := make([]byte, len("eco:")+len(msg))
		if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "eco:"+msg {
			t.Fatalf("eco de %q = %q, %v", msg, buf, err)
		}
	}
	// Sin credencial, ni apretón.
	c2, _ := net.Dial("tcp", strings.TrimPrefix(b.srv.URL, "http://"))
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c2, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", b.host(3000))
	if r2, err := http.ReadResponse(bufio.NewReader(c2), nil); err != nil || r2.StatusCode != 401 {
		t.Fatalf("WebSocket sin token: %v %v", r2, err)
	}
}

func TestIngresoTopes(t *testing.T) {
	b := montarIngreso(t, func(o *Opciones) { o.Ingreso.MaxCabeceras = 512; o.Ingreso.MaxConexiones = 1 })
	cab := map[string]string{"Authorization": "Bearer " + tokenAlice}
	if resp, _ := b.get(t, 3000, "/", map[string]string{"Authorization": "Bearer " + tokenAlice, "X-Grande": strings.Repeat("x", 600)}); resp.StatusCode != 431 {
		t.Fatalf("cabeceras grandes = %d, quería 431", resp.StatusCode)
	}
	// Un WebSocket abierto ocupa el único hueco; la siguiente petición es 429.
	c, _ := net.Dial("tcp", strings.TrimPrefix(b.srv.URL, "http://"))
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nAuthorization: Bearer %s\r\n\r\n", b.host(3000), tokenAlice)
	if r, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil || r.StatusCode != 101 {
		t.Fatalf("apretón: %v %v", r, err)
	}
	if resp, _ := b.get(t, 3000, "/", cab); resp.StatusCode != 429 {
		t.Fatalf("segunda conexión = %d, quería 429", resp.StatusCode)
	}
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if resp, _ := b.get(t, 3000, "/", cab); resp.StatusCode == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("el hueco no se liberó al cerrar el WebSocket")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIngresoHostRemotoPorGuest(t *testing.T) {
	b := montarIngreso(t, func(o *Opciones) { o.Ingreso.SoloGuest = true })
	cab := map[string]string{"Authorization": "Bearer " + tokenAlice, "Cookie": cookieEndpoint + "=z"}
	resp, body := b.get(t, 3000, "/x?y=2", cab)
	if resp.StatusCode != 201 || body != "via guest /x?y=2" {
		t.Fatalf("guest: %d %q", resp.StatusCode, body)
	}
	g := b.f.ing.guestVisto
	for k := range g.Headers {
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Cookie") {
			t.Fatalf("cabecera del gateway llegó al invitado: %v", g.Headers)
		}
	}
	if b.f.ing.publicadas != 0 {
		t.Fatal("un host remoto no publica en 127.0.0.1")
	}
	// Puerto no declarado: el daemon dice 403 y el cliente ve un 502 explicado.
	resp, body = b.get(t, 4000, "/", cab)
	if resp.StatusCode != 502 || !strings.Contains(body, api.LabelPorts) {
		t.Fatalf("puerto no declarado: %d %s", resp.StatusCode, body)
	}
	// WebSocket no cabe en /guest.
	cab["Upgrade"], cab["Connection"] = "websocket", "Upgrade"
	if resp, _ := b.get(t, 3000, "/", cab); resp.StatusCode != 501 {
		t.Fatalf("WebSocket por guest = %d, quería 501", resp.StatusCode)
	}
}

func TestIngresoApagadoYHostRaro(t *testing.T) {
	b := montarIngreso(t, func(o *Opciones) { o.Ingreso = IngresoOpciones{} })
	if code, _ := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 3000}); code != 501 {
		t.Fatalf("sin ingress = %d, quería 501", code)
	}
	b = montarIngreso(t, nil)
	req, _ := http.NewRequest("GET", b.srv.URL+"/", nil)
	req.Host = "nada." + dominioTest
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 404 {
		t.Fatalf("host sin forma = %v %v", resp, err)
	}
	// El dominio de la API no se confunde con el de ingress.
	if resp := pide(t, b.srv, "", "GET", "/v1/health", nil); resp.StatusCode != 200 {
		t.Fatalf("health = %d", resp.StatusCode)
	}
}

// C14: listar y revocar endpoints.
func TestIngresoListarYRevocarEndpoints(t *testing.T) {
	b := montarIngreso(t, nil)
	base := "/v1/sandboxes/" + b.id + "/endpoints"
	_, pub := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 3000, Public: true, TTLSeconds: 600})
	_, tok := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 3000, TTLSeconds: 600})
	_, otro := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 4000, TTLSeconds: 600})
	if resp, _ := b.get(t, 3000, "/", map[string]string{cabeceraEndpoint: tok.Token}); resp.StatusCode != 200 {
		t.Fatalf("token antes de revocar = %d", resp.StatusCode)
	}

	resp := pide(t, b.srv, tokenAlice, "GET", base, nil)
	esperaCodigo(t, resp, 200)
	lista := decodifica[[]Endpoint](t, resp)
	if len(lista) != 1 || lista[0].Port != 3000 || !lista[0].Public || lista[0].URL != pub.URL || lista[0].Token != "" {
		t.Fatalf("lista: %+v", lista)
	}
	esperaCodigo(t, pide(t, b.srv, tokenBob, "GET", base, nil), 404)
	esperaCodigo(t, pide(t, b.srv, tokenBob, "DELETE", base+"/3000", nil), 404)
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "DELETE", base+"/abc", nil), 400)

	esperaCodigo(t, pide(t, b.srv, tokenAlice, "DELETE", base+"/3000", nil), 204)
	if resp, _ := b.get(t, 3000, "/", nil); resp.StatusCode != 401 {
		t.Fatalf("público tras revocar = %d, quería 401", resp.StatusCode)
	}
	if resp, _ := b.get(t, 3000, "/", map[string]string{cabeceraEndpoint: tok.Token}); resp.StatusCode != 401 {
		t.Fatalf("token tras revocar = %d, quería 401", resp.StatusCode)
	}
	// Otro puerto, intacto; y un token NUEVO del puerto revocado vale.
	if resp, _ := b.get(t, 4000, "/", map[string]string{cabeceraEndpoint: otro.Token}); resp.StatusCode == 401 {
		t.Fatalf("el token de otro puerto también se revocó")
	}
	_, nuevo := b.endpoint(t, tokenAlice, EndpointPeticion{Port: 3000, TTLSeconds: 600})
	if resp, _ := b.get(t, 3000, "/", map[string]string{cabeceraEndpoint: nuevo.Token}); resp.StatusCode != 200 {
		t.Fatalf("token nuevo = %d, quería 200", resp.StatusCode)
	}
	resp = pide(t, b.srv, tokenAlice, "GET", base, nil)
	if l := decodifica[[]Endpoint](t, resp); len(l) != 0 {
		t.Fatalf("lista tras revocar: %+v", l)
	}
}
