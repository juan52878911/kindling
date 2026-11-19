package main

// `kling sbx mcp`: servidor MCP por stdio que expone el gateway como
// herramientas (crear, ejecutar, leer/escribir, borrar sandboxes).

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/juan52878911/kindling/ext/sandbox/internal/mcpsbx"
)

// Do hace que el cliente de `sbx` sea la API del servidor MCP: misma
// autenticación y mismo control de redirecciones que el resto de comandos.
func (c *cliente) Do(ctx context.Context, metodo, ruta string, cuerpo io.Reader, tipo string) (*http.Response, error) {
	req, err := c.peticion(ctx, metodo, ruta, nil)
	if err != nil {
		return nil, err
	}
	if cuerpo != nil {
		req.Body = io.NopCloser(cuerpo)
		req.Header.Set("Content-Type", tipo)
	}
	return c.http.Do(req)
}

func sbxMCP(args []string) error {
	return servirMCP(args, os.Stdin, os.Stdout)
}

func servirMCP(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("sbx mcp", flag.ContinueOnError)
	url := fs.String("url", "", "gateway URL (default: $KLING_SANDBOX_URL, sandbox.url)")
	ca := fs.String("ca", "", "PEM file with an extra CA to trust (default: $KLING_SANDBOX_CA)")
	if err := fs.Parse(args); err != nil {
		return err
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
	ctx, stop := ctxSenales()
	defer stop()
	s := &mcpsbx.Servidor{API: c, Nombre: "kling-sandbox", Version: strings.TrimPrefix(Version, "v"),
		Secretos: []string{c.token}}
	return s.Servir(ctx, in, out)
}
