package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/juan52878911/kindling/pkg/scheduler"
)

// configEscucha decide cómo sirve el gateway. Los tokens de inquilino y el
// tráfico de las herramientas viajan en cada petición: con certificado se
// sirve HTTPS; sin él, solo en loopback, salvo que se pida en claro a
// sabiendas (-insecure-listen). Devuelve nil si se sirve en claro.
//
// Es la misma regla que el frontal de ext/sandbox (escuchar en
// ext/sandbox/cmd/kling-sandbox/gateway.go), copiada aquí porque son módulos
// aparte; el loopback se decide con scheduler.IsLoopback, como -no-auth y
// -pprof. Se llama al validar los argumentos, antes de hablar con el daemon,
// para que un error salga al instante.
func configEscucha(addr, cert, key string, insecure bool) (*tls.Config, error) {
	if (cert == "") != (key == "") {
		return nil, errors.New("-tls-cert and -tls-key go together")
	}
	if cert != "" {
		par, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, fmt.Errorf("loading the TLS certificate: %w", err)
		}
		return &tls.Config{Certificates: []tls.Certificate{par}, MinVersion: tls.VersionTLS12}, nil
	}
	if !insecure && !scheduler.IsLoopback(addr) {
		return nil, fmt.Errorf("refusing to serve tokens over plain HTTP on %s: give -tls-cert and -tls-key, "+
			"listen on 127.0.0.1, or pass -insecure-listen", addr)
	}
	return nil, nil
}

// escuchar abre el puerto del gateway, con TLS si cfg no es nil.
func escuchar(addr string, cfg *tls.Config) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		return tls.NewListener(ln, cfg), nil
	}
	return ln, nil
}
