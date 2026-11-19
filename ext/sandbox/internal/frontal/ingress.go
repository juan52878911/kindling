package frontal

// Entrada HTTP a puertos del sandbox: <puerto>-<id>.<dominio>.
//
// TRANSPORTE. El núcleo no tiene un túnel TCP hacia el invitado: lo más cercano
// es POST /machines/{ref}/guest (JSON bufferizado, texto, sin WebSocket) y los
// puertos que el daemon publica en 127.0.0.1 del host (api.Publish), que además
// despiertan una máquina congelada al primera conexión. Este fichero no inventa
// un tercer camino:
//
//   - host local (socket Unix): se publica el puerto del invitado si no lo
//     estaba y se hace de proxy inverso a 127.0.0.1:<puerto publicado>. Es
//     streaming completo: cuerpos binarios, SSE y WebSocket.
//   - host remoto (ssh://): 127.0.0.1 sería el del otro extremo, así que solo
//     queda /guest: HTTP de texto con cuerpo acotado, y el puerto tiene que
//     estar en la etiqueta kling.ports. WebSocket da 501.
//
// AUTORIZACIÓN. Bearer del tenant dueño, o un token de endpoint (ver
// handleEndpoint) por Authorization, X-Kling-Endpoint-Token, ?kling_token= (una
// vez: queda en una cookie HttpOnly del subdominio) o la cookie. Un endpoint
// público no pide nada. El invitado NUNCA ve el token del tenant ni el de
// endpoint: se quitan Authorization, la cookie del gateway y el parámetro.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// LabelPublico lista los puertos públicos del sandbox como "puerto:caducidad
// unix,...". Va en el espacio reservado: el cliente no la ve ni la pone.
const LabelPublico = "kling.sandbox.public"

const (
	cookieEndpoint   = "kling_ep"
	paramEndpoint    = "kling_token"
	cabeceraEndpoint = "X-Kling-Endpoint-Token"

	ttlEndpointPorDefecto = time.Hour
	ttlEndpointMax        = 7 * 24 * time.Hour
	vidaCache             = 3 * time.Second
	maxCuerpoGuest        = 1 << 20
)

// IngresoOpciones configura la entrada a puertos. Dominio vacío = apagada.
type IngresoOpciones struct {
	// Dominio es el sufijo: los sandboxes se sirven en <puerto>-<id>.<Dominio>,
	// que necesita un DNS comodín (*.<Dominio>) y un certificado comodín.
	Dominio string
	// Esquema de las URL que se devuelven: "https" (por defecto) o "http".
	Esquema string
	// Secreto firma los tokens de endpoint. Vacío: uno al azar por proceso (los
	// tokens mueren al reiniciar y no valen en otro frontal).
	Secreto []byte
	// MaxCabeceras acota el tamaño de las cabeceras de la petición (0 = 32 KiB).
	MaxCabeceras int
	// MaxConexiones acota las peticiones y WebSockets simultáneos por sandbox
	// (0 = 64).
	MaxConexiones int
	// EsperaCabeceras es cuánto se espera a las cabeceras de la respuesta del
	// invitado (0 = 60 s). No acota el cuerpo ni un WebSocket.
	EsperaCabeceras time.Duration
	// SoloGuest fuerza el camino de hosts remotos (/guest) también en locales.
	// Solo lo usan los tests.
	SoloGuest bool
}

type ingreso struct {
	s       *Servidor
	o       IngresoOpciones
	sufijo  string // ".dominio" en minúsculas
	secreto []byte
	tr      *http.Transport

	mu    sync.Mutex
	cache map[string]*entrada
	conns map[string]int
}

// entrada es lo último que se sabe de un sandbox, durante vidaCache.
type entrada struct {
	mu      sync.Mutex // serializa descongelar y publicar
	h       *hosts.Host
	mc      *api.Machine
	hasta   time.Time
	puertos map[int]int // puerto del invitado -> puerto publicado en el host
}

func nuevoIngreso(s *Servidor, o IngresoOpciones) (*ingreso, error) {
	if o.Dominio == "" {
		return nil, nil
	}
	d := strings.ToLower(strings.Trim(o.Dominio, "."))
	if d == "" || strings.ContainsAny(d, "/: *") {
		return nil, fmt.Errorf("ingress domain %q is not a bare domain name", o.Dominio)
	}
	switch o.Esquema {
	case "":
		o.Esquema = "https"
	case "http", "https":
	default:
		return nil, fmt.Errorf("ingress scheme %q: use http or https", o.Esquema)
	}
	if o.MaxCabeceras <= 0 {
		o.MaxCabeceras = 32 << 10
	}
	if o.MaxConexiones <= 0 {
		o.MaxConexiones = 64
	}
	if o.EsperaCabeceras <= 0 {
		o.EsperaCabeceras = 60 * time.Second
	}
	if len(o.Secreto) == 0 {
		o.Secreto = make([]byte, 32)
		if _, err := rand.Read(o.Secreto); err != nil {
			return nil, err
		}
	} else if len(o.Secreto) < 16 {
		return nil, errors.New("ingress secret must be at least 16 bytes")
	}
	return &ingreso{
		s: s, o: o, sufijo: "." + d, secreto: o.Secreto,
		tr: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			ResponseHeaderTimeout: o.EsperaCabeceras,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       30 * time.Second,
		},
		cache: map[string]*entrada{}, conns: map[string]int{},
	}, nil
}

// ---- tokens de endpoint (sin estado: firmados con HMAC)

type cargaEndpoint struct {
	Tenant string `json:"t"`
	ID     string `json:"s"`
	Puerto int    `json:"p"`
	Exp    int64  `json:"e"`
}

func (g *ingreso) firmar(c cargaEndpoint) string {
	b, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, g.secreto)
	m.Write([]byte(p))
	return "kep1." + p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// verificar devuelve la carga de un token bien firmado y no caducado.
func (g *ingreso) verificar(tok string) (cargaEndpoint, bool) {
	var c cargaEndpoint
	partes := strings.Split(tok, ".")
	if len(partes) != 3 || partes[0] != "kep1" {
		return c, false
	}
	mac, err := base64.RawURLEncoding.DecodeString(partes[2])
	if err != nil {
		return c, false
	}
	m := hmac.New(sha256.New, g.secreto)
	m.Write([]byte(partes[1]))
	if !hmac.Equal(mac, m.Sum(nil)) {
		return c, false
	}
	b, err := base64.RawURLEncoding.DecodeString(partes[1])
	if err != nil || json.Unmarshal(b, &c) != nil {
		return c, false
	}
	return c, time.Now().Unix() < c.Exp
}

// ---- POST /v1/sandboxes/{id}/endpoints

// EndpointPeticion es el cuerpo de POST /v1/sandboxes/{id}/endpoints.
type EndpointPeticion struct {
	Port       int  `json:"port"`
	TTLSeconds int  `json:"ttl_seconds,omitempty"`
	Public     bool `json:"public,omitempty"`
}

// Endpoint es la respuesta: la URL y, salvo si es público, el token que la abre.
type Endpoint struct {
	URL       string    `json:"url"`
	Port      int       `json:"port"`
	Token     string    `json:"token,omitempty"`
	Public    bool      `json:"public,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Servidor) handleEndpoint(w http.ResponseWriter, r *http.Request, id string) {
	g := s.ingreso
	if g == nil {
		fail(w, http.StatusNotImplemented, errors.New("port ingress is not configured on this gateway (start it with -ingress-domain)"))
		return
	}
	var pet EndpointPeticion
	if err := leerJSON(r, &pet); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := (api.PortPublish{Guest: pet.Port}).Validate(); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ttl := time.Duration(pet.TTLSeconds) * time.Second
	switch {
	case pet.TTLSeconds < 0 || ttl > ttlEndpointMax:
		fail(w, http.StatusBadRequest, fmt.Errorf("ttl_seconds must be between 1 and %d", int(ttlEndpointMax.Seconds())))
		return
	case pet.TTLSeconds == 0:
		ttl = ttlEndpointPorDefecto
	}
	etiqueta := strconv.Itoa(pet.Port) + "-" + hosts.IDaDNS(id)
	if len(etiqueta) > hosts.MaxEtiquetaDNS {
		fail(w, http.StatusUnprocessableEntity, fmt.Errorf("the id %q is too long to be a DNS label with the port (%d > %d)", id, len(etiqueta), hosts.MaxEtiquetaDNS))
		return
	}
	t := tenantDe(r)
	h, mc, err := s.buscar(r.Context(), t, id)
	if err != nil {
		fail(w, codigoBuscar(err), err)
		return
	}
	exp := time.Now().Add(ttl)
	out := Endpoint{
		URL:  g.o.Esquema + "://" + etiqueta + g.sufijo + "/",
		Port: pet.Port, Public: pet.Public, ExpiresAt: exp.UTC().Truncate(time.Second),
	}
	if pet.Public {
		lab := publicosCon(mc.Labels[LabelPublico], pet.Port, exp, time.Now())
		if err := h.Cliente.SetLabels(r.Context(), mc.ID, map[string]string{LabelPublico: lab}); err != nil {
			fail(w, http.StatusBadGateway, deHost(h, err))
			return
		}
		g.olvidar(id)
	} else {
		out.Token = g.firmar(cargaEndpoint{Tenant: t.Nombre, ID: id, Puerto: pet.Port, Exp: exp.Unix()})
	}
	writeJSON(w, http.StatusCreated, out)
}

// publicosCon suma (puerto, exp) a la lista de la etiqueta y quita lo caducado.
func publicosCon(actual string, puerto int, exp, ahora time.Time) string {
	m := publicosDe(actual, ahora)
	m[puerto] = exp.Unix()
	var l []string
	for p, e := range m {
		l = append(l, strconv.Itoa(p)+":"+strconv.FormatInt(e, 10))
	}
	return strings.Join(l, ",")
}

func publicosDe(etiqueta string, ahora time.Time) map[int]int64 {
	m := map[int]int64{}
	for _, par := range strings.Split(etiqueta, ",") {
		p, e, ok := strings.Cut(par, ":")
		n, err1 := strconv.Atoi(p)
		x, err2 := strconv.ParseInt(e, 10, 64)
		if ok && err1 == nil && err2 == nil && x > ahora.Unix() {
			m[n] = x
		}
	}
	return m
}

// ---- servir

// esDeIngreso dice si el Host de la petición es <puerto>-<id>.<dominio>.
func (g *ingreso) esDeIngreso(r *http.Request) bool {
	h := strings.ToLower(r.Host)
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.HasSuffix(h, g.sufijo) && len(h) > len(g.sufijo)
}

// parsear saca puerto e id del Host.
func (g *ingreso) parsear(r *http.Request) (puerto int, id string, ok bool) {
	h := strings.ToLower(r.Host)
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	et := strings.TrimSuffix(h, g.sufijo)
	p, enc, ok := strings.Cut(et, "-")
	if !ok || strings.Contains(enc, ".") {
		return 0, "", false
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 || p != strconv.Itoa(n) {
		return 0, "", false
	}
	id, err = hosts.IDdeDNS(enc)
	if err != nil {
		return 0, "", false
	}
	if _, _, ok := partirID(id); !ok {
		return 0, "", false
	}
	return n, id, true
}

func tamanoCabeceras(h http.Header) int {
	n := 0
	for k, vs := range h {
		for _, v := range vs {
			n += len(k) + len(v) + 4
		}
	}
	return n
}

func (g *ingreso) servir(w http.ResponseWriter, r *http.Request) {
	puerto, id, ok := g.parsear(r)
	if !ok {
		fail(w, http.StatusNotFound, errors.New("not a sandbox address: expected <port>-<id>.<domain>"))
		return
	}
	if tamanoCabeceras(r.Header)+len(r.RequestURI) > g.o.MaxCabeceras {
		fail(w, http.StatusRequestHeaderFieldsTooLarge, errors.New("request headers are too large"))
		return
	}
	tok, delQuery := credencial(r)
	e, err := g.entrada(r.Context(), id)
	if err != nil {
		// Sin credencial no se dice si el sandbox existe.
		if tok == "" || errors.As(err, new(*errHost)) {
			g.fallo(w, tok, err)
			return
		}
		fail(w, http.StatusNotFound, err)
		return
	}
	if code, err := g.autorizar(e.mc, id, puerto, tok); err != nil {
		if code == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="kindling-sandbox-endpoint"`)
		}
		fail(w, code, err)
		return
	}
	// Token por query: pasa a cookie HttpOnly del subdominio y se va de la URL.
	if delQuery {
		http.SetCookie(w, &http.Cookie{
			Name: cookieEndpoint, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
			Secure: g.o.Esquema == "https", MaxAge: g.vidaToken(tok),
		})
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.Header.Get("Upgrade") == "" {
			u := *r.URL
			q := u.Query()
			q.Del(paramEndpoint)
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
			return
		}
	}
	if !g.tomar(id) {
		fail(w, http.StatusTooManyRequests, fmt.Errorf("this sandbox already has %d connections open through the ingress", g.o.MaxConexiones))
		return
	}
	defer g.soltar(id)

	hp, err := g.preparar(r.Context(), e, puerto)
	if err != nil {
		g.s.log.Printf("ingress %s:%d: %v", id, puerto, err)
		fail(w, codigoDeHost(err), &errPublico{msg: "could not bring the sandbox up for this request", err: err})
		return
	}
	limpiar(r)
	if hp == 0 {
		g.porGuest(w, r, e, puerto)
		return
	}
	g.proxy(w, r, id, puerto, hp)
}

// fallo contesta cuando no se pudo ni resolver el sandbox.
func (g *ingreso) fallo(w http.ResponseWriter, tok string, err error) {
	if tok == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="kindling-sandbox-endpoint"`)
		fail(w, http.StatusUnauthorized, errors.New("this endpoint needs a token"))
		return
	}
	fail(w, codigoBuscar(err), err)
}

// credencial busca el token en la petición. delQuery dice que vino por query
// (y por tanto hay que pasarlo a cookie).
func credencial(r *http.Request) (tok string, delQuery bool) {
	if t, ok := bearer(r.Header.Get("Authorization")); ok {
		return t, false
	}
	if t := r.Header.Get(cabeceraEndpoint); t != "" {
		return t, false
	}
	if t := r.URL.Query().Get(paramEndpoint); t != "" {
		return t, true
	}
	if c, err := r.Cookie(cookieEndpoint); err == nil {
		return c.Value, false
	}
	return "", false
}

func (g *ingreso) vidaToken(tok string) int {
	c, ok := g.verificar(tok)
	if !ok {
		return 0
	}
	return max(int(c.Exp-time.Now().Unix()), 1)
}

// autorizar decide si tok abre (id, puerto) del sandbox mc.
func (g *ingreso) autorizar(mc *api.Machine, id string, puerto int, tok string) (int, error) {
	no := func() (int, error) { return http.StatusUnauthorized, errors.New("this endpoint needs a valid token") }
	if tok == "" {
		// Sin token solo entra un puerto público y vigente.
		if exp, ok := publicosDe(mc.Labels[LabelPublico], time.Now())[puerto]; ok && exp > 0 && mc.Labels[LabelTenant] != "" {
			return 0, nil
		}
		return no()
	}
	var dueno string
	for i := range g.s.tenants { // sin cortar: tiempo constante, como auth
		if subtle.ConstantTimeCompare([]byte(tok), []byte(g.s.tenants[i].Token)) == 1 {
			dueno = g.s.tenants[i].Nombre
		}
	}
	if dueno == "" {
		c, ok := g.verificar(tok)
		if !ok || c.ID != id || c.Puerto != puerto {
			return no()
		}
		dueno = c.Tenant
	}
	for i := range g.s.tenants {
		if g.s.tenants[i].Nombre == dueno && esSandboxDe(mc, &g.s.tenants[i]) {
			return 0, nil
		}
	}
	// Token válido de quien no es dueño: la misma respuesta que un id que no existe.
	return http.StatusNotFound, noSandbox(id)
}

// limpiar quita de la petición lo que es del gateway: el invitado no debe ver
// ni el token del tenant ni el del endpoint.
func limpiar(r *http.Request) {
	r.Header.Del("Authorization")
	r.Header.Del(cabeceraEndpoint)
	if cs := r.Cookies(); len(cs) > 0 {
		r.Header.Del("Cookie")
		for _, c := range cs {
			if c.Name != cookieEndpoint {
				r.AddCookie(c)
			}
		}
	}
	if r.URL.Query().Has(paramEndpoint) {
		q := r.URL.Query()
		q.Del(paramEndpoint)
		r.URL.RawQuery = q.Encode()
		r.RequestURI = r.URL.RequestURI()
	}
}

// ---- conexiones por sandbox

func (g *ingreso) tomar(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conns[id] >= g.o.MaxConexiones {
		return false
	}
	g.conns[id]++
	return true
}

func (g *ingreso) soltar(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conns[id]--; g.conns[id] <= 0 {
		delete(g.conns, id)
	}
}

// ---- resolver, despertar y publicar

func (g *ingreso) olvidar(id string) {
	g.mu.Lock()
	delete(g.cache, id)
	g.mu.Unlock()
}

// entrada devuelve el sandbox id (sin mirar de quién es: eso lo decide
// autorizar), de la caché si es reciente.
func (g *ingreso) entrada(ctx context.Context, id string) (*entrada, error) {
	g.mu.Lock()
	e := g.cache[id]
	g.mu.Unlock()
	if e != nil {
		e.mu.Lock()
		vivo := time.Now().Before(e.hasta)
		e.mu.Unlock()
		if vivo {
			return e, nil
		}
	}
	nombre, ref, _ := partirID(id)
	h, ok := g.s.reg.Host(nombre)
	if !ok {
		return nil, noSandbox(id)
	}
	ms, err := h.Cliente.Sandboxes(ctx)
	if err != nil {
		return nil, deHost(h, err)
	}
	for _, mc := range ms {
		if mc.ID == ref && mc.Labels[LabelTenant] != "" {
			e := &entrada{h: h, mc: mc, hasta: time.Now().Add(vidaCache), puertos: map[int]int{}}
			g.mu.Lock()
			if len(g.cache) > 4096 {
				g.cache = map[string]*entrada{}
			}
			g.cache[id] = e
			g.mu.Unlock()
			return e, nil
		}
	}
	return nil, noSandbox(id)
}

// preparar despierta el sandbox si hace falta y devuelve el puerto del host
// al que se publicó puerto (0: host remoto, hay que ir por /guest).
func (g *ingreso) preparar(ctx context.Context, e *entrada, puerto int) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if hp, ok := e.puertos[puerto]; ok {
		return hp, nil
	}
	switch e.mc.State {
	case api.StateWarm, api.StatePaused:
		mc, err := e.h.Cliente.Thaw(ctx, e.mc.ID)
		if err != nil {
			return 0, deHost(e.h, err)
		}
		c := *e.mc
		c.State = mc.State
		e.mc = &c
	case api.StateRunning:
	default:
		return 0, &api.StatusError{Code: http.StatusConflict, Message: fmt.Sprintf("the sandbox is %s", e.mc.State)}
	}
	if !e.h.Local() || g.o.SoloGuest {
		e.puertos[puerto] = 0
		return 0, nil
	}
	pubs, err := e.h.Cliente.Publications(ctx)
	if err != nil {
		return 0, deHost(e.h, err)
	}
	hp := 0
	for _, p := range pubs {
		if p.Machine != e.mc.ID && p.Machine != e.mc.Name {
			continue
		}
		for _, pp := range p.Ports {
			if pp.Guest == puerto && pp.ProtoOr() == api.ProtoTCP {
				hp = pp.Host
			}
		}
	}
	if hp == 0 {
		p, err := e.h.Cliente.Publish(ctx, e.mc.ID, []api.PortPublish{{Guest: puerto}})
		if err != nil {
			return 0, deHost(e.h, err)
		}
		for _, pp := range p.Ports {
			if pp.Guest == puerto && pp.ProtoOr() == api.ProtoTCP {
				hp = pp.Host
			}
		}
		if hp == 0 {
			return 0, deHost(e.h, errors.New("the daemon did not say which host port it published"))
		}
	}
	e.puertos[puerto] = hp
	return hp, nil
}

// ---- proxy

func (g *ingreso) proxy(w http.ResponseWriter, r *http.Request, id string, puerto, hp int) {
	destino := net.JoinHostPort("127.0.0.1", strconv.Itoa(hp))
	rp := &httputil.ReverseProxy{
		Transport: g.tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = destino
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			g.olvidar(id)
			g.s.log.Printf("ingress %s:%d -> %s: %v", id, puerto, destino, err)
			var ne net.Error
			switch {
			case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, context.DeadlineExceeded):
				fail(w, http.StatusGatewayTimeout, &errPublico{msg: fmt.Sprintf("the sandbox did not answer on port %d in time", puerto), err: err})
			case errors.Is(err, context.Canceled):
				// El cliente se fue: nadie lee.
			default:
				fail(w, http.StatusBadGateway, &errPublico{msg: fmt.Sprintf("nothing is answering on port %d of the sandbox", puerto), err: err})
			}
		},
	}
	rp.ServeHTTP(w, r)
}

// porGuest es el camino de hosts remotos: POST /machines/{ref}/guest.
func (g *ingreso) porGuest(w http.ResponseWriter, r *http.Request, e *entrada, puerto int) {
	if r.Header.Get("Upgrade") != "" {
		fail(w, http.StatusNotImplemented, errors.New("WebSocket needs the sandbox's host to be local to the gateway: this host is remote (ssh://), where only buffered HTTP works"))
		return
	}
	cuerpo, err := io.ReadAll(io.LimitReader(r.Body, maxCuerpoGuest+1))
	if err != nil || len(cuerpo) > maxCuerpoGuest {
		fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("the request body is over %d bytes (remote hosts buffer it)", maxCuerpoGuest))
		return
	}
	if !utf8.Valid(cuerpo) {
		fail(w, http.StatusUnsupportedMediaType, errors.New("remote hosts carry text bodies only (the guest proxy is JSON)"))
		return
	}
	cab := map[string]string{}
	for k, vs := range r.Header {
		if len(vs) > 0 && !saltoAGuest(k) {
			cab[k] = strings.Join(vs, ", ")
		}
	}
	cab["X-Forwarded-Host"] = r.Host
	res, err := e.h.Cliente.Guest(r.Context(), e.mc.ID, api.GuestRequest{
		Port: puerto, Path: r.URL.RequestURI(), Method: r.Method, Body: string(cuerpo), Headers: cab,
		ResponseHeaders: []string{"Content-Type", "Location", "Set-Cookie", "Cache-Control", "ETag", "Content-Disposition"},
		WaitMS:          5000, MaxBodyBytes: api.GuestMaxBody,
	})
	if err != nil {
		g.olvidar(idDe(e.h, e.mc))
		g.s.log.Printf("ingress guest %s:%d: %v", e.mc.ID, puerto, err)
		msg := fmt.Sprintf("nothing is answering on port %d of the sandbox (remote hosts only reach ports listed in its %s label)", puerto, api.LabelPorts)
		fail(w, http.StatusBadGateway, &errPublico{msg: msg, err: err})
		return
	}
	for k, v := range res.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(res.Status)
	_, _ = w.Write([]byte(res.Body))
}

func saltoAGuest(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade", "proxy-authorization", "content-length", "host":
		return true
	}
	return false
}
