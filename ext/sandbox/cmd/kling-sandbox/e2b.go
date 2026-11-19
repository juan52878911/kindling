package main

// `kling sbx e2b`: shim compatible con E2B. Escucha en un puerto y traduce el
// plano de control y envd de E2B a la API /v1 del gateway.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/e2b"
)

func sbxE2B(args []string) error {
	fs := flag.NewFlagSet("sbx e2b", flag.ContinueOnError)
	url := fs.String("url", "", "gateway URL (default: $KLING_SANDBOX_URL, sandbox.url)")
	ca := fs.String("ca", "", "PEM file with an extra CA to trust for the gateway (default: $KLING_SANDBOX_CA)")
	listen := fs.String("listen", "127.0.0.1:3000", "address of the E2B control plane and envd")
	envdListen := fs.String("envd-listen", "", "extra address serving the same handler (E2B_DEBUG expects envd on 127.0.0.1:49983)")
	domain := fs.String("domain", "", "base domain advertised to clients (the E2B_DOMAIN they export)")
	home := fs.String("home", "/home/user", "directory that resolves relative paths of the SDK")
	debugSbx := fs.String("debug-sandbox", "", "gateway id of the sandbox that E2B_DEBUG=true clients (id debug_sandbox_id) talk to; needs -envd-listen on loopback and accepts requests without token there")
	cert := fs.String("tls-cert", "", "TLS certificate (the SDK uses https for envd outside E2B_DEBUG)")
	key := fs.String("tls-key", "", "TLS key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*cert == "") != (*key == "") {
		return errors.New("-tls-cert and -tls-key go together")
	}
	if *debugSbx != "" {
		if *envdListen == "" {
			return errors.New("-debug-sandbox needs -envd-listen")
		}
		h, _, err := net.SplitHostPort(*envdListen)
		if err != nil {
			return fmt.Errorf("-envd-listen: %w", err)
		}
		if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
			return errors.New("-debug-sandbox only with -envd-listen on a loopback IP (e.g. 127.0.0.1:49983)")
		}
		if *cert != "" {
			return errors.New("-debug-sandbox serves plain HTTP (E2B_DEBUG): do not combine with -tls-cert")
		}
	}
	if *url != "" {
		os.Setenv("KLING_SANDBOX_URL", *url)
	}
	c, err := nuevoCliente()
	if err != nil {
		return err
	}
	if *ca == "" {
		*ca = os.Getenv("KLING_SANDBOX_CA")
	}
	if *ca != "" {
		pem, err := os.ReadFile(*ca)
		if err != nil {
			return fmt.Errorf("-ca: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return errors.New("-ca: no certificate found in the file")
		}
		c.http.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	// La API key de E2B es el token del inquilino, salvo que se fije otra: así el
	// SDK no necesita el token real del gateway.
	clave := os.Getenv("KLING_E2B_KEY")
	if clave == "" {
		clave = c.token
	}
	sh := &e2b.Servidor{API: c, Clave: clave, Dominio: *domain, Home: *home,
		SandboxDebug: *debugSbx, Log: func(l string) { log.Println(l) }}
	if *debugSbx != "" {
		log.Printf("kling sbx e2b: DEBUG MODE: %s accepts requests WITHOUT X-Access-Token for sandbox %q (id %s); loopback only", *envdListen, *debugSbx, e2b.IDDebug)
	}

	ctx, stop := ctxSenales()
	defer stop()
	direcciones := []string{*listen}
	if *envdListen != "" {
		direcciones = append(direcciones, *envdListen)
	}
	errc := make(chan error, len(direcciones))
	var srvs []*http.Server
	for _, d := range direcciones {
		ln, err := net.Listen("tcp", d)
		if err != nil {
			return err
		}
		var h http.Handler = sh
		if d == *envdListen && *debugSbx != "" {
			h = sh.HandlerDebug()
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		srvs = append(srvs, srv)
		esquema := "http"
		if *cert != "" {
			esquema = "https"
		}
		log.Printf("kling sbx e2b: %s://%s -> %s", esquema, ln.Addr(), c.url)
		go func() {
			if *cert != "" {
				errc <- srv.ServeTLS(ln, *cert, *key)
			} else {
				errc <- srv.Serve(ln)
			}
		}()
	}
	select {
	case err = <-errc:
	case <-ctx.Done():
		err = nil
	}
	for _, s := range srvs {
		cx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.Shutdown(cx)
		cancel()
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
