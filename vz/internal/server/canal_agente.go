package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/vz/internal/vnet"
)

// El canal con el agente por vsock, vigilado.
//
// Tras algunas restauraciones de kling-vz (del orden de 1 de cada 50-100
// copias de una plantilla de compose con carpetas vivas, en un M4 con macOS
// 26), el vsock del framework deja de entregar al invitado: el connect del
// host sigue saliendo bien, pero el invitado no recibe nada más —sus
// interrupciones de vsock no se mueven y su ISR está a 0— mientras la red de
// usuario y el agente por TCP siguen bien. A veces pasa desde el principio y
// a veces tras contestar la primera petición. El núcleo se quedaba esperando
// su /resync, su /volume/acquire o el attach de la carpeta hasta el plazo de
// cada uno: la copia de postgres de compose no montaba su pgdata y el up se
// deshacía.
//
// Dos defensas, las dos con un GET /healthz por una conexión aparte, que es
// lo que distingue "el agente tarda" de "el vsock no entrega":
//
//   - Hasta la primera respuesta del agente en este proceso, cada conexión va
//     precedida de esa prueba (dos intentos: justo tras restaurar, el
//     invitado aún puede estar por atender el TRANSPORT_RESET, que corta en
//     silencio lo que ya hubiera conectado).
//   - Después, una conexión que escribió y lleva plazoMudez sin leer nada
//     lanza la prueba; si no contesta, el camino queda mudo (cada dial
//     siguiente da vnet.ErrCaminoMudo y el reenvío sigue por TCP hasta que
//     muera este proceso) y todas las conexiones abiertas se cierran, para
//     que quien esperaba reintente ya en vez de agotar su plazo.
//
// Cuesta un viaje por vsock (~1 ms) en la primera conexión de cada proceso, y
// nada en las demás salvo un temporizador por conexión.
func dialAgenteComprobado(dial func(ctx context.Context) (net.Conn, error)) func(ctx context.Context) (net.Conn, error) {
	v := &vigia{dial: dial}
	return v.dialar
}

type vigia struct {
	dial  func(ctx context.Context) (net.Conn, error)
	visto atomic.Bool // el agente contestó por vsock alguna vez
	mudo  atomic.Bool // y después dejó de hacerlo: todo por TCP
	// probando: una prueba de mudez en marcha (no se lanzan dos a la vez).
	probando atomic.Bool
	// vivas: las conexiones abiertas, para cerrarlas todas al enmudecer. mu
	// las guarda y ordena su alta con el paso a mudo.
	mu    sync.Mutex
	vivas map[*connVigilada]struct{}
}

// plazoMudez es cuánto puede estar una conexión con algo escrito y nada leído
// antes de comprobar que el vsock sigue entregando. Una petición lenta de
// verdad (un exec largo) no se toca: la prueba la contesta el agente y no
// pasa nada.
var plazoMudez = time.Second

// intentoPrueba acota cada GET /healthz de prueba. El agente contesta en ~1
// ms; dos intentos caben en el plazo del dial (vnet.intentoGuestDial).
// Es el único plazo de la prueba (ver probar).
const intentoPrueba = 200 * time.Millisecond

func (v *vigia) dialar(ctx context.Context) (net.Conn, error) {
	if v.mudo.Load() {
		return nil, vnet.ErrCaminoMudo
	}
	if !v.visto.Load() {
		if err := v.probar(ctx); err != nil {
			return nil, err
		}
		v.visto.Store(true)
		// La prueba lleva su propio plazo: el agente contestó, pero quizá
		// cuando quien llamaba ya no esperaba.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	c, err := v.dial(ctx)
	if err != nil {
		return nil, err
	}
	cv := &connVigilada{Conn: c, v: v}
	v.mu.Lock()
	if v.mudo.Load() {
		v.mu.Unlock()
		c.Close()
		return nil, vnet.ErrCaminoMudo
	}
	if v.vivas == nil {
		v.vivas = map[*connVigilada]struct{}{}
	}
	v.vivas[cv] = struct{}{}
	v.mu.Unlock()
	return cv, nil
}

// probar hace la prueba con dos intentos, con su propio plazo (intentoPrueba
// cada uno) y no con el de quien llama: un plazo corto de fuera (los 150 ms de
// un sondeo de puerto) contra un invitado vivo pero lento (recién reanudado,
// o parado por el freno de CPU) no es mudez, y dejaría la máquina por TCP de
// por vida. Devuelve vnet.ErrCaminoMudo si el vsock conecta y no contesta en
// ese plazo propio, ctx.Err() si venció el de quien llama sin veredicto de
// mudez, o el error del dial (un agente anterior, que ni conecta por vsock).
func (v *vigia) probar(ctx context.Context) error {
	pbase := context.WithoutCancel(ctx)
	var err error
	for i := 0; i < 2; i++ {
		pctx, cancel := context.WithTimeout(pbase, intentoPrueba)
		err = agenteContesta(pctx, v.dial)
		cancel()
		if err == nil {
			return nil
		}
		if !esSinRespuesta(err) {
			break
		}
	}
	if esSinRespuesta(err) {
		return fmt.Errorf("%w: %v", vnet.ErrCaminoMudo, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// sospechar comprueba, en segundo plano, si el camino sigue vivo; si no, lo
// marca mudo y cierra todas las conexiones vivas (no solo c: las que
// esperaban a la vez su /resync o su attach colgarían hasta su plazo). Si ya
// estaba mudo, cierra c en el acto; si hay otra prueba en marcha, su
// veredicto cierra también c, que está entre las vivas.
func (v *vigia) sospechar(c *connVigilada) {
	if v.mudo.Load() {
		c.Close()
		return
	}
	if !v.probando.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer v.probando.Store(false)
		// El aviso lo da vnet al ver el primer ErrCaminoMudo.
		if err := v.probar(context.Background()); errors.Is(err, vnet.ErrCaminoMudo) {
			v.enmudecer()
		}
	}()
}

// enmudecer marca el camino mudo y cierra las conexiones vivas.
func (v *vigia) enmudecer() {
	v.mu.Lock()
	v.mudo.Store(true)
	vivas := v.vivas
	v.vivas = nil
	v.mu.Unlock()
	for c := range vivas {
		c.Close()
	}
}

func (v *vigia) olvidar(c *connVigilada) {
	v.mu.Lock()
	delete(v.vivas, c)
	v.mu.Unlock()
}

// connVigilada es una conexión con el agente que avisa a su vigía si escribe
// y no lee nada en plazoMudez. La vigilancia se rearma con cada escritura que
// llega tras una lectura: un keep-alive reutilizado sobre un vsock que
// enmudece después también se detecta.
type connVigilada struct {
	net.Conn
	v *vigia
	// Con mu: armado, hay una escritura sin lectura detrás y t corre;
	// cerrada, ya pasó Close.
	mu      sync.Mutex
	armado  bool
	cerrada bool
	t       *time.Timer
}

func (c *connVigilada) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.mu.Lock()
		if !c.armado && !c.cerrada {
			c.armado = true
			if c.t == nil {
				c.t = time.AfterFunc(plazoMudez, c.vence)
			} else {
				c.t.Reset(plazoMudez)
			}
		}
		c.mu.Unlock()
	}
	return n, err
}

// vence: plazoMudez desde la primera escritura sin lectura detrás.
func (c *connVigilada) vence() {
	c.mu.Lock()
	sospecha := c.armado && !c.cerrada
	c.armado = false
	c.mu.Unlock()
	if sospecha {
		c.v.sospechar(c)
	}
}

func (c *connVigilada) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.mu.Lock()
		if c.armado {
			c.armado = false
			c.t.Stop()
		}
		c.mu.Unlock()
	}
	return n, err
}

func (c *connVigilada) Close() error {
	c.mu.Lock()
	c.cerrada = true
	if c.t != nil {
		c.t.Stop()
	}
	c.mu.Unlock()
	c.v.olvidar(c)
	return c.Conn.Close()
}

// CloseWrite deja pasar el medio cierre si la conexión de abajo lo tiene (el
// reenvío lo usa al acabar de copiar en un sentido).
func (c *connVigilada) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// errSinRespuesta: la conexión de prueba no recibió nada.
type errSinRespuesta struct{ err error }

func (e errSinRespuesta) Error() string {
	return "no answer to GET /healthz over vsock: " + e.err.Error()
}

func esSinRespuesta(err error) bool {
	_, ok := err.(errSinRespuesta)
	return ok
}

// agenteContesta abre otra conexión, pide /healthz y espera el primer byte.
func agenteContesta(ctx context.Context, dial func(ctx context.Context) (net.Conn, error)) error {
	c, err := dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	} else {
		_ = c.SetDeadline(time.Now().Add(time.Second))
	}
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: agent\r\nConnection: close\r\n\r\n", api.GuestHealthPath); err != nil {
		// Conectó pero ni acepta la petición antes del plazo: tan mudo como
		// el que no contesta. El plazo es siempre el propio de la prueba
		// (probar no le pasa el de quien llama), así que vencer aquí sí es
		// mudez.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return errSinRespuesta{err}
		}
		return err
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err != nil {
		return errSinRespuesta{err}
	}
	return nil
}
