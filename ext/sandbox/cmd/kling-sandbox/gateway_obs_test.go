package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/frontal"
	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
)

func banderasDePrueba(t *testing.T, args ...string) banderasObs {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("KLING_SANDBOX_AUDIT_DIR", "")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	b := banderasObservabilidad(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestObservabilidadApagadaDevuelveElMismoHandler(t *testing.T) {
	h := http.NewServeMux()
	out, cerrar, err := observabilidad(h, nil, nil, banderasDePrueba(t))
	if err != nil || out != http.Handler(h) {
		t.Fatalf("handler wrapped while off: %v", err)
	}
	cerrar()
}

// El frontal de verdad detrás del envoltorio: identidad por token, auditoría
// de una acción y exportación de spans y métricas a un colector falso.
func TestObservabilidadConElFrontalReal(t *testing.T) {
	var spans, metricas atomic.Int64
	var cuerpoMetricas atomic.Value
	col := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/traces":
			spans.Add(int64(strings.Count(string(b), `"traceId"`)))
		case "/v1/metrics":
			metricas.Add(1)
			cuerpoMetricas.Store(string(b))
		}
	}))
	defer col.Close()
	srv, err := frontal.Nuevo(frontal.Opciones{
		Hosts:   hosts.Nuevo(nil),
		Tenants: []frontal.Tenant{{Nombre: "ana", Token: "tok-ana-secreto-1234567890"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "audit")
	h, cerrar, err := observabilidad(srv, srv.Identificar, srv.MetricasTexto,
		banderasDePrueba(t, "-otlp-endpoint", col.URL, "-otlp-metrics-interval", "50ms", "-audit-dir", dir))
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(h)
	defer web.Close()
	do := func(m, p string) (int, string) {
		rq, _ := http.NewRequest(m, web.URL+p, nil)
		rq.Header.Set("Authorization", "Bearer tok-ana-secreto-1234567890")
		resp, err := http.DefaultClient.Do(rq)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	do("DELETE", "/v1/sandboxes/lab/m1")
	code, body := do("GET", "/v1/audit")
	var ev struct {
		Events []struct{ Tenant, Action, Sandbox string }
	}
	json.Unmarshal([]byte(body), &ev)
	if code != 200 || len(ev.Events) != 1 || ev.Events[0].Tenant != "ana" || ev.Events[0].Action != "sandbox.delete" || ev.Events[0].Sandbox != "lab/m1" {
		t.Errorf("audit: %d %s", code, body)
	}
	for i := 0; i < 100 && metricas.Load() == 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	cerrar() // vacía los spans pendientes sin esperar al ritmo del lote
	if spans.Load() < 2 {
		t.Errorf("spans at the collector: %d", spans.Load())
	}
	if m, _ := cuerpoMetricas.Load().(string); !strings.Contains(m, "kling_sandbox_host_up") && !strings.Contains(m, "kling_sandbox_otlp_spans_dropped_total") {
		t.Errorf("metrics not exported: %q", m)
	}
	if st, err := os.Stat(filepath.Join(dir, "audit.jsonl")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("audit file: %v %v", st, err)
	}
	if _, err := srv.MetricasTexto(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestObservabilidadEndpointInvalidoFalla(t *testing.T) {
	_, _, err := observabilidad(http.NewServeMux(), nil, nil, banderasDePrueba(t, "-otlp-endpoint", "colector:4318"))
	if err == nil || !strings.Contains(err.Error(), "-otlp-endpoint") {
		t.Errorf("err = %v", err)
	}
}
