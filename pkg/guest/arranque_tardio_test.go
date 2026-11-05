package guest

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// agenteTardio es un agente de preparación de snapshot por imagen, como lo
// deja NewDeferred con kling.imgsnap=1, ya registrado en un mux.
func agenteTardio(t *testing.T) (*Agent, *http.ServeMux) {
	t.Helper()
	a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}, diferido: true,
		arrancado: make(chan struct{}), tardio: true, preparado: make(chan struct{})}
	a.Env = []string{"PATH=/usr/bin", "DE_LA_IMAGEN=1"}
	mux := http.NewServeMux()
	a.Register(mux)
	return a, mux
}

func pedir(mux *http.ServeMux, metodo, ruta, cuerpo string, ctx context.Context) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(metodo, ruta, strings.NewReader(cuerpo))
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	r.Header.Set("Accept", "application/json")
	mux.ServeHTTP(rr, r)
	return rr
}

// Lo que el host congela es esto: un agente que contesta /healthz y /procs,
// anuncia que espera, y NO tiene aún ni el entorno de ninguna máquina ni el
// servicio lanzado. El entorno llega después, por POST /machine/start, y
// solo entonces se abren las rutas que dependen de él.
func TestArranqueTardioElEntornoLlegaDespues(t *testing.T) {
	a, mux := agenteTardio(t)
	close(a.preparado) // lo que hace FinishBoot en este modo

	rr := pedir(mux, "GET", "/healthz", "", nil)
	if rr.Code != 200 {
		t.Fatalf("/healthz antes del start: %d", rr.Code)
	}
	var h api.GuestHealth
	if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil || !slices.Contains(h.Caps, api.GuestCapLateStart) {
		t.Fatalf("/healthz no anuncia %s: %s (%v)", api.GuestCapLateStart, rr.Body, err)
	}
	if rr := pedir(mux, "GET", api.GuestProcsPath, "", nil); rr.Code != 200 {
		t.Fatalf("/procs antes del start: %d", rr.Code)
	}
	for _, kv := range a.Entorno() {
		if strings.HasPrefix(kv, "SECRETO=") {
			t.Fatalf("el entorno de la máquina ya estaba antes del start: %v", a.Entorno())
		}
	}
	// /ready depende del entorno: espera.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if rr := pedir(mux, "GET", api.GuestReadyPath, "", ctx); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ready antes del start: %d, quería 503", rr.Code)
	}

	cuerpo := `{"env":{"SECRETO":"s3cr3t","DE_LA_IMAGEN":"2"},"cmd":{"cmd":["--flag"]}}`
	if rr := pedir(mux, "POST", api.GuestStartPath, cuerpo, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("start: %d %s", rr.Code, rr.Body)
	}
	select {
	case <-a.arrancado:
	default:
		t.Fatal("tras el start las rutas siguen esperando")
	}
	env := a.Entorno()
	if !slices.Contains(env, "SECRETO=s3cr3t") || !slices.Contains(env, "DE_LA_IMAGEN=2") || slices.Contains(env, "DE_LA_IMAGEN=1") {
		t.Fatalf("entorno tras el start: %v", env)
	}
	if a.machineCmd == nil || !slices.Equal(a.machineCmd.Cmd, []string{"--flag"}) {
		t.Fatalf("comando tras el start: %+v", a.machineCmd)
	}
	// Una sola vez: otra máquina no puede colarle su entorno a esta.
	if rr := pedir(mux, "POST", api.GuestStartPath, `{"env":{"OTRO":"x"}}`, nil); rr.Code != http.StatusConflict {
		t.Fatalf("segundo start: %d, quería 409", rr.Code)
	}
	if slices.Contains(a.Entorno(), "OTRO=x") {
		t.Fatal("el segundo start cambió el entorno")
	}
}

// Un cuerpo que no vale no gasta el único start.
func TestArranqueTardioRechazaYSigueEsperando(t *testing.T) {
	a, mux := agenteTardio(t)
	close(a.preparado)
	for _, c := range []string{`{"env":{"1MAL":"x"}}`, `{"env":{"A":"x\u0000y"}}`, `{`, `{"cmd":{"cmd":["a\u0000"]}}`} {
		if rr := pedir(mux, "POST", api.GuestStartPath, c, nil); rr.Code != http.StatusBadRequest {
			t.Errorf("start con %s: %d, quería 400", c, rr.Code)
		}
	}
	if rr := pedir(mux, "GET", api.GuestStartPath, "", nil); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET start: %d", rr.Code)
	}
	select {
	case <-a.arrancado:
		t.Fatal("un start inválido arrancó la máquina")
	default:
	}
	if rr := pedir(mux, "POST", api.GuestStartPath, "", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("start vacío: %d %s", rr.Code, rr.Body)
	}
}

// Sin kling.imgsnap no hay /machine/start ni se anuncia la capacidad: el host
// no puede tomar por preparada una máquina que ya lanzó su servicio.
func TestSinTardioNoHayStart(t *testing.T) {
	a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}}
	mux := http.NewServeMux()
	a.Register(mux)
	if rr := pedir(mux, "POST", api.GuestStartPath, "{}", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("start sin el modo: %d, quería 404", rr.Code)
	}
	if slices.Contains(a.Caps(), api.GuestCapLateStart) {
		t.Fatal("anuncia late-start sin el modo")
	}
	if !IsControlPath(api.GuestStartPath) {
		t.Fatal("/machine/start no es ruta de control: el gateway lo reenviaría")
	}
}

// Close antes del start: ya no se lanza nada.
func TestArranqueTardioCloseImpideElStart(t *testing.T) {
	a, mux := agenteTardio(t)
	close(a.preparado)
	a.inicioUna.Do(func() {}) // lo que hace Close, sin desmontar nada
	if rr := pedir(mux, "POST", api.GuestStartPath, "{}", nil); rr.Code != http.StatusConflict {
		t.Fatalf("start tras Close: %d, quería 409", rr.Code)
	}
}

func TestMotivoSinTardio(t *testing.T) {
	plano := []byte("#!/bin/sh\n# kling base\nexport PATH=" + initPATH + "\nexport HOME=" + initHOME + "\nexec " + guestAgentPath + " -listen :8080\n")
	if _, ok := PlainEntrypoint(plano); !ok {
		t.Fatal("el /entrypoint de prueba no es el generado")
	}
	viejos := []any{esPID1, leerEntrypoint, servicioDeclarado}
	t.Cleanup(func() {
		esPID1 = viejos[0].(func() bool)
		leerEntrypoint = viejos[1].(func() ([]byte, error))
		servicioDeclarado = viejos[2].(func() (*Service, error))
	})
	casos := []struct {
		nombre string
		pid1   bool
		entry  []byte
		eerr   error
		svc    *Service
		vale   bool
	}{
		{"docker sin /entrypoint", true, nil, fs.ErrNotExist, newService(api.ServiceSpec{Argv: []string{"/app"}}), true},
		{"base generada", true, plano, nil, nil, true},
		{"entrypoint propio", true, []byte("#!/bin/sh\nmidaemon &\nexec /usr/local/bin/kling-guest\n"), nil, nil, false},
		{"no es PID 1", false, nil, fs.ErrNotExist, nil, false},
		{"init de la imagen", true, nil, fs.ErrNotExist, newService(api.ServiceSpec{Argv: []string{"/init"}, Init: true}), false},
	}
	for _, c := range casos {
		esPID1 = func() bool { return c.pid1 }
		leerEntrypoint = func() ([]byte, error) { return c.entry, c.eerr }
		servicioDeclarado = func() (*Service, error) { return c.svc, nil }
		if m := motivoSinTardio(); (m == "") != c.vale {
			t.Errorf("%s: motivo %q, quería apta=%v", c.nombre, m, c.vale)
		}
	}
}
