package guest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

type escritorConPlazo struct {
	*httptest.ResponseRecorder
	plazos []time.Time
	falla  bool
	n      int
}

func (e *escritorConPlazo) SetWriteDeadline(t time.Time) error {
	e.plazos = append(e.plazos, t)
	return nil
}
func (e *escritorConPlazo) Write(p []byte) (int, error) {
	e.n++
	if e.falla {
		return 0, errors.New("cliente colgado")
	}
	return e.ResponseRecorder.Write(p)
}

// Cada evento fija su plazo de escritura, y tras un fallo no se insiste.
func TestEmitFijaPlazoPorEventoYSeRindeTrasUnError(t *testing.T) {
	w := &escritorConPlazo{ResponseRecorder: httptest.NewRecorder()}
	e := newEmitter(w)
	e.emit(api.ExecEvent{Stream: "stdout", Data: []byte("a")})
	e.emit(api.ExecEvent{Stream: "stdout", Data: []byte("b")})
	if len(w.plazos) != 2 || !w.plazos[0].After(time.Now()) {
		t.Fatalf("plazos = %v", w.plazos)
	}
	w.falla = true
	e.emit(api.ExecEvent{Stream: "stdout", Data: []byte("c")})
	e.emit(api.ExecEvent{Stream: "stdout", Data: []byte("d")})
	if w.n != 3 {
		t.Fatalf("escrituras = %d, quería 3 (la cuarta no debe intentarse)", w.n)
	}
}

// /ready no se queda esperando a otra sonda si el cliente se fue.
func TestReadyRespetaElContextoEsperandoLaSonda(t *testing.T) {
	r := &readiness{probePath: "/bin/true", hooksDir: t.TempDir()}
	var corrio atomic.Bool
	suelta := make(chan struct{})
	r.probe = func(ctx context.Context, p string, env []string) (string, error) {
		corrio.Store(true)
		<-suelta
		return "", nil
	}
	go r.check(context.Background()) // la primera sonda queda colgada con probeMu
	for !corrio.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	r.check(ctx)
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("check tardó %s con el contexto vencido", d)
	}
	close(suelta)
	_ = http.StatusOK
}
