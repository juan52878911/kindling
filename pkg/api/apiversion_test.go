package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// daemonConAPI sirve /info con las cabeceras de API que se le den ("" = un
// daemon anterior, que no manda ninguna).
func daemonConAPI(t *testing.T, apiHdr, version string) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "kl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aquí: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiHdr != "" {
			w.Header().Set(HeaderAPI, apiHdr)
			w.Header().Set(HeaderVersion, version)
		}
		_, _ = w.Write([]byte(`{"version":"` + version + `"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewClient(sock)
}

func conAvisos(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	antes := APIWarning
	APIWarning = func(msg string) {
		if !strings.Contains(msg, "API 99") {
			t.Errorf("aviso sin el API del daemon: %q", msg)
		}
		n.Add(1)
	}
	t.Cleanup(func() { APIWarning = antes })
	return &n
}

// Un daemon con un API más nuevo que la del cliente: la petición sigue, con un
// aviso, y uno solo aunque haya muchas peticiones.
func TestDaemonConAPIMasNuevaAvisaUnaVez(t *testing.T) {
	avisos := conAvisos(t)
	c := daemonConAPI(t, "99", "v9.0.0")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		if _, err := c.Info(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := avisos.Load(); n != 1 {
		t.Fatalf("%d avisos, quería 1", n)
	}
}

// Un daemon con un API más vieja que la mínima del cliente es un error que
// dice qué actualizar, no un 404 a medio camino. Sin cabecera es un daemon
// anterior: el API 1.
func TestDaemonDemasiadoViejoEsUnErrorClaro(t *testing.T) {
	antes := MinDaemonAPI
	MinDaemonAPI = 2
	t.Cleanup(func() { MinDaemonAPI = antes })

	for hdr, version := range map[string]string{"": "v0.17.0", "1": "v0.18.0"} {
		c := daemonConAPI(t, hdr, version)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := c.Info(ctx)
		cancel()
		var viejo *ErrDaemonTooOld
		if !errors.As(err, &viejo) || viejo.API != 1 {
			t.Fatalf("cabecera %q: %v", hdr, err)
		}
		if !strings.Contains(err.Error(), "update the daemon") || strings.HasPrefix(err.Error(), "Get ") {
			t.Fatalf("mensaje poco claro: %q", err)
		}
	}
}

// El API de siempre, con o sin cabecera, ni avisa ni falla.
func TestDaemonConLaMismaAPINoDiceNada(t *testing.T) {
	avisos := conAvisos(t)
	for _, hdr := range []string{"", "1"} {
		c := daemonConAPI(t, hdr, "v0.18.0")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := c.Info(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	if avisos.Load() != 0 {
		t.Fatal("avisó con el mismo API")
	}
	if (&Info{}).DaemonAPI() != 1 || (&Info{API: 3}).DaemonAPI() != 3 {
		t.Fatal("DaemonAPI")
	}
}
