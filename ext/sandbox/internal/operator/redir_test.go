package operator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Ni el cliente del frontal ni el del API de Kubernetes llevan su token a otro
// origen si el servidor redirige.
func TestClientesNoReenvianElTokenTrasUnaRedireccion(t *testing.T) {
	var conToken atomic.Int32
	otro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			conToken.Add(1)
		}
	}))
	t.Cleanup(otro.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otro.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	if _, err := NewFrontalClient(srv.URL, "secreto").Get(context.Background(), "a/b"); err == nil {
		t.Fatal("frontal client: the redirect to another origin was followed")
	}
	k := NewKubeClient(&KubeConfig{BaseURL: srv.URL, Token: "secreto"})
	for _, c := range []*http.Client{k.http, k.watchHTTP} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api", nil)
		req.Header.Set("Authorization", "Bearer secreto")
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
			t.Fatal("kube client: the redirect to another origin was followed")
		}
	}
	if n := conToken.Load(); n != 0 {
		t.Fatalf("the token reached the other origin %d time(s)", n)
	}
}
