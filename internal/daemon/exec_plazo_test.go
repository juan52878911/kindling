package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// agenteQueGotea manda cabeceras y luego, cada poco, una línea vacía, sin
// evento final: el plazo de inactividad no vence nunca.
func agenteQueGotea(tb testing.TB, durante time.Duration) *httptest.Server {
	tb.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fin := time.Now().Add(durante)
		for time.Now().Before(fin) {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return
			}
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	tb.Cleanup(srv.Close)
	return srv
}

// duracionFlujoGoteando es cuánto tarda readExecEvents en soltar un flujo que
// gotea líneas vacías, con un timeout de 1 s y execBodyGrace de 200 ms.
func duracionFlujoGoteando(tb testing.TB, durante time.Duration) (time.Duration, api.ExecEvent) {
	tb.Helper()
	viejo := execBodyGrace
	execBodyGrace = 200 * time.Millisecond
	tb.Cleanup(func() { execBodyGrace = viejo })
	g := agenteQueGotea(tb, durante)
	t0 := time.Now()
	body, code, err := openGuestExec(context.Background(), g.URL, api.ExecRequest{Cmd: []string{"x"}, TimeoutSeconds: 1})
	if err != nil {
		tb.Fatalf("%d %v", code, err)
	}
	defer body.Close()
	var ultimo api.ExecEvent
	for ev := range readExecEvents(body, 1<<20) {
		ultimo = ev
	}
	return time.Since(t0), ultimo
}

// BenchmarkExecFlujoGoteando mide cuánto retiene el daemon un exec cuyo
// agente gotea líneas vacías durante 3 s con un timeout de 1 s (B-16).
func BenchmarkExecFlujoGoteando(b *testing.B) {
	var total time.Duration
	for i := 0; i < b.N; i++ {
		d, _ := duracionFlujoGoteando(b, 3*time.Second)
		total += d
	}
	b.ReportMetric(total.Seconds()/float64(b.N), "s-held/op")
}
