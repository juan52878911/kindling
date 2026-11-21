package guest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest/share"
)

// Tabla de rutas del agente y su decisión: detrás de la puerta del resync o no.
// Las que actúan sobre el invitado o leen lo que el segundo tiempo toca van
// detrás; las que no dependen de la identidad, fuera.
func TestPuertaTablaDeRutas(t *testing.T) {
	a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}, tardio: true}
	mux := http.NewServeMux()
	a.Register(mux)

	tabla := []struct {
		ruta   string
		detras bool
	}{
		{api.GuestHooksPath, true},
		{"/volume/sync", true},
		{"/volume/release", true},
		{"/volume/acquire", true},
		{"/dns", true},
		{api.GuestServicePath, true},
		{api.GuestServiceStopPath, true},
		{api.GuestStartPath, true},
		{share.AttachPath, true},
		{api.GuestProcsPath, true},
		{api.GuestHealthPath, false},
		{api.GuestReadyPath, false},
		{api.GuestMemInfoPath, false},
		{api.GuestDropCachesPath, false},
	}
	puertaResync.Cerrar()
	t.Cleanup(puertaResync.Abrir)
	for _, c := range tabla {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		req := httptest.NewRequest(http.MethodPost, c.ruta, strings.NewReader("{}")).WithContext(ctx)
		rr := httptest.NewRecorder()
		hecho := make(chan struct{})
		go func() { defer close(hecho); mux.ServeHTTP(rr, req) }()
		select {
		case <-hecho:
		case <-time.After(3 * time.Second):
			cancel()
			<-hecho
		}
		cancel()
		retenida := rr.Code == http.StatusServiceUnavailable && strings.Contains(rr.Body.String(), "finishing a resync")
		if retenida != c.detras {
			t.Errorf("%s: detrás de la puerta = %v, quería %v (código %d: %s)", c.ruta, retenida, c.detras, rr.Code, rr.Body)
		}
	}
}

// Un pánico en el segundo tiempo abre la puerta y suelta resyncMu.
func TestSegundoTiempoPanicoAbreLaPuerta(t *testing.T) {
	falsos(t, nil, nil)
	viejo := hacerSegundoTiempo
	hacerSegundoTiempo = func(string, uint64) { panic("fallo de prueba") }
	t.Cleanup(func() { hacerSegundoTiempo = viejo })

	if rr := postResync(t, cuerpoInstancia(t)); rr.Code != http.StatusOK {
		t.Fatalf("código %d: %s", rr.Code, rr.Body)
	}
	esperarSegundoTiempo(t)
}

// Un cuelgue de E/S en el segundo tiempo no deja la puerta cerrada para
// siempre: el vigilante la abre y suelta el cerrojo.
func TestSegundoTiempoColgadoLoCortaElVigilante(t *testing.T) {
	falsos(t, nil, nil)
	suelta := make(chan struct{})
	viejo, tope := hacerSegundoTiempo, topeSegundoTiempo
	hacerSegundoTiempo = func(string, uint64) { <-suelta }
	topeSegundoTiempo = 200 * time.Millisecond
	t.Cleanup(func() { close(suelta); hacerSegundoTiempo, topeSegundoTiempo = viejo, tope })

	if rr := postResync(t, cuerpoInstancia(t)); rr.Code != http.StatusOK {
		t.Fatalf("código %d: %s", rr.Code, rr.Body)
	}
	if !puertaResync.Cerrada() {
		t.Fatal("la puerta debería estar cerrada mientras corre el segundo tiempo")
	}
	esperarSegundoTiempo(t)
}
