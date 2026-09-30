package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// frontalFalso apunta el cliente a un frontal que anota lo que recibe.
func frontalFalso(t *testing.T) (*[]string, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var rutas []string
	var cuerpos []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c map[string]any
		_ = json.NewDecoder(r.Body).Decode(&c)
		mu.Lock()
		rutas = append(rutas, r.Method+" "+r.URL.Path)
		cuerpos = append(cuerpos, c)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"h/abc","on_ttl":"freeze"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KLING_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KLING_SANDBOX_URL", srv.URL)
	t.Setenv("KLING_SANDBOX_TOKEN", "t")
	return &rutas, &cuerpos
}

// `kling sbx renew <id> -ttl 30m` es el orden que enseña la ayuda: tiene que
// funcionar (flag.Parse paraba en el id y daba el error de uso).
func TestRenewConFlagDetrasDelID(t *testing.T) {
	rutas, cuerpos := frontalFalso(t)
	if err := sbxRenew([]string{"h/abc", "-ttl", "30m"}); err != nil {
		t.Fatalf("renew <id> -ttl 30m: %v", err)
	}
	if len(*rutas) != 1 || (*rutas)[0] != "POST /v1/sandboxes/h/abc/renew" || (*cuerpos)[0]["ttl_seconds"] != float64(1800) {
		t.Fatalf("requests %v %v", *rutas, *cuerpos)
	}
}

// -ttl por debajo de un segundo es un error, no un 0 que se omite callado.
func TestTTLMenorDeUnSegundo(t *testing.T) {
	rutas, _ := frontalFalso(t)
	if err := sbxRenew([]string{"-ttl", "500ms", "h/abc"}); err == nil || !strings.Contains(err.Error(), "1s") {
		t.Errorf("renew -ttl 500ms: err = %v", err)
	}
	if err := sbxNew([]string{"-ttl", "500ms", "-q"}); err == nil || !strings.Contains(err.Error(), "1s") {
		t.Errorf("new -ttl 500ms: err = %v", err)
	}
	if len(*rutas) != 0 {
		t.Errorf("requests were sent: %v", *rutas)
	}
}
