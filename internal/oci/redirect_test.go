package oci

import (
	"net/http"
	"net/url"
	"testing"
)

func TestIsLocalHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "127.0.0.1": true, "::1": true,
		"localhost.example.com": false, "127.0.0.1.nip.io": false, "registry-1.docker.io": false,
	} {
		if got := isLocalHost(host); got != want {
			t.Errorf("isLocalHost(%q) = %v", host, got)
		}
	}
	for reg, want := range map[string]string{"localhost:5000": "localhost", "[::1]:5000": "::1", "ghcr.io": "ghcr.io"} {
		if got := registryHost(reg); got != want {
			t.Errorf("registryHost(%q) = %q, want %q", reg, got, want)
		}
	}
}

func TestCheckRedirect(t *testing.T) {
	req := func(u string) *http.Request {
		p, _ := url.Parse(u)
		return &http.Request{URL: p}
	}
	desde := []*http.Request{req("https://registry-1.docker.io/v2/x/blobs/sha256:a")}
	if err := checkRedirect(req("https://cdn.example.com/blob"), desde); err != nil {
		t.Fatalf("https a un CDN: %v", err)
	}
	if err := checkRedirect(req("http://cdn.example.com/blob"), desde); err == nil {
		t.Fatal("se siguió una redirección de https a http")
	}
	if err := checkRedirect(req("http://localhost:5000/b"), desde); err == nil {
		t.Fatal("un registro remoto redirigió a http://localhost y se siguió")
	}
	local := []*http.Request{req("http://127.0.0.1:5000/v2/x")}
	if err := checkRedirect(req("http://localhost:5000/b"), local); err != nil {
		t.Fatalf("registro de pruebas local: %v", err)
	}
	if err := checkRedirect(req("https://a/b"), make([]*http.Request, 10)); err == nil {
		t.Fatal("sin tope de redirecciones")
	}
}
