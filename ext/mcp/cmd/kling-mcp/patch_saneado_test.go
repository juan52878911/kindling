package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// patchFicheroDe deja un config con `previo` en dir/nombre y devuelve su ruta.
func patchFicheroDe(t *testing.T, dir, nombre, previo string) string {
	t.Helper()
	p := filepath.Join(dir, nombre)
	if err := os.WriteFile(p, []byte(previo), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func leerDoc(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s ilegible: %v", p, err)
	}
	return doc
}

// Un config gestionado con symlink (home-manager, dotfiles) sigue siendo un
// symlink y el cambio cae en su destino.
func TestPatchJSONSigueElSymlink(t *testing.T) {
	dir := t.TempDir()
	real := patchFicheroDe(t, dir, "real.json", `{"mcpServers":{}}`)
	enlace := filepath.Join(dir, "mcp.json")
	if err := os.Symlink(real, enlace); err != nil {
		t.Fatal(err)
	}

	if err := patchJSON(enlace, "mcpServers", "kindling", map[string]any{"url": testURL}, ""); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(enlace)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("sustituyó el symlink por un fichero")
	}
	srv, _ := leerDoc(t, real)["mcpServers"].(map[string]any)
	if _, ok := srv["kindling"]; !ok {
		t.Error("el destino del symlink no tiene la entrada nueva")
	}
}

// La segunda ejecución no pisa el respaldo del original con el ya parcheado.
func TestPatchJSONRespaldoNoSePisa(t *testing.T) {
	dir := t.TempDir()
	previo := `{"mcpServers":{"otro":{"url":"http://ya-estaba"}}}`
	p := patchFicheroDe(t, dir, "mcp.json", previo)

	for _, k := range []string{"uno", "dos"} {
		if err := patchJSON(p, "mcpServers", k, map[string]any{"url": testURL}, ""); err != nil {
			t.Fatal(err)
		}
	}
	bak, err := os.ReadFile(p + ".kling-backup")
	if err != nil {
		t.Fatal(err)
	}
	if string(bak) != previo {
		t.Errorf("el respaldo ya no es el original:\n got  %s\n want %s", bak, previo)
	}
}

// Un número que no cabe en un float64 vuelve tal cual.
func TestPatchJSONConservaNumerosGrandes(t *testing.T) {
	dir := t.TempDir()
	p := patchFicheroDe(t, dir, "mcp.json", `{"id":12345678901234567891,"mcpServers":{}}`)

	if err := patchJSON(p, "mcpServers", "kindling", map[string]any{"url": testURL}, ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "12345678901234567891") {
		t.Errorf("redondeó el número:\n%s", b)
	}
}

// Si el dueño del fichero lo reescribe mientras se parchea, su cambio no se
// pierde: se vuelve a parchear sobre lo nuevo.
func TestPatchJSONNoPisaUnCambioConcurrente(t *testing.T) {
	dir := t.TempDir()
	p := patchFicheroDe(t, dir, "mcp.json", `{"mcpServers":{}}`)

	veces := 0
	antesDeReleer = func(destino string) {
		veces++
		if veces == 1 {
			_ = os.WriteFile(destino, []byte(`{"mcpServers":{},"delCliente":"nuevo"}`), 0o600)
		}
	}
	t.Cleanup(func() { antesDeReleer = nil })

	if err := patchJSON(p, "mcpServers", "kindling", map[string]any{"url": testURL}, ""); err != nil {
		t.Fatal(err)
	}
	doc := leerDoc(t, p)
	if doc["delCliente"] != "nuevo" {
		t.Error("pisó el cambio que el cliente hizo durante el parcheo")
	}
	srv, _ := doc["mcpServers"].(map[string]any)
	if _, ok := srv["kindling"]; !ok {
		t.Error("no escribió la entrada nueva")
	}

	// Y si no deja de cambiar, se rinde sin escribir.
	antesDeReleer = func(destino string) {
		veces++
		_ = os.WriteFile(destino, []byte(fmt.Sprintf(`{"mcpServers":{},"n":%d}`, veces)), 0o600)
	}
	if err := patchJSON(p, "mcpServers", "otra", map[string]any{"url": testURL}, ""); err == nil {
		t.Error("escribió sobre un fichero que no dejaba de cambiar")
	}
}

// Lo que manda el servidor MCP (serverInfo, nombres y descripciones de
// herramientas) no llega a la terminal con caracteres de control.
func TestServidorMCPSaleSaneado(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"eco\u001b]0;x\u0007","version":"1\u009b2"}}}`)
		case "tools/list":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"leer\u001b[2J"}]}}`)
		}
	}))
	defer srv.Close()

	info, tools, err := probeMCP(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append([]string{info}, tools...) {
		if strings.ContainsAny(s, "\x1b\x07\u009b") {
			t.Errorf("sale con caracteres de control: %q", s)
		}
	}

	if got := columna("abc\x1b[31mdéf", 100); strings.ContainsRune(got, 0x1b) {
		t.Errorf("columna deja pasar el ESC: %q", got)
	}
	// El corte cuenta caracteres, no bytes: no parte uno UTF-8.
	if got := columna(strings.Repeat("é", 80), 68); len([]rune(got)) != 68 || strings.ContainsRune(got, '�') {
		t.Errorf("corte mal hecho: %q", got)
	}
}
