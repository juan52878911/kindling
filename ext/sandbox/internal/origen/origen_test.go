package origen

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSoloMismoOrigenNoLlevaElTokenAOtroHost(t *testing.T) {
	var conToken atomic.Int32
	otro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			conToken.Add(1)
		}
	}))
	defer otro.Close()
	// Mismo host, otro puerto: net/http compara solo el nombre y reenviaría
	// el token.
	otroURL := otro.URL
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fuera":
			http.Redirect(w, r, otroURL+"/x", http.StatusTemporaryRedirect)
		case "/dentro":
			http.Redirect(w, r, "/final", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer gw.Close()

	c := &http.Client{CheckRedirect: SoloMismoOrigen}
	pide := func(ruta string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, gw.URL+ruta, nil)
		req.Header.Set("Authorization", "Bearer secreto")
		return c.Do(req)
	}

	_, err := pide("/fuera")
	if n := conToken.Load(); n != 0 {
		t.Fatalf("the token reached the other origin %d time(s)", n)
	}
	if err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatalf("cross-origin redirect followed: %v", err)
	}
	resp, err := pide("/dentro")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("same-origin redirect: status %d", resp.StatusCode)
	}
}
