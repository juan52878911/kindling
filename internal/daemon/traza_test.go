package daemon

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tpPrueba = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

func TestTrazaDeSoloAceptaTraceparentBienFormado(t *testing.T) {
	r := httptest.NewRequest("GET", "/info", nil)
	if got := trazaDe(r); got != "" {
		t.Fatalf("sin cabecera: %q", got)
	}
	for _, malo := range []string{"x", tpPrueba + "0", strings.ToUpper(tpPrueba), strings.Replace(tpPrueba, "-", "_", 1), tpPrueba[:54] + "\n"} {
		r.Header.Set("Traceparent", malo)
		if got := trazaDe(r); got != "" {
			t.Errorf("%q se aceptó: %q", malo, got)
		}
	}
	r.Header.Set("Traceparent", tpPrueba)
	if got := trazaDe(r); got != " traceparent="+tpPrueba {
		t.Fatalf("válido: %q", got)
	}
}

// El traceparent de la petición sale en la línea de log del error interno.
func TestLogDeErrorInternoLlevaTraceparent(t *testing.T) {
	s, root, id := servidorConMaquina(t)
	p := filepath.Join(root, "audit", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nada", p); err != nil {
		t.Fatal(err)
	}
	logd := conLog(t)
	r := comoInquilino(httptest.NewRequest("GET", "/machines/svc/logs", nil), false)
	r.Header.Set("Traceparent", tpPrueba)
	r.SetPathValue("ref", "svc")
	s.handleLogs(httptest.NewRecorder(), r)
	if !strings.Contains(logd.String(), "traceparent="+tpPrueba) {
		t.Fatalf("el log no lleva el traceparent: %q", logd.String())
	}
}
