package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// La normalización de PEP 503 es lo que hace que el nombre del registro, el
// `pip install` y el ejecutable inferido hablen del MISMO paquete: pip trata
// "Mcp_Server.Git" y "mcp-server-git" como equivalentes, y si kindling no lo
// hiciera igual, la imagen instalaría con un nombre y arrancaría con otro.
func TestNormalizePyPI(t *testing.T) {
	cases := map[string]string{
		"semgrep":        "semgrep",
		"mcp-server-git": "mcp-server-git",
		"Mcp_Server.Git": "mcp-server-git",
		"a__b--c..d":     "a-b-c-d",
		"UPPER":          "upper",
	}
	for in, want := range cases {
		if got := NormalizePyPI(in); got != want {
			t.Errorf("NormalizePyPI(%q) = %q, want %q", in, got, want)
		}
	}
}

// ResolvePyPIBin aplica la convención de uvx (ejecutable = nombre normalizado
// de la distribución) sobre el nombre CANÓNICO que devuelve PyPI, no sobre lo
// que tecleó el usuario: es PyPI quien sabe cómo se llama de verdad el paquete.
func TestResolvePyPIBin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pypi/mcp-server-git/json":
			// PyPI canonicaliza a su manera; aquí se simula que el proyecto se
			// registró con guion bajo para comprobar que se re-normaliza.
			_, _ = w.Write([]byte(`{"info":{"name":"mcp_server_git"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := pypiBase
	pypiBase = srv.URL
	defer func() { pypiBase = old }()

	cl := &http.Client{Timeout: 5 * time.Second}

	// El nombre de entrada también se normaliza ANTES de preguntar: PyPI
	// responde a la forma normalizada de cualquier alias.
	got, err := ResolvePyPIBin(context.Background(), cl, "Mcp_Server.Git")
	if err != nil {
		t.Fatalf("ResolvePyPIBin: %v", err)
	}
	if got != "mcp-server-git" {
		t.Errorf("bin = %q, want mcp-server-git", got)
	}

	// Un paquete que no existe tiene que fallar AQUÍ, con el 404 a la vista, y
	// no dentro del chroot del daemon a mitad de `pip install`.
	if _, err := ResolvePyPIBin(context.Background(), cl, "no-existe"); err == nil {
		t.Error("un paquete inexistente debería dar error")
	}
}

// Si un servidor publica npm Y pypi, gana npm: su ejecutable se resuelve con
// certeza (campo `bin`), el de PyPI se infiere por convención. Y solo cuentan
// los paquetes stdio: el puente no sabe envolver otra cosa.
func TestStdioEligeEcosistema(t *testing.T) {
	stdio := func(reg, id string) Package {
		p := Package{RegistryType: reg, Identifier: id}
		p.Transport.Type = "stdio"
		return p
	}
	sse := func(reg, id string) Package {
		p := Package{RegistryType: reg, Identifier: id}
		p.Transport.Type = "sse"
		return p
	}

	cases := []struct {
		nombre string
		pkgs   []Package
		wantID string
		wantOK bool
	}{
		{"solo pypi", []Package{stdio("pypi", "semgrep")}, "semgrep", true},
		{"solo npm", []Package{stdio("npm", "srv")}, "srv", true},
		// npm gana aunque el registro liste pypi primero.
		{"ambos, pypi primero", []Package{stdio("pypi", "py"), stdio("npm", "js")}, "js", true},
		// pypi que solo habla SSE no vale: dentro del invitado nadie escucharía.
		{"pypi sin stdio", []Package{sse("pypi", "py")}, "", false},
		{"oci no se automatiza", []Package{stdio("oci", "img")}, "", false},
	}
	for _, c := range cases {
		s := &Server{Packages: c.pkgs}
		got, ok := s.Stdio()
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v", c.nombre, ok, c.wantOK)
			continue
		}
		if ok && got.Identifier != c.wantID {
			t.Errorf("%s: eligió %q, want %q", c.nombre, got.Identifier, c.wantID)
		}
	}
}
