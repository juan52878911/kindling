// Package origen tiene el CheckRedirect de los clientes que llevan un token.
package origen

import (
	"fmt"
	"net/http"
)

// SoloMismoOrigen es el CheckRedirect de un cliente con Bearer: sigue una
// redirección al mismo esquema y host (puerto incluido) y se niega a cualquier
// otra. net/http reenvía Authorization a un subdominio y a otro puerto del
// mismo host, y de https a http del mismo host lo manda en claro; el gateway y
// el API de Kubernetes no redirigen fuera de sí mismos, así que si pasa es un error de configuración o alguien
// en medio, no un sitio al que llevar el token.
func SoloMismoOrigen(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after %d redirects", len(via))
	}
	orig := via[0].URL
	if req.URL.Scheme != orig.Scheme || req.URL.Host != orig.Host {
		return fmt.Errorf("%s redirects to %s: refusing to send the token to another origin", orig.Redacted(), req.URL.Redacted())
	}
	return nil
}
