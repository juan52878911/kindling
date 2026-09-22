package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// De este contrato cuelga que una llamada NO se ejecute dos veces.
//
// callLink solo rehace el handshake y reenvia cuando el error es
// errStaleSession, que significa que el servidor rechazo la peticion ANTES de
// tocar la herramienta. Cualquier otro error —y un timeout el primero— puede
// haberse ejecutado ya: reenviarlo escribiria dos veces en un servicio con
// estado, sin que nada lo dijera.
func TestSoloUn400O404SonSesionCaducada(t *testing.T) {
	casos := []struct {
		nombre    string
		status    int
		caducada  bool
		reintenta string
	}{
		{"400: el puente no reconoce la sesion", 400, true, "si"},
		{"404: la sesion ya no existe", 404, true, "si"},
		{"500: el servidor fallo DESPUES de recibirla", 500, false, "no"},
		{"502: puede haber llegado", 502, false, "no"},
		{"503: puede haber llegado", 503, false, "no"},
	}
	for _, c := range casos {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
		}))
		_, err := mcpCallAt(context.Background(), srv.URL, "sid", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`)
		srv.Close()

		if err == nil {
			t.Errorf("%s: no dio error", c.nombre)
			continue
		}
		if got := errors.Is(err, errStaleSession); got != c.caducada {
			t.Errorf("%s: errStaleSession=%v, esperaba %v (reintentar: %s) — %v",
				c.nombre, got, c.caducada, c.reintenta, err)
		}
	}
}

// Un timeout NUNCA es sesion caducada: la peticion salio y puede haber escrito.
func TestUnTimeoutNoEsSesionCaducada(t *testing.T) {
	// Espera ACOTADA, no indefinida: httptest.Server.Close() aguarda a las
	// peticiones en vuelo, y un handler que no termina cuelga el test entero.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := mcpCallAt(ctx, srv.URL, "sid", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`)
	if err == nil {
		t.Fatal("el timeout no dio error")
	}
	if errors.Is(err, errStaleSession) {
		t.Error("un timeout se clasifico como sesion caducada: se reintentaria una llamada que quiza ya escribio")
	}
}
