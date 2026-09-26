package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// npmBase es el registro de paquetes de node.
const npmBase = "https://registry.npmjs.org"

// ResolveBin averigua con qué comando se arranca un paquete npm.
//
// Hace falta porque las microVMs arrancan SIN salida a internet: `npx -y <pkg>`
// intentaría descargar y fallaría. El paquete se preinstala en la imagen con
// `-n` y luego hay que invocar su ejecutable por su nombre real, que no tiene
// por qué parecerse al del paquete: @modelcontextprotocol/server-filesystem
// instala un binario llamado `mcp-server-filesystem`.
//
// Ese dato solo está en el campo `bin` del package.json, así que se pregunta al
// registro de npm.
func ResolveBin(ctx context.Context, cl *http.Client, pkg string) (string, error) {
	name, _ := SplitVersion(pkg)

	// Los paquetes con ámbito llevan la barra codificada en la ruta.
	u := npmBase + "/" + strings.ReplaceAll(url.PathEscape(name), "%2F", "/") + "/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach the npm registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("npm responded %s for %s", resp.Status, name)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}

	// `bin` es un string cuando hay un solo ejecutable y un objeto cuando hay
	// varios. Hay que aceptar las dos formas.
	var meta struct {
		Name string          `json:"name"`
		Bin  json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return "", err
	}

	var single string
	if json.Unmarshal(meta.Bin, &single) == nil && single != "" {
		// bin como string: el ejecutable se llama como el paquete, sin ámbito.
		if _, last, ok := strings.Cut(name, "/"); ok {
			return last, nil
		}
		return name, nil
	}

	var multi map[string]string
	if err := json.Unmarshal(meta.Bin, &multi); err != nil || len(multi) == 0 {
		return "", fmt.Errorf("%s declares no executable, so I don't know how to start it.\n"+
			"Package it by hand with scripts/80-mcp-image.sh specifying the command", name)
	}
	if len(multi) == 1 {
		for k := range multi {
			return k, nil
		}
	}

	// Varios ejecutables: se prefiere el que se llama como el paquete, que es
	// la convención. Si no, se ordena para que la elección sea reproducible y
	// no dependa del recorrido aleatorio del mapa.
	short := name
	if _, last, ok := strings.Cut(name, "/"); ok {
		short = last
	}
	if _, ok := multi[short]; ok {
		return short, nil
	}
	keys := make([]string, 0, len(multi))
	for k := range multi {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0], nil
}

// SplitVersion separa "paquete@version", respetando el ámbito de los paquetes
// con arroba inicial: "@scope/pkg@1.2.3" -> "@scope/pkg", "1.2.3".
func SplitVersion(pkg string) (string, string) {
	at := strings.LastIndex(pkg, "@")
	if at <= 0 {
		return pkg, ""
	}
	return pkg[:at], pkg[at+1:]
}
