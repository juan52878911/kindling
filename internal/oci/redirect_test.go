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

// La cabecera Authorization solo sigue a una redirección al mismo host:puerto.
func TestCheckRedirectQuitaAuthorization(t *testing.T) {
	req := func(u string) *http.Request {
		p, _ := url.Parse(u)
		return &http.Request{URL: p, Header: http.Header{"Authorization": {"Basic x"}}}
	}
	desde := []*http.Request{req("https://registry.example.com/v2/x/blobs/sha256:a")}
	for _, u := range []string{"https://cdn.example.com/b", "https://registry.example.com:8443/b", "https://sub.registry.example.com/b"} {
		r := req(u)
		if err := checkRedirect(r, desde); err != nil || r.Header.Get("Authorization") != "" {
			t.Errorf("%s: Authorization %q, %v", u, r.Header.Get("Authorization"), err)
		}
	}
	r := req("https://registry.example.com/otra")
	if err := checkRedirect(r, desde); err != nil || r.Header.Get("Authorization") == "" {
		t.Errorf("mismo host: se quitó la cabecera (%v)", err)
	}
}

func TestRealmPermitido(t *testing.T) {
	for _, c := range []struct {
		reg, realm string
		ok         bool
	}{
		{"registry-1.docker.io", "auth.docker.io", true},
		{"ghcr.io", "ghcr.io", true},
		{"registry.gitlab.com", "gitlab.com", true},
		{"registry.example.com:5000", "gitlab.example.com", true},
		{"localhost:5000", "127.0.0.1", true},
		{"ghcr.io", "evil.io", false},
		{"registry-1.docker.io", "docker.io.evil.com", false},
		{"registry.example.com", "example.org", false},
		{"10.0.0.5:5000", "10.0.0.6", false},
		{"localhost:5000", "example.com", false},
	} {
		if got := realmPermitido(c.reg, c.realm); got != c.ok {
			t.Errorf("realmPermitido(%q, %q) = %v", c.reg, c.realm, got)
		}
	}
}
