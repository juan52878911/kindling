package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// run de punta a punta: gateway falso por HTTP y daemon falso por socket unix
// que sirve /info y un /metrics en el que las microVMs del servicio viven
// durante la ronda y desaparecen después. Comprueba el informe JSON, la fila
// Markdown y el tiempo hasta cero.
func TestRunDePuntaAPunta(t *testing.T) {
	g := nuevoFalso(false)
	gw := httptest.NewServer(g)
	defer gw.Close()

	// Directorio corto a propósito: la ruta de un socket unix no pasa de ~104
	// bytes en macOS, y la de t.TempDir() puede.
	dir, err := os.MkdirTemp("", "mcpb")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var vivas atomic.Int64
	vivas.Store(2)
	daemon := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info":
			fmt.Fprint(w, `{"version":"v9.9.9","firecracker":"v1.12.0","backend":"firecracker","arch":"amd64"}`)
		case "/metrics":
			fmt.Fprint(w, "kling_available_mib 5000\nkling_total_pss_mib 80\n")
			for i := range vivas.Load() {
				fmt.Fprintf(w, "kling_machine_pss_mib{id=\"m%d\",name=\"x\",service=\"a\",from=\"a\",state=\"running\"} 40\n", i)
			}
		}
	})}
	go func() { _ = daemon.Serve(ln) }()
	defer daemon.Close()

	// Las microVMs "se congelan" poco después de acabar la ronda.
	go func() {
		time.Sleep(300 * time.Millisecond)
		vivas.Store(0)
	}()

	t.Setenv("KLING_GATEWAY_TOKEN", "t0k")
	out := filepath.Join(dir, "r.json")
	var stdout, stderr strings.Builder
	err = run([]string{"-gateway", gw.URL, "-services", "a", "-sessions", "4", "-calls", "2",
		"-sock", sock, "-sample", "20ms", "-settle", "5s", "-label", "celda", "-json", out, "-md"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.SessionsOK != 4 || r.Status != "OK" || r.Latency.Steady.N != 8 || r.Host.Kling != "v9.9.9" {
		t.Errorf("informe: ok=%d status=%s steady=%d kling=%q", r.SessionsOK, r.Status, r.Latency.Steady.N, r.Host.Kling)
	}
	if r.Metrics == nil || r.Metrics.Samples == 0 || r.Metrics.TimeToZeroMS == nil || r.Metrics.DistinctMachines != 2 {
		t.Errorf("métricas: %+v", r.Metrics)
	}
	if !strings.HasPrefix(stdout.String(), "| celda | 1 | 4 | 4/4 |") {
		t.Errorf("fila md: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "time to zero") {
		t.Errorf("resumen sin tiempo hasta cero:\n%s", stderr.String())
	}
}

// Con el gateway caído no se corre nada: es un error de preparación, no una
// tabla de sesiones fallidas.
func TestRunGatewayCaido(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	var so, se strings.Builder
	err := run([]string{"-gateway", url, "-services", "a", "-sock", ""}, &so, &se)
	if err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("err = %v", err)
	}
}
