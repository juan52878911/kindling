package plugin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
)

type ceros struct{}

func (ceros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Un asset grande va a disco según llega: antes se leía entero a memoria
// (hasta 512 MiB, y el doble mientras crecía el búfer). Y si no se verifica,
// no deja temporal en el directorio.
func TestAssetGrandeNoVaAMemoria(t *testing.T) {
	const size = 128 << 20
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			http.NotFound(w, r)
			return
		}
		io.CopyN(w, ceros{}, size)
	}))
	defer srv.Close()
	dir := t.TempDir()
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	_, err := Install(context.Background(), InstallOptions{Name: "demo", CoreVersion: "1.0.0", Dir: dir,
		Client: srv.Client(), From: srv.URL + "/v1.0.0/kling-demo", SHA256: strings.Repeat("0", 64)})
	runtime.ReadMemStats(&m1)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if got := m1.TotalAlloc - m0.TotalAlloc; got > size/2 { // sin el arreglo, más del doble de size
		t.Fatalf("a %d MiB asset allocated %d MiB", size>>20, got>>20)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("left %d files in the install dir: %s", len(ents), ents[0].Name())
	}
}
