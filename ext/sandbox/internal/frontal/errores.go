package frontal

// Errores que ve el cliente.
//
// Los tenants hablan con el frontal por la red; el frontal habla con los
// daemons por sockets Unix o direcciones internas. Un error de transporte
// ("dial unix /var/lib/kling/kling.sock: connect: ...") lleva esas rutas y
// direcciones, que no son de ningún tenant. Por eso fail solo pasa tal cual los
// errores del cliente (4xx, que construye el propio frontal al validar) y lo
// que el daemon contestó con su código; un 5xx sin respuesta del daemon sale
// con un mensaje genérico que nombra el host, y el detalle va al log.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// errHost es un fallo hablando con un host concreto.
type errHost struct {
	host string
	err  error
}

func (e *errHost) Error() string { return e.host + ": " + e.err.Error() }
func (e *errHost) Unwrap() error { return e.err }

func deHost(h *hosts.Host, err error) error { return &errHost{host: h.Nombre, err: err} }

// errPublico es un error que lleva su propio mensaje para el cliente: msg sale
// tal cual y err solo va al log.
type errPublico struct {
	msg string
	err error
}

func (e *errPublico) Error() string   { return e.msg + " (" + e.err.Error() + ")" }
func (e *errPublico) Unwrap() error   { return e.err }
func (e *errPublico) publico() string { return e.msg }

// conPublico es lo que tienen errPublico y errCuotaSinContar.
type conPublico interface{ publico() string }

// fail responde un error con su código. Si el error viene del daemon con
// código propio, ese manda: un 507 o un 409 del daemon significan algo concreto
// para quien llama y aplastarlos con un 502 perdería justo eso.
func fail(w http.ResponseWriter, code int, err error) {
	if p, ok := err.(conPublico); ok {
		if code >= 500 {
			log.Printf("frontal: %d: %v", code, err)
		}
		writeJSON(w, code, api.Error{Message: p.publico()})
		return
	}
	var se *api.StatusError
	if errors.As(err, &se) && se.Code != 0 {
		writeJSON(w, se.Code, api.Error{Message: err.Error()})
		return
	}
	if code < 500 {
		writeJSON(w, code, api.Error{Message: err.Error()})
		return
	}
	log.Printf("frontal: %d: %v", code, err)
	writeJSON(w, code, api.Error{Message: mensajeGenerico(code, err)})
}

// mensajeGenerico describe un 5xx sin datos internos: qué hosts fallaron y
// de qué manera, pero no su endpoint ni el error de red.
func mensajeGenerico(code int, err error) string {
	switch {
	case errors.Is(err, hosts.ErrSinHosts):
		return hosts.ErrSinHosts.Error()
	case errors.Is(err, hosts.ErrSinSitio):
		return hosts.ErrSinSitio.Error()
	}
	que := "failed"
	if errors.Is(err, context.DeadlineExceeded) || code == http.StatusGatewayTimeout {
		que = "did not answer in time"
	}
	if hs := hostsDe(err); len(hs) > 0 {
		return fmt.Sprintf("kindling host %s %s (the gateway log has the details)", unir(hs), que)
	}
	return fmt.Sprintf("%s (the gateway log has the details)", http.StatusText(code))
}

// hostsDe recoge, sin repetir, los hosts nombrados en el árbol de err.
func hostsDe(err error) []string {
	var out []string
	var andar func(error)
	andar = func(e error) {
		if e == nil {
			return
		}
		if eh, ok := e.(*errHost); ok && !slices.Contains(out, eh.host) {
			out = append(out, eh.host)
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, x := range u.Unwrap() {
				andar(x)
			}
		case interface{ Unwrap() error }:
			andar(u.Unwrap())
		}
	}
	andar(err)
	return out
}

func unir(hs []string) string {
	s := ""
	for i, h := range hs {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%q", h)
	}
	return s
}

// mensajes es el aviso de X-Kindling-Partial: un trozo por host que no
// contestó, con lo que dijo el daemon si llegó a contestar.
func mensajes(errs []error) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		var se *api.StatusError
		hs := hostsDe(e)
		switch {
		case errors.As(e, &se):
			out[i] = e.Error()
		case len(hs) > 0:
			out[i] = unir(hs) + ": did not answer"
		default:
			out[i] = "a host did not answer"
		}
	}
	return out
}
