package guest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// conExec enciende kling.exec para la prueba, como si lo pidiera el kernel.
func conExec(t *testing.T) {
	t.Helper()
	viejo := cmdlineParams()
	vals := map[string]string{execBootParam: "1"}
	for k, v := range viejo.values {
		vals[k] = v
	}
	cmdlineVal = bootParams{values: vals, unknown: viejo.unknown}
	t.Cleanup(func() { cmdlineVal = viejo })
}

// Las rutas del agente, salvo /healthz, son solo del host: un proceso del
// invitado que llega por loopback, por la IP del invitado, por otra de sus
// direcciones o desde un contenedor con red puente recibe 403; el host (la
// pasarela, o el veth del host por la ruta por defecto) pasa.
func TestRutasSoloDelHost(t *testing.T) {
	falsos(t, nil, nil)
	conExec(t)
	viejas := direccionesPropias
	direccionesPropias = func() []net.IP { return []net.IP{net.ParseIP("172.16.0.2"), net.ParseIP("10.9.0.1")} }
	t.Cleanup(func() { direccionesPropias = viejas })
	conRutas(t, tablaInvitado(), nil)

	a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}}
	mux := http.NewServeMux()
	a.Register(mux)
	local := &net.TCPAddr{IP: net.ParseIP("172.16.0.2"), Port: 8080}
	pet := func(metodo, ruta, remota string) int {
		r := httptest.NewRequest(metodo, ruta, nil)
		r.RemoteAddr = remota
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(local)))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, r)
		return rr.Code
	}
	// GET también en las que cambian algo: con el filtro, 403; sin él, 405 o
	// lo que conteste la ruta, nunca lo que haría un POST.
	rutas := []string{"/exec", "/exec/stream", "/exec/pty", "/files", "/resync", "/volume/release",
		"/volume/sync", "/volume/acquire", "/service/stop", "/service", "/dns", "/procs", "/ready",
		"/hooks", "/meminfo", "/drop-caches", "/share/attach"}
	for _, ruta := range rutas {
		for _, dentro := range []string{"127.0.0.1:40000", "[::1]:40000", "172.16.0.2:40000", "10.9.0.1:40000", "172.17.0.2:40000"} {
			if c := pet(http.MethodGet, ruta, dentro); c != http.StatusForbidden {
				t.Errorf("%s desde %s: %d, quería 403", ruta, dentro, c)
			}
		}
		for _, host := range []string{"172.16.0.1:40000", "172.30.0.1:40000"} {
			if c := pet(http.MethodGet, ruta, host); c == http.StatusForbidden {
				t.Errorf("%s desde el host (%s): 403", ruta, host)
			}
		}
	}
	if c := pet(http.MethodGet, "/healthz", "127.0.0.1:40000"); c != http.StatusOK {
		t.Errorf("/healthz desde loopback: %d, quería 200", c)
	}
}
