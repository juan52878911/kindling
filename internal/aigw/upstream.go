package aigw

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// BACKENDS UPSTREAM.
//
// Un backend con "upstream" es una URL fija que kindling no arranca (un vLLM
// con GPU, otra instancia, una API). No hay réplicas que despertar: lo que el
// gateway pone es un semáforo de max_inflight con cola acotada (un upstream
// saturado no acumula peticiones sin fin en memoria) y un cortacircuitos: tres
// fallos seguidos (sondas o peticiones sin respuesta) lo abren, y desde ahí
// cada petición recibe 503 con Retry-After al instante, hasta que una sonda
// pase.
//
// Las sondas van a remolque del tráfico, no en una goroutine por backend: una
// petición que encuentra la última sonda más vieja que health.every lanza una
// en segundo plano (una a la vez) y sigue. Así un upstream sin tráfico no
// cuesta nada y no hay nada que parar al recargar o al cerrar; lo que se
// pierde es detectar la caída antes de la primera petición, que de todos
// modos fallaría ella sola.
//
// Ronda 9 (pendiente 5). Al crear el estado (al cargar, al recargar o en la
// primera petición tras un cambio del bloque) se lanza una SONDA DE CARGA: si
// el bloque tiene upstream_model, además de la salud lista GET /v1/models, y
// si el upstream no sirve ese modelo el circuito se abre ya, con un aviso en
// el log, hasta que una sonda lo encuentre (un vLLM con otro modelo rechaza
// cada petición). Un fallo de salud en la sonda de carga no cuenta para el
// circuito: solo se avisa. Con health.grace, mientras ninguna sonda haya
// pasado y dure la gracia, el upstream «está cargando»: cada petición recibe
// 503 con Retry-After al instante, sin reenviarse, y los fallos no abren el
// circuito.

// fallosParaAbrir son los fallos seguidos que abren el circuito.
const fallosParaAbrir = 3

// colaPorHueco es cuántas peticiones esperan por cada hueco de max_inflight.
const colaPorHueco = 4

// sondaAbierto es el periodo de sondeo con el circuito abierto cuando el
// bloque no dice health.every (o dice uno más largo): el mismo Retry-After que
// reciben los clientes. Variable para los tests.
var sondaAbierto = 2 * time.Second

// plazoSonda acota una sonda cuando health.timeout no dice otro.
const plazoSonda = 5 * time.Second

var (
	errCircuito = errors.New("upstream circuit open")
	errSaturado = errors.New("upstream queue full")
	errCargando = errors.New("upstream still loading (health.grace)")
)

// maxModelosBytes acota la respuesta de GET /v1/models de la sonda.
const maxModelosBytes = 1 << 20

// resultadoSonda es lo que dice una sonda (o una petición).
type resultadoSonda int

const (
	sondaOK resultadoSonda = iota
	sondaFallo
	// sondaSinModelo: el upstream contesta pero no sirve upstream_model.
	sondaSinModelo
	// sondaSinVerificar: la salud pasa pero la lista de modelos no se pudo
	// leer; no cierra un circuito abierto por sondaSinModelo.
	sondaSinVerificar
)

// upstreamReplicas es el estado de un backend upstream: transporte propio
// (puede ser https), semáforo y circuito.
type upstreamReplicas struct {
	b     *Backend
	base  string // esquema://host[:puerto]/prefijo, sin barra final
	host  string // host:puerto, para la sonda TCP
	tr    *http.Transport
	dial  func(ctx context.Context, network, addr string) (net.Conn, error)
	sem   chan struct{}
	ahora func() time.Time

	mu        sync.Mutex
	cola      int
	maxCola   int
	fallos    int
	abiertoEn bool
	sondaEn   time.Time // última sonda lanzada
	sondeando bool
	creado    time.Time // desde cuándo cuenta health.grace
	listo     bool      // alguna sonda o petición pasó
	modeloOK  bool      // una sonda vio upstream_model en /v1/models
	sinModelo bool      // el circuito está abierto porque no lo sirve
}

// newUpstream prepara el estado de b. inflight es el max_inflight del gateway,
// para un bloque que no da el suyo; cabeceras, el tope de espera a las
// cabeceras de la respuesta.
func newUpstream(b *Backend, inflight int, cabeceras time.Duration) (*upstreamReplicas, error) {
	u, err := url.Parse(b.Upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("model %q: upstream is not an http(s) URL without credentials", b.Name)
	}
	n := b.MaxInflight
	if n <= 0 {
		n = inflight
	}
	if n <= 0 {
		n = 1
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	// Cada conexión se comprueba ya resuelta: un nombre que lleve a los
	// metadatos, a la red de kindling o al rango de reenvíos no se marca.
	dialer := &net.Dialer{Timeout: 3 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		return destinoVetado(address)
	}}
	if d, err := time.ParseDuration(b.Limits.Timeout); err == nil && d > cabeceras {
		cabeceras = d // limits.timeout puede alargar la espera a las cabeceras
	}
	var tlsCfg *tls.Config
	if b.CAFile != "" {
		pool, err := cargarCA(b.CAFile)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", b.Name, err)
		}
		tlsCfg = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	tr := &http.Transport{
		Proxy:                  nil, // el upstream lo da el operador; nada de HTTP_PROXY del entorno
		DialContext:            dialer.DialContext,
		TLSClientConfig:        tlsCfg,
		TLSHandshakeTimeout:    10 * time.Second,
		ForceAttemptHTTP2:      u.Scheme == "https",
		MaxResponseHeaderBytes: 64 << 10,
		DisableCompression:     true,
		MaxConnsPerHost:        n,
		MaxIdleConnsPerHost:    n,
		IdleConnTimeout:        90 * time.Second,
		ResponseHeaderTimeout:  cabeceras,
	}
	return &upstreamReplicas{
		b: b, base: u.Scheme + "://" + u.Host + strings.TrimSuffix(u.EscapedPath(), "/"), host: host,
		tr: tr, dial: dialer.DialContext, sem: make(chan struct{}, n), maxCola: colaPorHueco * n,
		ahora: time.Now, creado: time.Now(),
	}, nil
}

// necesitaModelo dice si la sonda tiene que ver upstream_model en /v1/models.
func (u *upstreamReplicas) necesitaModelo() bool {
	return u.b.UpstreamModel != "" && (u.b.Contract == ContractOpenAI || u.b.Contract == ContractOpenAIEmbeddings)
}

// cargando dice si el upstream está en su gracia de carga. Con u.mu tomado.
func (u *upstreamReplicas) cargando() bool {
	return u.b.Health.Grace > 0 && !u.listo && u.ahora().Before(u.creado.Add(u.b.Health.Grace))
}

// mismo dice si u sigue sirviendo a b tras una recarga: lo que lee el estado
// (URL, semáforo, sonda y la credencial de la sonda) ha de ser igual; si no,
// hace falta uno nuevo. u.b no se cambia nunca: lo leen las sondas sin cerrojo.
func (u *upstreamReplicas) mismo(b *Backend) bool {
	a1, a2 := u.b.Auth, b.Auth
	return u.b.Upstream == b.Upstream && u.b.MaxInflight == b.MaxInflight && u.b.Health == b.Health &&
		(a1 == a2 || (a1 != nil && a2 != nil && *a1 == *a2)) &&
		u.b.CAFile == b.CAFile && u.b.UpstreamModel == b.UpstreamModel && u.b.Contract == b.Contract &&
		u.b.Limits.Timeout == b.Limits.Timeout
}

// abierto dice si el circuito está abierto.
func (u *upstreamReplicas) abierto() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.abiertoEn
}

// enCola es cuántas peticiones esperan hueco.
func (u *upstreamReplicas) enCola() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cola
}

// adquirir reserva un hueco: con el circuito abierto o la cola llena falla al
// instante; si no, espera hueco mientras ctx viva. De paso lanza la sonda si
// toca.
func (u *upstreamReplicas) adquirir(ctx context.Context) (func(), error) {
	u.mu.Lock()
	u.quizaSondear()
	if u.abiertoEn {
		u.mu.Unlock()
		return nil, errCircuito
	}
	if u.cargando() {
		u.mu.Unlock()
		return nil, errCargando
	}
	select {
	case u.sem <- struct{}{}:
		u.mu.Unlock()
		return u.soltar, nil
	default:
	}
	if u.cola >= u.maxCola {
		u.mu.Unlock()
		return nil, errSaturado
	}
	u.cola++
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.cola--
		u.mu.Unlock()
	}()
	select {
	case u.sem <- struct{}{}:
		return u.soltar, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (u *upstreamReplicas) soltar() { <-u.sem }

// quizaSondear lanza una sonda en segundo plano si no hay otra en curso y la
// última es más vieja que el periodo. Con u.mu tomado.
func (u *upstreamReplicas) quizaSondear() {
	every := u.b.Health.Every
	if (u.abiertoEn || u.cargando()) && (every <= 0 || every > sondaAbierto) {
		every = sondaAbierto
	}
	if every <= 0 || u.sondeando || u.ahora().Sub(u.sondaEn) < every {
		return
	}
	u.lanzarSonda(false)
}

// sondearYa lanza la sonda de carga si no hay otra en curso.
func (u *upstreamReplicas) sondearYa() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.sondeando {
		u.lanzarSonda(true)
	}
}

// lanzarSonda sondea en segundo plano. Con u.mu tomado y sin otra en curso.
// Un fallo de salud de la sonda de carga solo se avisa.
func (u *upstreamReplicas) lanzarSonda(carga bool) {
	u.sondeando, u.sondaEn = true, u.ahora()
	go func() {
		r := u.sondear(context.Background())
		u.mu.Lock()
		u.sondeando = false
		u.mu.Unlock()
		if carga && r == sondaFallo {
			log.Printf("warning: upstream model %q: %s does not answer its health probe at load; requests will try it anyway", u.b.Name, u.host)
			return
		}
		u.resultado(r)
	}()
}

// sondear hace una sonda: la de salud y, si hace falta, mira que el upstream
// sirva upstream_model.
func (u *upstreamReplicas) sondear(ctx context.Context) resultadoSonda {
	plazo := u.b.Health.Timeout
	if plazo <= 0 {
		plazo = plazoSonda
	}
	ctx, cancel := context.WithTimeout(ctx, plazo)
	defer cancel()
	if !u.salud(ctx) {
		return sondaFallo
	}
	u.mu.Lock()
	verificar := u.necesitaModelo() && !u.modeloOK
	u.mu.Unlock()
	if !verificar {
		return sondaOK
	}
	return u.sirveModelo(ctx)
}

// sirveModelo lista GET /v1/models (formato OpenAI: {"data":[{"id":…}]}) y
// busca upstream_model. Una lista que no se puede leer no prueba que falte:
// sondaSinVerificar.
func (u *upstreamReplicas) sirveModelo(ctx context.Context) resultadoSonda {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.base+"/v1/models", nil)
	if err != nil {
		return sondaSinVerificar
	}
	if err := ponerAuth(u.b, req.Header); err != nil {
		return sondaSinVerificar
	}
	req.Header.Set("Accept", "application/json")
	resp, err := u.tr.RoundTrip(req)
	if err != nil {
		return sondaFallo
	}
	defer resp.Body.Close()
	var lista struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, maxModelosBytes)).Decode(&lista) != nil || lista.Data == nil {
		return sondaSinVerificar
	}
	for _, m := range lista.Data {
		if m.ID == u.b.UpstreamModel {
			u.mu.Lock()
			u.modeloOK = true
			u.mu.Unlock()
			return sondaOK
		}
	}
	return sondaSinModelo
}

// salud es la sonda de salud: GET health.path esperando health.expect (200
// por defecto), o un dial TCP si no hay ruta.
func (u *upstreamReplicas) salud(ctx context.Context) bool {
	if u.b.Health.Path == "" {
		c, err := u.dial(ctx, "tcp", u.host)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.base+u.b.Health.Path, nil)
	if err != nil {
		return false
	}
	if err := ponerAuth(u.b, req.Header); err != nil {
		return false
	}
	resp, err := u.tr.RoundTrip(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	want := u.b.Health.Expect
	if want == 0 {
		want = http.StatusOK
	}
	return resp.StatusCode == want
}

// resultado apunta lo que dijo una sonda o una petición: un éxito cierra el
// circuito (salvo el abierto por un modelo que falta, que solo cierra una
// sonda que lo vea); tres fallos seguidos lo abren, salvo durante la gracia
// de carga; un modelo que falta lo abre ya.
func (u *upstreamReplicas) resultado(r resultadoSonda) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch r {
	case sondaOK, sondaSinVerificar:
		u.fallos, u.listo = 0, true
		if r == sondaOK && u.modeloOK {
			u.sinModelo = false
		}
		if !u.sinModelo {
			u.abiertoEn = false
		}
		return
	case sondaSinModelo:
		u.listo = true
		if !u.sinModelo {
			log.Printf("warning: upstream model %q: %s does not serve upstream_model %q (not in GET /v1/models); circuit open until it does", u.b.Name, u.host, u.b.UpstreamModel)
			u.sondaEn = u.ahora()
		}
		u.sinModelo, u.abiertoEn = true, true
		return
	}
	if u.cargando() {
		return
	}
	u.fallos++
	if u.fallos >= fallosParaAbrir {
		if !u.abiertoEn {
			// La siguiente sonda espera su periodo desde ahora.
			u.sondaEn = u.ahora()
		}
		u.abiertoEn = true
	}
}

// enviar manda s al upstream. Una respuesta, sea cual sea su código, cuenta
// como éxito para el circuito (el upstream está vivo); no haber recibido
// respuesta, como fallo. Una conexión reutilizada que muere sin respuesta se
// repite una vez por otra si el cuerpo no llegó a salir.
func (u *upstreamReplicas) enviar(ctx context.Context, s *salida) (*http.Response, func(), error) {
	soltar, err := u.adquirir(ctx)
	if err != nil {
		return nil, nil, &wakeError{err}
	}
	var repetida bool
	for {
		req, err := s.request(ctx, u.base)
		if err != nil {
			soltar()
			return nil, nil, err
		}
		var reused bool
		req = req.WithContext(httptrace.WithClientTrace(req.Context(),
			&httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }}))
		resp, err := u.tr.RoundTrip(req)
		if err == nil {
			u.resultado(sondaOK)
			return resp, soltar, nil
		}
		if staleConn(ctx, reused, err) && !repetida && s.reintentable() {
			repetida = true
			continue
		}
		soltar()
		if ctx.Err() == nil && !s.excedido() {
			u.resultado(sondaFallo)
		}
		return nil, nil, &wakeError{err}
	}
}

// upstreamDe devuelve el estado del upstream de b, creándolo la primera vez
// (o de nuevo si una recarga cambió lo que le afecta).
func (g *Gateway) upstreamDe(b *Backend) *upstreamReplicas {
	p := g.guests
	p.upsMu.Lock()
	defer p.upsMu.Unlock()
	if u := p.ups[b.Name]; u != nil && u.mismo(b) {
		return u
	}
	u, err := newUpstream(b, g.opts.MaxInflight, g.opts.ProxyTimeout)
	if err != nil {
		return nil
	}
	if old := p.ups[b.Name]; old != nil {
		old.tr.CloseIdleConnections()
	}
	p.ups[b.Name] = u
	u.sondearYa() // la sonda de carga
	return u
}

// prepararUpstreams crea el estado de cada upstream del registro efectivo, y
// con él su sonda de carga: así un upstream caído o sin su modelo se sabe al
// arrancar o recargar, no en la primera petición.
func (g *Gateway) prepararUpstreams() {
	for _, b := range g.catalogo().lista {
		if b.Upstream != "" {
			g.upstreamDe(b)
		}
	}
}
