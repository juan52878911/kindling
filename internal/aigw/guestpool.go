package aigw

import (
	"context"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// CONEXIONES A LAS RÉPLICAS.
//
// Antes cada petición a una réplica abría una conexión TCP nueva
// (DisableKeepAlives). En latencia no se nota —abrirla hacia el host local
// cuesta ≤0,2 ms (docs/despertar.md)—, pero bajo carga sí: un conjunto de CI
// entero son decenas de miles de clasificaciones, cada una dejaba un socket en
// TIME_WAIT durante ~30-60 s, y en macOS (reenvíos por 127.0.0.1, 16 384
// puertos efímeros) eso agotaba los puertos del host: "can't assign requested
// address" y 503. Por eso cada dirección de réplica tiene ahora su transporte
// con keep-alive y un tope de conexiones.
//
// Lo delicado es congelar y despertar. Al congelar o pausar, el invitado se
// queda con el estado TCP de ese instante y al despertar lo recupera del
// volcado; el host, en cambio, sigue su vida: puede haber cerrado su extremo,
// el reenvío de macOS puede haberse rehecho, o (en Linux) la IP puede ser ya
// de OTRA máquina restaurada del mismo dorado, que no conoce esa conexión y
// contesta RST. Una conexión ociosa que cruza un congelar/despertar no es de
// fiar. Por eso:
//
//   - al dormir una réplica (el segador la congela o la pausa, o se la
//     desaloja) el planificador avisa (Scheduler.OnSleep) y se cierran sus
//     conexiones ociosas y se olvida su transporte; igual al retirarla (Drop);
//   - la primera petición tras un despertar (Replica.Wake != nil) cierra las
//     que quedaran, por si el aviso no llegó (la congeló otro proceso);
//   - IdleConnTimeout es menor que el plazo de inactividad del segador, así
//     que una conexión ociosa se cierra sola antes de que su réplica se duerma;
//   - y si aun así una conexión REUTILIZADA falla sin respuesta, se repite una
//     vez por una conexión nueva (postGuest): clasificar, embeber o generar no
//     tienen efectos, repetir no duplica nada.

// guestConnsPerReplica acota las conexiones abiertas a una misma réplica, y es
// también cuántas ociosas se guardan. Una réplica atiende pocas peticiones a la
// vez (MaxInflight), pero pasado el tope de réplicas el planificador reparte el
// exceso entre las que hay, así que se deja holgura. Con el tope, una ráfaga
// espera a que se libere una conexión en vez de abrir miles.
const guestConnsPerReplica = 64

// guestPool guarda un transporte por dirección de réplica: uno por dirección y
// no uno compartido para poder cerrar las conexiones de UNA réplica cuando se
// duerme sin tocar las de las demás.
type guestPool struct {
	idleTimeout time.Duration
	mu          sync.Mutex
	byAddr      map[string]*http.Transport
}

// newGuestPool crea el pool. idle es el plazo de inactividad del segador: las
// conexiones ociosas se cierran a la mitad, antes de que la réplica se duerma
// con ellas abiertas.
func newGuestPool(idle time.Duration) *guestPool {
	t := idle / 2
	if t <= 0 || t > 90*time.Second {
		t = 90 * time.Second
	}
	return &guestPool{idleTimeout: t, byAddr: map[string]*http.Transport{}}
}

// transport devuelve el transporte de addr, creándolo si no existe. El
// invitado no es de fiar: conectar tiene plazo corto y las cabeceras tope; el
// cuerpo lo acota quien lee.
func (p *guestPool) transport(addr string) *http.Transport {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.byAddr[addr]; t != nil {
		return t
	}
	t := &http.Transport{
		DialContext:            (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		MaxResponseHeaderBytes: 64 << 10,
		DisableCompression:     true,
		MaxConnsPerHost:        guestConnsPerReplica,
		MaxIdleConns:           guestConnsPerReplica,
		MaxIdleConnsPerHost:    guestConnsPerReplica,
		IdleConnTimeout:        p.idleTimeout,
	}
	p.byAddr[addr] = t
	return t
}

// closeIdle cierra las conexiones ociosas hacia addr (las que están en uso
// siguen: CloseIdleConnections no las toca).
func (p *guestPool) closeIdle(addr string) {
	p.mu.Lock()
	t := p.byAddr[addr]
	p.mu.Unlock()
	if t != nil {
		t.CloseIdleConnections()
	}
}

// forget cierra las ociosas de addr y olvida su transporte: la réplica se
// durmió o murió, y la dirección puede acabar siendo de otra máquina (en
// macOS, otro puerto de reenvío; en Linux, la misma IP para otra réplica).
func (p *guestPool) forget(addr string) {
	p.mu.Lock()
	t := p.byAddr[addr]
	delete(p.byAddr, addr)
	p.mu.Unlock()
	if t != nil {
		t.CloseIdleConnections()
	}
}

// size es cuántos transportes hay.
func (p *guestPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byAddr)
}

// do manda req por el transporte de addr y dice si la conexión era
// reutilizada: solo esas pueden estar muertas por un congelar/despertar.
func (p *guestPool) do(addr string, req *http.Request) (*http.Response, bool, error) {
	var reused bool
	trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := p.transport(addr).RoundTrip(req)
	return resp, reused, err
}

// staleConn dice si un fallo merece repetirse por una conexión nueva: la
// conexión era reutilizada y quien pide no ha cancelado. No se mira el tipo de
// error (EOF, reset, broken pipe... cambia con la plataforma y con el momento
// del corte): una conexión reutilizada que falla sin respuesta es justo el
// caso de la conexión muerta, y repetir una vez no cuesta nada.
func staleConn(ctx context.Context, reused bool, err error) bool {
	return err != nil && reused && ctx.Err() == nil
}
