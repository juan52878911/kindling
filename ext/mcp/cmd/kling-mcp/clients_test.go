package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testURL   = "http://lab:8080/mcp/_all"
	testToken = "s3cr3t0"
)

// Cada cliente inventó su propio nombre para el mismo campo. Este test es el
// que impide que un refactor los uniforme "por limpieza" y rompa dos de ellos.
func TestEntryFormatPorCliente(t *testing.T) {
	cases := map[string]struct {
		section string
		want    string
	}{
		"claude-code": {"mcpServers", `{"headers":{"Authorization":"Bearer s3cr3t0"},"type":"http","url":"http://lab:8080/mcp/_all"}`},
		"opencode":    {"mcp", `{"enabled":true,"headers":{"Authorization":"Bearer s3cr3t0"},"type":"remote","url":"http://lab:8080/mcp/_all"}`},
		"cursor":      {"mcpServers", `{"headers":{"Authorization":"Bearer s3cr3t0"},"url":"http://lab:8080/mcp/_all"}`},
		"vscode":      {"servers", `{"headers":{"Authorization":"Bearer s3cr3t0"},"type":"http","url":"http://lab:8080/mcp/_all"}`},
		// serverUrl, no url: con `url` Windsurf lo ignora en silencio.
		"windsurf": {"mcpServers", `{"headers":{"Authorization":"Bearer s3cr3t0"},"serverUrl":"http://lab:8080/mcp/_all"}`},
		// streamableHttp en camelCase: con "streamable-http" Cline cae a SSE.
		"cline": {"mcpServers", `{"autoApprove":[],"disabled":false,"headers":{"Authorization":"Bearer s3cr3t0"},"type":"streamableHttp","url":"http://lab:8080/mcp/_all"}`},
	}

	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			c, ok := findClient(name)
			if !ok {
				t.Fatalf("no existe el cliente %q", name)
			}
			if c.section != want.section {
				t.Errorf("sección = %q, want %q", c.section, want.section)
			}
			if got := mustJSON(c.entry(testURL, testToken)); got != want.want {
				t.Errorf("entrada:\n got  %s\n want %s", got, want.want)
			}
		})
	}
}

// Sin token no debe aparecer una cabecera vacía: un "Authorization: Bearer "
// suelto haría que el gateway rechazara al cliente con un 401 confuso.
func TestEntrySinTokenNoPoneCabecera(t *testing.T) {
	for _, c := range clients() {
		if c.entry == nil {
			continue
		}
		if _, hay := c.entry(testURL, "")["headers"]; hay {
			t.Errorf("%s: mete headers sin haber token", c.name)
		}
	}
}

// Zed no habla MCP remoto por HTTP, así que no se le parchea la configuración:
// se le enseña el rodeo. Si alguien lo convierte en parcheable sin comprobar
// que Zed ya lo soporta, este test lo para.
func TestZedNoSeParchea(t *testing.T) {
	c, _ := findClient("zed")
	if c.manual == "" {
		t.Error("Zed debe seguir marcado como manual mientras no hable HTTP remoto")
	}
	if s := c.text("kindling", testURL, testToken); !strings.Contains(s, "mcp-remote") {
		t.Errorf("el fragmento de Zed debe enseñar el rodeo con mcp-remote, dice:\n%s", s)
	}
}

func TestInstallEscribeYRespalda(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// PATH vacío para que no encuentre `claude` ni `code`: aquí se ejercita el
	// parcheo del fichero, que es lo único que puede corromper algo del usuario.
	t.Setenv("PATH", "")

	previo := `{"mcpServers":{"otro":{"url":"http://ya-estaba"}},"algo":"que no se toca"}`
	dst := filepath.Join(tmp, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(previo), 0o600); err != nil {
		t.Fatal(err)
	}

	c, _ := findClient("cursor")
	if err := c.install("kindling", testURL, testToken); err != nil {
		t.Fatalf("install: %v", err)
	}

	var doc map[string]any
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("dejó el fichero ilegible: %v", err)
	}

	// Lo que ya estaba sigue estando: la configuración es del usuario.
	if doc["algo"] != "que no se toca" {
		t.Error("se perdieron claves de nivel superior")
	}
	srv, _ := doc["mcpServers"].(map[string]any)
	if _, ok := srv["otro"]; !ok {
		t.Error("se perdió el servidor que ya había")
	}
	if _, ok := srv["kindling"]; !ok {
		t.Error("no escribió la entrada nueva")
	}

	// Y hay copia de seguridad con el contenido anterior.
	bak, err := os.ReadFile(dst + ".kling-backup")
	if err != nil {
		t.Fatalf("no dejó respaldo: %v", err)
	}
	if string(bak) != previo {
		t.Errorf("el respaldo no es el original:\n got  %s\n want %s", bak, previo)
	}
}

// Un fichero con comentarios no se parchea NUNCA: json.Unmarshal los rechaza y
// una reescritura los borraría. Se prefiere no hacer nada a hacerlo a medias.
func TestInstallNoCorrompeJSONC(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("PATH", "")

	conComentarios := "{\n  // esto lo puso su dueño\n  \"mcpServers\": {}\n}\n"
	dst := filepath.Join(tmp, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(conComentarios), 0o600); err != nil {
		t.Fatal(err)
	}

	c, _ := findClient("cursor")
	if err := c.install("kindling", testURL, testToken); err != nil {
		t.Fatalf("debería avisar y seguir, no fallar: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != conComentarios {
		t.Errorf("tocó un fichero que no sabe parsear:\n got  %q\n want %q", got, conComentarios)
	}
}

func TestFindClientAlias(t *testing.T) {
	for alias, want := range map[string]string{
		"claude": "claude-code", "claude-code": "claude-code",
		"code": "vscode", "vscode": "vscode",
	} {
		c, ok := findClient(alias)
		if !ok || c.name != want {
			t.Errorf("findClient(%q) = %q,%v; want %q", alias, c.name, ok, want)
		}
	}
	if _, ok := findClient("emacs"); ok {
		t.Error("findClient debería fallar con un cliente que no está en la tabla")
	}
}
