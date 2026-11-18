package daemon

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/juan52878911/kindling/pkg/api"
)

// cupo reparte un presupuesto (bytes, o peticiones a la vez) con un tope
// global y otro por inquilino. Nunca espera: lo que no cabe no se toma, y
// quien llama contesta 429. Esperar dejaría a cada petición en cola con lo
// que ya hubiera tomado (su conexión, su parte del cuerpo leída), y con
// varias así se puede llegar a no avanzar ninguna.
//
// La clave "" es quien no es inquilino (admin, o un daemon sin política): solo
// le aplica el tope global.
type cupo struct {
	mu    sync.Mutex
	total int64
	por   map[string]int64
}

// tomar aparta n para clave si cabe bajo maxTotal (0 = sin tope global) y,
// si clave no es "", bajo maxPor (0 = sin tope por inquilino).
func (c *cupo) tomar(clave string, n, maxTotal, maxPor int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if maxTotal > 0 && c.total+n > maxTotal {
		return false
	}
	if clave != "" && maxPor > 0 && c.por[clave]+n > maxPor {
		return false
	}
	c.total += n
	if clave != "" {
		if c.por == nil {
			c.por = map[string]int64{}
		}
		c.por[clave] += n
	}
	return true
}

// soltar devuelve n de clave.
func (c *cupo) soltar(clave string, n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total -= n
	if clave != "" {
		if c.por[clave] -= n; c.por[clave] <= 0 {
			delete(c.por, clave)
		}
	}
}

// enUso es lo apartado en total y por clave (para los tests).
func (c *cupo) enUso(clave string) (total, deClave int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total, c.por[clave]
}

// ── /machines/{ref}/guest ────────────────────────────────────────────────────

// Topes de la memoria de /guest en vuelo (R2). Una petición puede traer hasta
// guestRequestMaxBody y su respuesta hasta GuestMaxBodyCap, y el daemon tiene
// las dos enteras en memoria. Sin tope, unas pocas en paralelo llevaban al
// daemon al OOM, y con él a todas las máquinas (8 de 100 MiB a la vez: de 1,6 a
// 3,1 GiB de heap; con el tope, 0,7 GiB y cinco de ellas con 429). Se cuentan los bytes según se leen, no lo que se declara: una
// llamada JSON-RPC normal aparta unos KiB, y solo las grandes compiten por el
// presupuesto. El tope por inquilino deja sitio a los demás, y en él cabe una
// llamada del tamaño máximo en cada sentido. Variables para los tests.
var (
	guestVueloMax       int64 = 1 << 30
	guestVueloInquilino int64 = factorMemoriaGuest * (guestRequestMaxBody + api.GuestMaxBodyCap)
)

// Carril de las llamadas pequeñas (R2 de la ronda 5). La reserva de una
// llamada dura lo que tarde el invitado en contestar, y eso solo tiene plazo
// de inactividad: dos inquilinos con una llamada grande cada uno ante un
// invitado lento llenaban guestVueloMax, y desde ahí todo /guest era un 429,
// también las llamadas JSON-RPC de pocos KiB y las del admin. Ahora lo que
// aparta una llamada mientras no pase de guestPequena sale de un presupuesto
// aparte (guestCarrilMax, con guestCarrilInquilino por inquilino), que las
// grandes no tocan; la que crece más se pasa entera al general. Variables
// para los tests.
var (
	guestPequena         int64 = factorMemoriaGuest << 20
	guestCarrilMax       int64 = 64 << 20
	guestCarrilInquilino int64 = 16 << 20
)

// factorMemoriaGuest es cuánta memoria cuesta cada byte leído: el búfer del
// decodificador (o de io.ReadAll), que crece doblando, y la cadena que sale
// de él. Medido con 8 peticiones de 100 MiB, el pico de heap era unas tres
// veces lo leído.
const factorMemoriaGuest = 3

// errGuestSinSitio: el presupuesto de /guest está lleno.
var errGuestSinSitio = errors.New("the daemon has too many large guest calls in flight")

// reservaGuest es lo que una llamada a /guest lleva apartado del cupo.
type reservaGuest struct {
	// c es el presupuesto general y peq el carril de las pequeñas (nil = sin
	// carril). Lo apartado, n, está entero en uno de los dos: en c si
	// enGeneral.
	c, peq    *cupo
	clave     string
	n         int64
	enGeneral bool
}

// reservaGuest abre la reserva de una llamada de clave (inquilinoDe).
func (s *Server) reservaGuest(clave string) *reservaGuest {
	return &reservaGuest{c: &s.vueloGuest, peq: &s.vueloGuestPeq, clave: clave}
}

// donde es el cupo en el que está lo apartado.
func (r *reservaGuest) donde() *cupo {
	if r.enGeneral || r.peq == nil {
		return r.c
	}
	return r.peq
}

// tomar aparta n bytes más, o devuelve errGuestSinSitio.
func (r *reservaGuest) tomar(n int64) error {
	if r == nil || n <= 0 {
		return nil
	}
	if !r.enGeneral && r.peq != nil {
		if r.n+n <= guestPequena && r.peq.tomar(r.clave, n, guestCarrilMax, guestCarrilInquilino) {
			r.n += n
			return nil
		}
		// No es pequeña (o el carril está lleno): todo al general, de una vez.
		if !r.c.tomar(r.clave, r.n+n, guestVueloMax, guestVueloInquilino) {
			return r.sinSitio()
		}
		if r.n > 0 {
			r.peq.soltar(r.clave, r.n)
		}
		r.enGeneral = true
		r.n += n
		return nil
	}
	if !r.c.tomar(r.clave, n, guestVueloMax, guestVueloInquilino) {
		return r.sinSitio()
	}
	r.enGeneral = true
	r.n += n
	return nil
}

// sinSitio es el error de una reserva que no cabe.
func (r *reservaGuest) sinSitio() error {
	if r.clave != "" {
		return fmt.Errorf("%w (this tenant's share is %d MiB of memory; retry when the others finish)", errGuestSinSitio, guestVueloInquilino>>20)
	}
	return fmt.Errorf("%w (%d MiB of memory; retry when the others finish)", errGuestSinSitio, guestVueloMax>>20)
}

// peticionLeida rebaja lo apartado por la petición, ya decodificada, a lo
// que de verdad sigue en memoria: la cadena del cuerpo (un byte por byte
// leído). El búfer del decodificador, el resto de factorMemoriaGuest, ya es
// basura, y retenerlo mientras el invitado contesta dejaba sin sitio a otros.
func (r *reservaGuest) peticionLeida() {
	if r == nil {
		return
	}
	r.devolver(r.n - r.n/factorMemoriaGuest)
}

// devolver suelta n de lo apartado (lo que un Read apartó y no llegó).
func (r *reservaGuest) devolver(n int64) {
	if n <= 0 {
		return
	}
	r.donde().soltar(r.clave, n)
	r.n -= n
}

// soltar lo devuelve todo.
func (r *reservaGuest) soltar() {
	if r == nil || r.n == 0 {
		return
	}
	r.donde().soltar(r.clave, r.n)
	r.n = 0
}

// lector aparta de la reserva cada byte que lee de rd. declarado (> 0) es
// lo que el emisor dijo que mandaría (Content-Length): se aparta entero de
// golpe, y si no cabe, error sin leer nada. Así, con varias grandes a la vez,
// entran las que caben enteras y las demás se rechazan al instante; contando
// solo según se lee, todas avanzaban a medias hasta agotar el presupuesto y
// no acababa ninguna. Sin declarar (chunked), se aparta según se lee.
func (r *reservaGuest) lector(rd io.Reader, declarado int64) (io.Reader, error) {
	if r == nil {
		return rd, nil
	}
	l := &lectorConReserva{rd: rd, res: r}
	if declarado > 0 {
		if err := r.tomar(factorMemoriaGuest * declarado); err != nil {
			return nil, err
		}
		l.pagado, l.declarado = declarado, true
	}
	return l, nil
}

type lectorConReserva struct {
	rd  io.Reader
	res *reservaGuest
	// pagado son los bytes ya apartados que aún no se han leído; declarado,
	// que se apartó lo declarado al empezar.
	pagado    int64
	declarado bool
}

// trozoReserva es lo más que se lee de una vez sin haberlo apartado antes:
// se aparta ANTES de leer y se devuelve lo que no llegó. Apartar después no
// servía: el último Read ya había metido sus bytes en memoria, y el
// decodificador JSON da por buena la petición si con ellos el valor queda
// completo, aunque el Read traiga error.
const trozoReserva = 64 << 10

func (l *lectorConReserva) Read(p []byte) (int, error) {
	if l.pagado > 0 {
		if int64(len(p)) > l.pagado {
			p = p[:l.pagado]
		}
		n, err := l.rd.Read(p)
		l.pagado -= int64(n)
		return n, err
	}
	if l.declarado && len(p) > 0 {
		// Leído lo declarado, lo que queda es el EOF: apartar otro trozo para
		// él podía tumbar, con el presupuesto casi lleno, un cuerpo que ya
		// había llegado entero. Se lee de byte en byte y se cuenta después
		// (un byte de más como mucho).
		n, err := l.rd.Read(p[:1])
		if n > 0 {
			if e := l.res.tomar(factorMemoriaGuest * int64(n)); e != nil {
				return n, e
			}
		}
		return n, err
	}
	if len(p) > trozoReserva {
		p = p[:trozoReserva]
	}
	if len(p) == 0 {
		return l.rd.Read(p)
	}
	if err := l.res.tomar(factorMemoriaGuest * int64(len(p))); err != nil {
		return 0, err
	}
	n, err := l.rd.Read(p)
	l.res.devolver(factorMemoriaGuest * int64(len(p)-n))
	return n, err
}

// lectorCerrable es lector para un cuerpo de petición (http.Request.Body).
func (r *reservaGuest) lectorCerrable(rc io.ReadCloser, declarado int64) (io.ReadCloser, error) {
	rd, err := r.lector(rc, declarado)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{rd, rc}, nil
}

// failSinSitio contesta a una llamada que no cupo: 429 y que reintente.
func failSinSitio(w http.ResponseWriter, err error) {
	w.Header().Set("Retry-After", guestBusyRetryAfter)
	fail(w, http.StatusTooManyRequests, err)
}

// ── exec, shell y ficheros ───────────────────────────────────────────────────

// maxSesionesInquilino es cuántos exec, shell y llamadas a ficheros puede
// tener abiertos a la vez un inquilino (R8). Cada uno es una goroutine y dos
// conexiones en el daemon (con el cliente y con el agente), y el agente solo
// acota los suyos por máquina: con muchas máquinas, un inquilino no tenía
// techo. Los admin no lo tienen. Variable para los tests.
var maxSesionesInquilino int64 = 64

// tomarSesionGuest aparta una de las sesiones del inquilino de r; si no
// quedan, contesta 429 y devuelve false. soltar la devuelve.
func (s *Server) tomarSesionGuest(w http.ResponseWriter, r *http.Request) (soltar func(), ok bool) {
	t, filtra := inquilinoDe(r)
	if !filtra {
		return func() {}, true
	}
	if !s.sesionesGuest.tomar(t, 1, 0, maxSesionesInquilino) {
		w.Header().Set("Retry-After", guestBusyRetryAfter)
		fail(w, http.StatusTooManyRequests, fmt.Errorf("this tenant has %d exec, shell or file calls open (the limit); retry when one ends", maxSesionesInquilino))
		return nil, false
	}
	return func() { s.sesionesGuest.soltar(t, 1) }, true
}
