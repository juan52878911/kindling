package oci_test

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
)

// Un registro privado, con Basic directo (registry:2 con htpasswd) y con el
// flujo Bearer (el token se pide con Basic): sin credenciales falla y dice
// cómo darlas; con las buenas baja; con malas dice que se rechazaron. Ni el
// error ni el log llevan la contraseña.
func TestRegistroPrivado(t *testing.T) {
	const pass = "s3cr3t-registry-pass"
	for _, basic := range []bool{true, false} {
		r := ocitest.New()
		r.User, r.Pass, r.Basic = "juan", pass, basic
		layer := ocitest.TarGz([]ocitest.File{{Name: "a", Body: "x"}})
		_, idx := r.Image("amd64", []string{"/a"}, layer)
		r.Tag("v1", idx)
		ref, _ := oci.ParseImageRef(r.Host() + "/priv/app:v1")
		key, _ := oci.CredentialKey(r.Host())

		var log bytes.Buffer
		c := &oci.Client{Cache: t.TempDir(), Log: &log}
		_, err := c.Resolve(context.Background(), ref)
		if err == nil || !strings.Contains(err.Error(), "kling registry login "+key) {
			t.Fatalf("basic=%v: sin credenciales: %v", basic, err)
		}

		c = &oci.Client{Cache: t.TempDir(), Log: &log, Auth: map[string]oci.Credential{key: {Username: "juan", Password: "mala"}}}
		_, err = c.Resolve(context.Background(), ref)
		if err == nil || !strings.Contains(err.Error(), "were refused") {
			t.Fatalf("basic=%v: credenciales malas: %v", basic, err)
		}

		c = &oci.Client{Cache: t.TempDir(), Log: &log, Auth: map[string]oci.Credential{key: {Username: "juan", Password: pass}}}
		d, err := c.Resolve(context.Background(), ref)
		if err != nil || d != idx {
			t.Fatalf("basic=%v: Resolve = %s, %v", basic, d, err)
		}
		if _, err := c.Pull(context.Background(), ref.Name(), d, "amd64"); err != nil {
			t.Fatalf("basic=%v: Pull: %v", basic, err)
		}
		if strings.Contains(log.String(), pass) {
			t.Fatalf("basic=%v: la contraseña salió en el log: %s", basic, log.String())
		}
		r.Close()
	}
}

// Las credenciales de un registro no van a otro: ni a otro registro, ni al CDN
// al que redirigen los blobs (aunque sea el mismo host en otro puerto, que Go
// sí dejaría pasar).
func TestCredencialesSoloASuHost(t *testing.T) {
	const pass = "s3cr3t-registry-pass"
	r := ocitest.New()
	defer r.Close()
	r.User, r.Pass, r.Basic = "juan", pass, true

	var mu sync.Mutex
	var vistos []string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		vistos = append(vistos, req.Header.Get("Authorization"))
		mu.Unlock()
		w.Write(r.Blob(path.Base(req.URL.Path)))
	}))
	defer cdn.Close()
	r.BlobRedirect = cdn.URL

	layer := ocitest.TarGz([]ocitest.File{{Name: "a", Body: "x"}})
	man, _ := r.Image("amd64", nil, layer)
	key, _ := oci.CredentialKey(r.Host())
	c := &oci.Client{Cache: t.TempDir(), Auth: map[string]oci.Credential{key: {Username: "juan", Password: pass}}}
	if _, err := c.Pull(context.Background(), r.Host()+"/priv/app", man, "amd64"); err != nil {
		t.Fatal(err)
	}
	if len(vistos) != 2 { // la configuración y la capa
		t.Fatalf("el CDN recibió %d peticiones, quiero 2", len(vistos))
	}
	for _, a := range vistos {
		if a != "" {
			t.Fatalf("la cabecera Authorization llegó al CDN tras la redirección: %q", a)
		}
	}

	// Las de otro registro no se usan con éste.
	otro := &oci.Client{Cache: t.TempDir(), Auth: map[string]oci.Credential{"ghcr.io": {Username: "juan", Password: pass}}}
	if _, err := otro.Pull(context.Background(), r.Host()+"/priv/app", man, "amd64"); err == nil ||
		!strings.Contains(err.Error(), "kling registry login") || strings.Contains(err.Error(), pass) {
		t.Fatalf("credenciales de otro registro: %v", err)
	}
}

func TestCredentialKey(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io": "ghcr.io", "GHCR.io": "ghcr.io", "https://index.docker.io/v1/": "docker.io",
		"docker.io": "docker.io", "registry-1.docker.io": "docker.io", "localhost:5000": "localhost:5000",
		"https://registry.example.com:8443/": "registry.example.com:8443",
	} {
		if got, err := oci.CredentialKey(in); err != nil || got != want {
			t.Errorf("CredentialKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "a b", "user@host", "host:port:x", "https://"} {
		if _, err := oci.CredentialKey(bad); err == nil {
			t.Errorf("CredentialKey(%q) accepted", bad)
		}
	}
}

// Las credenciales de un registro van a su servicio de tokens solo si es el
// mismo host o uno de los conocidos (registry-1.docker.io → auth.docker.io):
// en un sufijo compartido (*.azurecr.io), el realm de otro inquilino no las
// recibe aunque comparta dominio. Todo se sirve desde un TLS local: el
// transporte manda cualquier host a él.
func TestRealmDeOtroInquilinoSinCredenciales(t *testing.T) {
	const pass = "s3cr3t-tenant-pass"
	for _, c := range []struct {
		reg, realm string
		manda      bool
	}{
		{"mio.azurecr.io", "otro.azurecr.io", false},
		{"mio.jfrog.io", "otro.jfrog.io", false},
		{"registry.example.co.uk", "evil.co.uk", false},
		{"registry.example.com", "auth.example.com", false},
		{"mio.azurecr.io", "mio.azurecr.io", true},
		{"registry-1.docker.io", "auth.docker.io", true},
		{"registry.gitlab.com", "gitlab.com", true},
	} {
		var mu sync.Mutex
		var auths []string
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if strings.HasPrefix(req.URL.Path, "/token") {
				mu.Lock()
				auths = append(auths, req.Host+" "+req.Header.Get("Authorization"))
				mu.Unlock()
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Www-Authenticate", `Bearer realm="https://`+c.realm+`/token",service="x"`)
			http.Error(w, "auth", http.StatusUnauthorized)
		}))
		tr := srv.Client().Transport.(*http.Transport).Clone()
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		}
		tr.TLSClientConfig.InsecureSkipVerify = true
		key, _ := oci.CredentialKey(c.reg)
		cl := &oci.Client{Cache: t.TempDir(), HTTP: &http.Client{Transport: tr},
			Auth: map[string]oci.Credential{key: {Username: "juan", Password: pass}}}
		_, err := cl.Pull(context.Background(), c.reg+"/app/x", "sha256:"+strings.Repeat("0", 64), "amd64")
		srv.Close()
		if err == nil {
			t.Fatalf("%s: el Pull pasó", c.reg)
		}
		mandó := false
		for _, a := range auths {
			if strings.HasSuffix(a, " ") {
				continue
			}
			mandó = true
		}
		if mandó != c.manda {
			t.Errorf("%s → realm %s: credenciales mandadas = %v, quiero %v (%q; %v)", c.reg, c.realm, mandó, c.manda, auths, err)
		}
	}
}
