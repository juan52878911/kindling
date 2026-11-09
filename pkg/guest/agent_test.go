package guest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Con NewDeferred el agente escucha antes de leer MMDS, pero /healthz no
// contesta hasta que FinishBoot acaba: el daemon borra el entorno de MMDS en
// cuanto el agente le contesta. Las rutas de volúmenes no esperan.
func TestArranqueDiferidoEsperaAlEntorno(t *testing.T) {
	a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}, diferido: true, arrancado: make(chan struct{})}
	mux := http.NewServeMux()
	a.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("POST", "/volume/sync", nil))
	if rr.Code != 204 {
		t.Fatalf("/volume/sync durante el arranque: %d", rr.Code)
	}

	// Un cliente que se cansa antes: 503, no un "ok" sin entorno.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil).WithContext(ctx))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz con el arranque a medias: %d, quería 503", rr.Code)
	}

	hecho := make(chan int)
	go func() {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
		hecho <- rr.Code
	}()
	select {
	case c := <-hecho:
		t.Fatalf("/healthz contestó (%d) antes de acabar el arranque", c)
	case <-time.After(50 * time.Millisecond):
	}
	close(a.arrancado)
	select {
	case c := <-hecho:
		if c != 200 {
			t.Fatalf("/healthz tras el arranque: %d", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("/healthz sigue esperando tras el arranque")
	}
}

// Las rutas que el daemon usa con cualquier invitado tienen que existir, y
// /exec NO sin el parámetro del kernel: en una máquina de servicio ni se registra.
func TestRegisterRutasDelAgente(t *testing.T) {
	if ExecEnabled() {
		t.Skip("este anfitrión arrancó con kling.exec=1: la prueba de ausencia no aplica")
	}
	a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}}
	mux := http.NewServeMux()
	a.Register(mux)

	casos := []struct {
		metodo, ruta string
		quiero       int
	}{
		{"GET", "/healthz", 200},
		{"POST", "/volume/sync", 204},
		{"POST", "/volume/release", 204},
		{"POST", "/volume/acquire", 204},
		{"POST", "/exec", 404},
	}
	for _, c := range casos {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(c.metodo, c.ruta, nil))
		if rr.Code != c.quiero {
			t.Errorf("%s %s: %d, quería %d", c.metodo, c.ruta, rr.Code, c.quiero)
		}
	}
}

// Las rutas de volúmenes mutan: solo con POST. Un GET (el que repite un cliente
// que sigue un 303, por ejemplo) no puede desmontar nada.
func TestVolumenesSoloPost(t *testing.T) {
	vols := &Volumes{mounted: []VolumeSpec{{device: "/dev/vdz", mount: t.TempDir()}}}
	a := &Agent{Reaper: DefaultReaper, Volumes: vols}
	mux := http.NewServeMux()
	a.Register(mux)
	for _, ruta := range []string{"/volume/sync", "/volume/release", "/volume/acquire"} {
		for _, metodo := range []string{"GET", "HEAD", "PUT", "DELETE"} {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(metodo, ruta, nil))
			if rr.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d, quería 405", metodo, ruta, rr.Code)
			}
			if rr.Header().Get("Allow") != http.MethodPost {
				t.Errorf("%s %s: Allow %q", metodo, ruta, rr.Header().Get("Allow"))
			}
		}
	}
	if n := len(vols.Specs()); n != 1 {
		t.Fatalf("un GET soltó el volumen: quedan %d montados", n)
	}
}
