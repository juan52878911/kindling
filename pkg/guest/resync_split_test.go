package guest

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// peticionHost lanza un POST /resync por el mux del agente como si llegara del
// host (la pasarela, por la ruta por defecto).
func peticionHost(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, api.GuestResyncPath, strings.NewReader(body))
	r.RemoteAddr = "172.16.0.1:40000"
	local := &net.TCPAddr{IP: net.ParseIP("172.16.0.2"), Port: 8080}
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(local)))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	return rr
}

// La respuesta lleva lo que tardó el camino crítico y la comprobación del
// origen, sin segundo viaje.
func TestResyncRespondeCriticoYOrigen(t *testing.T) {
	falsos(t, nil, nil)
	conExec(t)
	conRutas(t, tablaInvitado(), nil)
	a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}}
	mux := http.NewServeMux()
	a.Register(mux)

	rr := peticionHost(t, mux, cuerpo(t, time.Now().UnixNano(), make([]byte, api.GuestResyncEntropy)))
	if rr.Code != http.StatusOK {
		t.Fatalf("código %d: %s", rr.Code, rr.Body)
	}
	var res api.GuestResyncResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.CriticalUS <= 0 || res.OriginUS <= 0 {
		t.Fatalf("critical_us=%d origin_us=%d, quería ambos > 0", res.CriticalUS, res.OriginUS)
	}
	if res.OriginUS > res.CriticalUS {
		t.Fatalf("origin_us=%d mayor que critical_us=%d", res.OriginUS, res.CriticalUS)
	}
	esperarSegundoTiempo(t)
}

// Las direcciones propias se leen una vez y se vuelven a leer tras un /resync
// con restauración, que es cuando pueden cambiar.
func TestDireccionesPropiasSeCachean(t *testing.T) {
	falsos(t, nil, nil)
	conExec(t)
	conRutas(t, tablaInvitado(), nil)
	lecturas := 0
	viejo := leerDireccionesPropias
	leerDireccionesPropias = func() []net.IP { lecturas++; return []net.IP{net.ParseIP("172.16.0.2")} }
	t.Cleanup(func() { leerDireccionesPropias = viejo; invalidarDireccionesPropias() })
	invalidarDireccionesPropias()

	a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}}
	mux := http.NewServeMux()
	a.Register(mux)
	for i := 0; i < 3; i++ {
		if rr := peticionHost(t, mux, cuerpo(t, time.Now().UnixNano(), make([]byte, api.GuestResyncEntropy))); rr.Code != http.StatusOK {
			t.Fatalf("resync sin restauración: %d: %s", rr.Code, rr.Body)
		}
	}
	if lecturas != 1 {
		t.Fatalf("lecturas tras 3 peticiones sin restauración = %d, quería 1", lecturas)
	}
	// Con restauración se invalida: la siguiente petición lee de nuevo.
	if rr := peticionHost(t, mux, cuerpoInstancia(t)); rr.Code != http.StatusOK {
		t.Fatalf("resync con restauración: %d: %s", rr.Code, rr.Body)
	}
	esperarSegundoTiempo(t)
	peticionHost(t, mux, cuerpo(t, time.Now().UnixNano(), make([]byte, api.GuestResyncEntropy)))
	if lecturas != 2 {
		t.Fatalf("lecturas tras la restauración = %d, quería 2", lecturas)
	}
	esperarSegundoTiempo(t)
}

// /healthz cuenta los segundos tiempos que vencieron su tope.
func TestHealthzCuentaSegundosTiemposVencidos(t *testing.T) {
	falsos(t, nil, nil)
	suelta := make(chan struct{})
	viejo, tope := hacerSegundoTiempo, topeSegundoTiempo
	hacerSegundoTiempo = func(string, uint64) { <-suelta }
	topeSegundoTiempo = 50 * time.Millisecond
	t.Cleanup(func() { close(suelta); hacerSegundoTiempo, topeSegundoTiempo = viejo, tope })

	leer := func() uint64 {
		a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}}
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, api.GuestHealthPath, nil)
		r.Header.Set("Accept", "application/json")
		a.handleHealthz(rr, r)
		var h struct {
			N uint64 `json:"segundos_tiempos_vencidos"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		return h.N
	}
	antes := leer()
	if rr := postResync(t, cuerpoInstancia(t)); rr.Code != http.StatusOK {
		t.Fatalf("código %d: %s", rr.Code, rr.Body)
	}
	esperarSegundoTiempo(t)
	if d := leer() - antes; d != 1 {
		t.Fatalf("segundos_tiempos_vencidos subió %d, quería 1", d)
	}
}
