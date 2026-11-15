package operator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Al rotar el token de la ServiceAccount (kubelet reescribe el fichero), el
// cliente lo relee tras el primer 401 y reintenta: nada de 401 hasta
// reiniciar el pod.
func TestKubeClientReleeElTokenRotado(t *testing.T) {
	fichero := filepath.Join(t.TempDir(), "token")
	escribe := func(tok string) {
		t.Helper()
		if err := os.WriteFile(fichero, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	escribe("tok-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vale, _ := os.ReadFile(fichero)
		if r.Header.Get("Authorization") != "Bearer "+strings.TrimSpace(string(vale)) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"kind":"Status","message":"Unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"1"},"items":[]}`))
	}))
	t.Cleanup(srv.Close)

	k := NewKubeClient(&KubeConfig{BaseURL: srv.URL, Token: "tok-1", TokenFile: fichero})
	if _, err := k.List(context.Background()); err != nil {
		t.Fatalf("con el token inicial: %v", err)
	}
	escribe("tok-2") // kubelet rota; el servidor ya no acepta tok-1
	if _, err := k.List(context.Background()); err != nil {
		t.Fatalf("tras rotar el token: %v", err)
	}
	// El PATCH (con cuerpo) también se reintenta con el token nuevo.
	escribe("tok-3")
	if _, err := k.PatchFinalizers(context.Background(), "default", "x", nil); err != nil && strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("PATCH tras rotar el token: %v", err)
	}
}
