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
//     muera este proceso) y la conexión colgada se cierra, para que quien
//     esperaba reintente ya en vez de agotar su plazo.
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
}

// plazoMudez es cuánto puede estar una conexión con algo escrito y nada leído
// antes de comprobar que el vsock sigue entregando. Una petición lenta de
// verdad (un exec largo) no se toca: la prueba la contesta el agente y no
// pasa nada.
var plazoMudez = time.Second

// intentoPrueba acota cada GET /healthz de prueba. El agente contesta en ~1
// ms; dos intentos caben en el plazo del dial (vnet.intentoGuestDial).
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
	}
	c, err := v.dial(ctx)
	if err != nil {
		return nil, err
	}
	return &connVigilada{Conn: c, v: v}, nil
}

// probar hace la prueba con dos intentos. Devuelve vnet.ErrCaminoMudo si el
// vsock conecta y no contesta, ctx.Err() si venció el plazo de quien llama, o
// el error del dial (un agente anterior, que ni conecta por vsock).
func (v *vigia) probar(ctx context.Context) error {
	var err error
	for i := 0; i < 2; i++ {
		pctx, cancel := context.WithTimeout(ctx, intentoPrueba)
		err = agenteContesta(pctx, v.dial)
		cancel()
		if err == nil {
			return nil
		}
		if !esSinRespuesta(err) || ctx.Err() != nil {
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
// marca mudo y cierra c.
func (v *vigia) sospechar(c *connVigilada) {
	if v.mudo.Load() || !v.probando.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer v.probando.Store(false)
		// El aviso lo da vnet al ver el primer ErrCaminoMudo.
		if err := v.probar(context.Background()); errors.Is(err, vnet.ErrCaminoMudo) {
			v.mudo.Store(true)
			c.Close()
		}
	}()
}

// connVigilada es una conexión con el agente que avisa a su vigía si escribe
// y no lee nada en plazoMudez.
type connVigilada struct {
	net.Conn
	v      *vigia
	leido  atomic.Bool
	armado sync.Once
	mu     sync.Mutex
	t      *time.Timer
}

func (c *connVigilada) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 && !c.leido.Load() {
		c.armado.Do(func() {
			c.mu.Lock()
			c.t = time.AfterFunc(plazoMudez, func() {
				if !c.leido.Load() {
					c.v.sospechar(c)
				}
			})
			c.mu.Unlock()
		})
	}
	return n, err
}

func (c *connVigilada) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.leido.Store(true)
	}
	return n, err
}

func (c *connVigilada) Close() error {
	c.mu.Lock()
	if c.t != nil {
		c.t.Stop()
	}
	c.mu.Unlock()
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
		return err
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err != nil {
		return errSinRespuesta{err}
	}
	return nil
}
