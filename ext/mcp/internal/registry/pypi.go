package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// pypiBase es el índice de paquetes de Python. Variable y no constante para que
// los tests puedan apuntar a un servidor local sin tocar la red.
var pypiBase = "https://pypi.org"

// ResolvePyPIBin averigua con qué comando se arranca un paquete de PyPI.
//
// Existe por la misma razón que ResolveBin para npm: las microVMs arrancan SIN
// salida a internet, así que `uvx <paquete>` fallaría al descargar. El paquete
// se preinstala con pip (-P) y hay que invocar su ejecutable por su nombre.
//
// A diferencia de npm, PyPI NO publica los entry points en su API JSON — el
// equivalente al campo `bin` vive dentro de la rueda, y descargarla para
// leerlo sería pagar la instalación dos veces. Se aplica en su lugar la misma
// convención que usa uvx, que es la norma de facto entre los servidores MCP de
// Python: el ejecutable se llama como la distribución, en su forma normalizada
// (PEP 503). Los que no la siguen los caza el empaquetado, que comprueba que
// el comando exista dentro de la imagen antes de darla por buena.
//
// Sí se consulta la API para confirmar que el paquete EXISTE y obtener su
// nombre canónico: fallar aquí con un 404 claro es mejor que fallar en mitad
// del `pip install` dentro del chroot del daemon.
func ResolvePyPIBin(ctx context.Context, cl *http.Client, pkg string) (string, error) {
	u := pypiBase + "/pypi/" + NormalizePyPI(pkg) + "/json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach PyPI: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("PyPI responded %s for %s", resp.Status, pkg)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}

	// El nombre canónico de PyPI puede diferir del que llegó (mayúsculas,
	// guiones bajos): la convención del ejecutable se aplica sobre el canónico.
	var meta struct {
		Info struct {
			Name string `json:"name"`
		} `json:"info"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return "", fmt.Errorf("PyPI returned something I don't understand: %w", err)
	}
	if meta.Info.Name == "" {
		return "", fmt.Errorf("PyPI has no name for %s", pkg)
	}
	return NormalizePyPI(meta.Info.Name), nil
}

var pypiSeps = regexp.MustCompile(`[-_.]+`)

// NormalizePyPI aplica la normalización de nombres de PEP 503: minúsculas y
// cualquier racha de guiones, guiones bajos o puntos colapsada en un guion.
// "Mcp_Server.Git" y "mcp-server-git" son EL MISMO paquete para pip, y usar la
// forma normalizada evita empaquetar dos veces lo mismo con dos nombres.
func NormalizePyPI(name string) string {
	return pypiSeps.ReplaceAllString(strings.ToLower(name), "-")
}
