// Package guesthttp construye los clientes HTTP que hablan con el agente de un
// invitado (o con lo que sirva dentro de la microVM).
//
// Lo que contesta un invitado no es de fiar: con root dentro puede responder lo
// que quiera, también un 3xx con Location hacia la IP de otra máquina, hacia el
// reenvío de 127.0.0.1 de otro kling-vz o hacia un servicio del loopback del
// host. Un http.Client por defecto sigue esas redirecciones —un 307/308 con el
// mismo método y cuerpo, un 303 como GET— y repetía la petición del daemon allí,
// saltándose la barrera entre invitados. Por eso aquí:
//
//   - CheckRedirect devuelve la respuesta 3xx tal cual, sin seguirla.
//   - El transporte, además, rechaza cualquier petición que no vaya al mismo
//     host:puerto que la que empezó la llamada, por si algún día alguien le
//     cambia el CheckRedirect al cliente.
//
// Todo cliente nuevo hacia un invitado sale de New.
package guesthttp

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// NoRedirect es el CheckRedirect de los clientes hacia invitados: el 3xx llega
// a quien llama como una respuesta más.
func NoRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// New da un cliente hacia invitados sobre rt (nil = un transporte propio sin
// conexiones guardadas). timeout 0 = sin plazo global.
func New(rt http.RoundTripper, timeout time.Duration) *http.Client {
	if rt == nil {
		rt = &http.Transport{DisableKeepAlives: true}
	}
	return &http.Client{
		Transport:     Guard(rt),
		Timeout:       timeout,
		CheckRedirect: NoRedirect,
	}
}

// ErrOtroHost: la petición iba a un host distinto del de la llamada original.
var ErrOtroHost = errors.New("guest request redirected to another host")

// Guard envuelve rt para que solo deje pasar peticiones al host de la llamada
// original. Envolver dos veces no cambia nada.
func Guard(rt http.RoundTripper) http.RoundTripper {
	if g, ok := rt.(*guardia); ok {
		return g
	}
	return &guardia{rt: rt}
}

type guardia struct{ rt http.RoundTripper }

// RoundTrip compara req con la petición que abrió la llamada. http.Client solo
// rellena req.Response en las peticiones que crea al seguir una redirección:
// subiendo por esa cadena se llega a la que construyó quien llama, y su
// URL.Host es la dirección del invitado que esperaba.
func (g *guardia) RoundTrip(req *http.Request) (*http.Response, error) {
	orig := req
	for orig.Response != nil && orig.Response.Request != nil {
		orig = orig.Response.Request
	}
	if req.URL.Host != orig.URL.Host {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, fmt.Errorf("%w: %s, expected %s", ErrOtroHost, req.URL.Host, orig.URL.Host)
	}
	return g.rt.RoundTrip(req)
}

// CloseIdleConnections lo reenvía http.Client al transporte.
func (g *guardia) CloseIdleConnections() {
	if c, ok := g.rt.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}
