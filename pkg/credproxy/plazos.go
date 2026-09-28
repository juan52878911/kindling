package credproxy

// Plazos de una petición a través del proxy.
//
// POR QUÉ NO UN PLAZO TOTAL: antes toda la petición tenía 120 s (contexto,
// http.Client.Timeout y ReadTimeout/WriteTimeout del servidor). Un stream SSE
// de un LLM que razona o escribe mucho dura más que eso sin que nada vaya mal,
// y se cortaba a mitad. Lo que delata a una petición atascada no es cuánto
// dura sino cuánto lleva sin moverse.
//
// LOS TRES PLAZOS:
//   - HeaderTimeout: desde que se envía la petición hasta las cabeceras de la
//     respuesta (ResponseHeaderTimeout del transporte seguro).
//   - IdleTimeout: INACTIVIDAD. Un vigía cancela la petición si pasa ese tiempo
//     sin que se mueva un byte del cuerpo, en ninguno de los dos sentidos; cada
//     lectura o escritura lo renueva. Del lado del invitado, además, cada
//     lectura y escritura renueva el plazo de su conexión (un invitado que deja
//     de leer bloquea la escritura, y el contexto no la desbloquea).
//   - MaxDuration: techo absoluto. Es la red contra un invitado HOSTIL que
//     mantenga viva una petición goteando un byte cada poco: ocuparía una de
//     las MaxInFlight plazas para siempre.

import (
	"io"
	"net/http"
	"time"
)

const (
	// HeaderTimeout: plazo para las cabeceras de la respuesta del proveedor.
	HeaderTimeout = 60 * time.Second
	// IdleTimeout: plazo sin que se mueva un byte del cuerpo, en cualquier
	// sentido. Holgado: un modelo que razona puede tardar en emitir el
	// siguiente evento, y los proveedores serios mandan pings antes.
	IdleTimeout = 120 * time.Second
	// MaxDuration: techo de una petición entera, por mucho que se mueva.
	MaxDuration = 15 * time.Minute
)

// vigia cancela la petición cuando pasa d sin un latido.
type vigia struct {
	t *time.Timer
	d time.Duration
}

func nuevoVigia(d time.Duration, cancel func()) *vigia {
	return &vigia{t: time.AfterFunc(d, cancel), d: d}
}

func (v *vigia) latido() { v.t.Reset(v.d) }
func (v *vigia) parar()  { v.t.Stop() }

// plazosInvitado renueva los plazos de la conexión con el invitado: el
// siguiente movimiento debe llegar antes de idle, y nunca después de fin.
type plazosInvitado struct {
	rc   *http.ResponseController
	idle time.Duration
	fin  time.Time
}

func (p plazosInvitado) proximo() time.Time {
	t := time.Now().Add(p.idle)
	if t.After(p.fin) {
		return p.fin
	}
	return t
}

// Los errores se ignoran: un ResponseWriter que no admite plazos (uno de
// tests, un servidor ajeno) sigue teniendo el vigía y el techo del contexto.
func (p plazosInvitado) lectura()   { _ = p.rc.SetReadDeadline(p.proximo()) }
func (p plazosInvitado) escritura() { _ = p.rc.SetWriteDeadline(p.proximo()) }

// lectorVigilado es el cuerpo de la petición del invitado: cada lectura da un
// latido y renueva el plazo de lectura de su conexión.
type lectorVigilado struct {
	r io.Reader
	v *vigia
	p plazosInvitado
}

func (l lectorVigilado) Read(b []byte) (int, error) {
	l.p.lectura()
	n, err := l.r.Read(b)
	if n > 0 {
		l.v.latido()
	}
	// Terminado el cuerpo, fuera el plazo: el servidor deja una lectura de
	// fondo en la conexión para saber si el invitado se va, y si ese plazo
	// venciera cancelaría el contexto de la petición a mitad del stream. Solo
	// con EOF: tras otro error (el propio plazo vencido) el servidor intenta
	// descartar el resto del cuerpo, y sin plazo esperaría para siempre.
	if err == io.EOF {
		_ = l.p.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// escritorVigilado es la respuesta hacia el invitado: renueva el plazo de
// escritura antes de cada escritura y da un latido después.
type escritorVigilado struct {
	w http.ResponseWriter
	v *vigia
	p plazosInvitado
}

func (e escritorVigilado) Write(b []byte) (int, error) {
	e.p.escritura()
	n, err := e.w.Write(b)
	if n > 0 {
		e.v.latido()
	}
	return n, err
}

// Flush deja que el redactor empuje cada evento de un SSE en el acto.
func (e escritorVigilado) Flush() {
	e.p.escritura()
	_ = e.p.rc.Flush()
}

// origenVigilado es el cuerpo de la respuesta del proveedor: cada lectura con
// datos da un latido.
type origenVigilado struct {
	r io.Reader
	v *vigia
}

func (o origenVigilado) Read(b []byte) (int, error) {
	n, err := o.r.Read(b)
	if n > 0 {
		o.v.latido()
	}
	return n, err
}
