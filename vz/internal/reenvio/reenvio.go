// Package reenvio pasa las conexiones del invitado a la pasarela al proxy de
// credenciales aparte de su máquina (kling-credproxy, ver
// docs/proxy-macos-separado.md del núcleo). Las conexiones son de gVisor, no
// del kernel: no hay descriptor que entregar, así que por cada una se abre
// otra al socket de reenvío del proxy, se escribe la cabecera fija
// (credproxy.SplitHeader: versión y puerto) y se copian bytes en los dos
// sentidos. Este proceso no ve claves: solo bytes con marcadores.
package reenvio

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// plazoConexion es cuánto se espera a que el proxy acepte.
const plazoConexion = 5 * time.Second

// Reenvio es el reenvío de una máquina. Lo enciende el daemon (Fijar, por PUT
// /kling/credproxy) cuando le ha dado las claves al proxy aparte.
type Reenvio struct {
	ruta       string
	activo, db atomic.Bool
	logf       func(string, ...any)
	// avisado evita llenar el log con un fallo por conexión.
	avisado atomic.Int64
	// Dial sustituye a la conexión al socket (tests).
	Dial func(ctx context.Context) (net.Conn, error)
}

// New crea el reenvío al socket ruta (absoluta, o relativa al directorio de
// trabajo si la absoluta no cabe en sun_path: ver kling-vz).
func New(ruta string, logf func(string, ...any)) *Reenvio {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Reenvio{ruta: ruta, logf: logf}
}

// Fijar enciende o apaga el reenvío.
func (r *Reenvio) Fijar(activo, db bool) {
	r.activo.Store(activo)
	r.db.Store(activo && db)
}

// Activo dice si el 80 de la pasarela va al proxy aparte.
func (r *Reenvio) Activo() bool { return r.activo.Load() }

// DB dice si el resto de puertos de la pasarela también.
func (r *Reenvio) DB() bool { return r.db.Load() }

// abortable es una conexión del invitado que se corta con un RST.
type abortable interface{ Abort() }

// semiCerrable admite cerrar solo la escritura.
type semiCerrable interface{ CloseWrite() error }

// Forward atiende UNA conexión del invitado al puerto port de la pasarela y la
// cierra al terminar. Si el proxy no contesta, la del invitado se corta con un
// RST: lo mismo que ve cuando no hay nadie en la pasarela.
func (r *Reenvio) Forward(ctx context.Context, c net.Conn, port int) {
	h, err := credproxy.SplitHeader(port)
	if err != nil {
		cortar(c)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, plazoConexion)
	var u net.Conn
	if r.Dial != nil {
		u, err = r.Dial(dctx)
	} else {
		u, err = (&net.Dialer{}).DialContext(dctx, "unix", r.ruta)
	}
	cancel()
	if err != nil {
		if ahora := time.Now().Unix(); r.avisado.Swap(ahora) != ahora {
			r.logf("WARNING: the credential proxy does not answer (%v): a guest connection to port %d was reset", err, port)
		}
		cortar(c)
		return
	}
	if _, err := u.Write(h[:]); err != nil {
		u.Close()
		cortar(c)
		return
	}
	Unir(ctx, c, u)
}

// cortar cierra c con un RST si puede.
func cortar(c net.Conn) {
	if a, ok := c.(abortable); ok {
		a.Abort()
	}
	_ = c.Close()
}

// Unir copia a y b en los dos sentidos hasta que los dos acaben (o ctx) y los
// cierra. Al acabar un sentido se cierra la escritura del otro extremo, para
// que un cliente que espera el fin de la respuesta no se quede colgado.
func Unir(ctx context.Context, a, b net.Conn) {
	parar := context.AfterFunc(ctx, func() { a.Close(); b.Close() })
	defer parar()
	var wg sync.WaitGroup
	copiar := func(dst, src net.Conn) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if s, ok := dst.(semiCerrable); ok && (err == nil || errors.Is(err, io.EOF)) {
			_ = s.CloseWrite()
		} else {
			dst.Close()
			src.Close()
		}
	}
	wg.Add(2)
	go copiar(b, a)
	go copiar(a, b)
	wg.Wait()
	a.Close()
	b.Close()
}
