package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Una ráfaga más corta que el intervalo de muestreo no deja foto propia: la de
// cierre, fechada en el final de la ronda, es la que ve las microVMs que
// despertó. Sin ella la tabla decía "0 máquinas" en las celdas congeladas.
func TestCierreCuentaLaRafagaCorta(t *testing.T) {
	cli := &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(metricsDaemon))}, nil
	})}
	smp := &sampler{client: cli, targets: map[string]bool{"tb-0": true, "tb-1": true},
		psiPath: "/no/existe", t0: time.Now().Add(-time.Hour)}
	end := 30 * time.Millisecond
	smp.cierre(context.Background(), end)

	h := summarizeSamples(smp.samples, end)
	if h.Samples != 1 || h.PeakLive != 2 || h.DistinctMachines != 2 {
		t.Fatalf("la foto de cierre no cuenta como parte de la ronda: %+v", h)
	}
}
