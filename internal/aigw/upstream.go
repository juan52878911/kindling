package aigw

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
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
)

// upstreamReplicas es el estado de un backend upstream: transporte propio
// (puede ser https), semáforo y circuito.
type upstreamReplicas struct {
	b     *Backend
	base  string // esquema://host[:puerto]/prefijo, sin barra final
	host  string // host:puerto, para la sonda TCP
	tr    *http.Transport
	sem   chan struct{}
	ahora func() time.Time

	mu        sync.Mutex
	cola      int
	maxCola   int
	fallos    int
	abiertoEn bool
	sondaEn   time.Time // última sonda lanzada
	sondeando bool
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
	tr := &http.Transport{
		Proxy:                  nil, // el upstream lo da el operador; nada de HTTP_PROXY del entorno
		DialContext:            (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
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
		tr: tr, sem: make(chan struct{}, n), maxCola: colaPorHueco * n, ahora: time.Now,
	}, nil
}

// mismo dice si u sigue sirviendo a b tras una recarga: lo que lee el estado
// (URL, semáforo, sonda y la credencial de la sonda) ha de ser igual; si no,
// hace falta uno nuevo. u.b no se cambia nunca: lo leen las sondas sin cerrojo.
func (u *upstreamReplicas) mismo(b *Backend) bool {
	a1, a2 := u.b.Auth, b.Auth
	return u.b.Upstream == b.Upstream && u.b.MaxInflight == b.MaxInflight && u.b.Health == b.Health &&
		(a1 == a2 || (a1 != nil && a2 != nil && *a1 == *a2))
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
	if u.abiertoEn && (every <= 0 || every > sondaAbierto) {
		every = sondaAbierto
	}
	if every <= 0 || u.sondeando || u.ahora().Sub(u.sondaEn) < every {
		return
	}
	u.sondeando, u.sondaEn = true, u.ahora()
	go func() {
		ok := u.sondear(context.Background())
		u.mu.Lock()
		u.sondeando = false
		u.mu.Unlock()
		u.resultado(ok)
	}()
}

// sondear hace una sonda: GET health.path esperando health.expect (200 por
// defecto), o un dial TCP si no hay ruta.
func (u *upstreamReplicas) sondear(ctx context.Context) bool {
	plazo := u.b.Health.Timeout
	if plazo <= 0 {
		plazo = plazoSonda
	}
	ctx, cancel := context.WithTimeout(ctx, plazo)
	defer cancel()
	if u.b.Health.Path == "" {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", u.host)
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

// resultado apunta un éxito (cierra el circuito) o un fallo (tres seguidos lo
// abren).
func (u *upstreamReplicas) resultado(ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if ok {
		u.fallos, u.abiertoEn = 0, false
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
			u.resultado(true)
			return resp, soltar, nil
		}
		if staleConn(ctx, reused, err) && !repetida && s.reintentable() {
			repetida = true
			continue
		}
		soltar()
		if ctx.Err() == nil && !s.excedido() {
			u.resultado(false)
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
	return u
}
