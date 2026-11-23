package main

// `kling sbx osb`: shim compatible con OpenSandbox 1.2.1. Sirve el plano de
// control sandbox-lifecycle bajo /v1 y el execd de cada sandbox, traducidos a la
// API /v1 del gateway (docs/opensandbox.md).

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
	neturl "net/url"
	"os"
	"strings"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/osb"
)

// mapaImagenes es -image-map uri=plantilla, repetible.
type mapaImagenes map[string]string

func (m mapaImagenes) String() string { return fmt.Sprint(map[string]string(m)) }
func (m mapaImagenes) Set(v string) error {
	u, t, ok := strings.Cut(v, "=")
	if !ok || u == "" || t == "" {
		return fmt.Errorf("want uri=template, got %q", v)
	}
	m[u] = t
	return nil
}

func sbxOSB(args []string) error {
	fs := flag.NewFlagSet("sbx osb", flag.ContinueOnError)
	gw := fs.String("gateway", "", "gateway URL (default: $KLING_SANDBOX_URL, sandbox.url)")
	fs.StringVar(gw, "url", "", "alias of -gateway")
	ca := fs.String("ca", "", "PEM file with an extra CA to trust for the gateway (default: $KLING_SANDBOX_CA)")
	listen := fs.String("listen", "127.0.0.1:8080", "address of the OpenSandbox API (the SDK domain)")
	imagenes := mapaImagenes{}
	fs.Var(imagenes, "image-map", "image.uri=template: which kindling template an OSB image runs on (repeatable; also tried without the tag)")
	defecto := fs.String("default-template", "", "template for images not in -image-map (empty: those are a 422)")
	publico := fs.String("public", "", "host[:port] advertised in endpoints/{port} (default: the Host the SDK used)")
	ingreso := fs.String("ingress-domain", "", "the gateway's -ingress-domain: other sandbox ports are proxied there (empty: 501)")
	cert := fs.String("tls-cert", "", "TLS certificate (the SDK then needs protocol=https)")
	key := fs.String("tls-key", "", "TLS key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*cert == "") != (*key == "") {
		return errors.New("-tls-cert and -tls-key go together")
	}
	if *gw != "" {
		os.Setenv("KLING_SANDBOX_URL", *gw)
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
	// La OPEN-SANDBOX-API-KEY es el token del inquilino, salvo que se fije otra:
	// así el SDK no necesita el token real del gateway.
	clave := os.Getenv("KLING_OSB_KEY")
	if clave == "" {
		clave = c.token
	}
	sh := &osb.Servidor{API: c, Clave: clave, Imagenes: imagenes, PlantillaDefecto: *defecto,
		Publico: *publico, Log: func(l string) { log.Println(l) }}
	if *ingreso != "" {
		u, err := neturl.Parse(c.url)
		if err != nil {
			return fmt.Errorf("gateway URL: %w", err)
		}
		sh.Ingreso = &osb.Ingreso{Dominio: *ingreso, URL: u, Token: c.token, Transport: c.http.Transport}
	}
	if len(imagenes) == 0 && *defecto == "" {
		log.Printf("kling sbx osb: no -image-map nor -default-template: every create with image.uri is a 422 (templateId still works)")
	}

	ctx, stop := ctxSenales()
	defer stop()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: sh, ReadHeaderTimeout: 10 * time.Second}
	esquema := "http"
	if *cert != "" {
		esquema = "https"
	}
	log.Printf("kling sbx osb: OpenSandbox %s API on %s://%s -> %s", osb.VersionOSB, esquema, ln.Addr(), c.url)
	errc := make(chan error, 1)
	go func() {
		if *cert != "" {
			errc <- srv.ServeTLS(ln, *cert, *key)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	select {
	case err = <-errc:
	case <-ctx.Done():
		err = nil
	}
	cx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	srv.Shutdown(cx)
	cancel()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
