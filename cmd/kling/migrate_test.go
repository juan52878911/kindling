package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// La invariante de migrate: conservar el NOMBRE que la skill referencia y apuntar
// al endpoint POR-SERVICIO, nunca al agregado. Si esto se rompe, una skill deja
// de encontrar sus herramientas y hay que reescribirla — justo lo que migrate
// existe para evitar.
func TestMigrateTarget_ConservaNombreYUsaPerServicio(t *testing.T) {
	gw := "http://lab:8080"
	cases := []struct {
		mcp, service string
		wantName     string
		wantURL      string
	}{
		// Sin -service: el servicio en kindling se llama igual que el MCP.
		{"context7", "", "context7", "http://lab:8080/mcp/context7"},
		// Con -service distinto: la ENTRADA mantiene el nombre del MCP (lo que ve
		// la skill), aunque el servicio en kindling se llame de otra forma.
		{"context7", "ctx7", "context7", "http://lab:8080/mcp/ctx7"},
		{"my-fetch", "fetch", "my-fetch", "http://lab:8080/mcp/fetch"},
	}
	for _, c := range cases {
		name, url := migrateTarget(c.mcp, c.service, gw)
		if name != c.wantName {
			t.Errorf("migrate %q -service %q: nombre = %q, quería %q (romper esto reescribe la skill)",
				c.mcp, c.service, name, c.wantName)
		}
		if url != c.wantURL {
			t.Errorf("migrate %q -service %q: url = %q, quería %q", c.mcp, c.service, url, c.wantURL)
		}
		// Nunca el agregado: eso renombraría las herramientas y rompería la skill.
		if strings.Contains(url, "/mcp/_all") {
			t.Errorf("migrate no debe usar NUNCA el agregado, dio %q", url)
		}
	}
}

// Barra sobrante en el gateway: no debe duplicarse en la URL.
func TestMigrateTarget_NormalizaBarra(t *testing.T) {
	_, url := migrateTarget("context7", "", "http://lab:8080/")
	if url != "http://lab:8080/mcp/context7" {
		t.Errorf("url = %q; la barra final del gateway no debe duplicarse", url)
	}
}

// La garantía a nivel de fichero: migrar sobre una entrada que YA existe (el MCP
// directo de antes) la reemplaza CONSERVANDO el nombre, así que la skill que la
// referencia sigue resolviendo. Reproduce lo que hace `installConfig` con el
// nombre y la URL que calcula `migrateTarget`.
func TestMigrate_ReemplazaLaEntradaExistenteConservandoNombre(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("PATH", "") // sin CLIs: se ejercita solo el parcheo del fichero

	// "Antes": opencode con context7 apuntando a un MCP directo, más otras cosas
	// del usuario que NO se deben tocar, y una skill que lo referencia.
	previo := `{
	  "mcp": {
	    "context7": {"type": "local", "command": ["npx", "@upstash/context7-mcp"]},
	    "engram": {"type": "local", "command": ["engram", "mcp"]}
	  },
	  "agent": {"mi-skill": {"prompt": "usa context7 para docencia"}}
	}`
	dst := filepath.Join(tmp, ".config", "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(previo), 0o600); err != nil {
		t.Fatal(err)
	}

	// "Migrar": nombre = context7 (el que ve la skill), url = per-servicio de kindling.
	name, url := migrateTarget("context7", "", "http://lab:8080")
	if err := installConfig("opencode", name, url, "tok3n"); err != nil {
		t.Fatalf("installConfig: %v", err)
	}

	var doc map[string]any
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("dejó el fichero ilegible: %v", err)
	}
	mcp, _ := doc["mcp"].(map[string]any)

	// 1) La entrada context7 SIGUE existiendo con ese nombre: la skill no se rompe.
	c7, ok := mcp["context7"].(map[string]any)
	if !ok {
		t.Fatal("desapareció la entrada 'context7': la skill dejaría de encontrar sus herramientas")
	}
	// 2) Y ahora apunta al endpoint por-servicio de kindling.
	if got, _ := c7["url"].(string); got != url {
		t.Errorf("context7.url = %q, quería %q (no reapuntó a kindling)", got, url)
	}
	// 3) NO se creó una entrada 'kindling' ni '_all' (eso rompería las referencias).
	if _, bad := mcp["kindling"]; bad {
		t.Error("migrate no debe crear una entrada 'kindling' agregada")
	}
	// 4) Lo demás del usuario intacto: engram y la skill.
	if _, ok := mcp["engram"]; !ok {
		t.Error("se perdió el MCP 'engram' del usuario")
	}
	if _, ok := doc["agent"]; !ok {
		t.Error("se perdió la sección 'agent' (la skill) del usuario")
	}
}
