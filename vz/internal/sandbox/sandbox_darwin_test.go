//go:build darwin

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/vz/internal/server"
)

func TestParametros(t *testing.T) {
	d := t.TempDir()
	k, o := filepath.Join(d, "k"), filepath.Join(d, "o")
	for _, f := range []string{k, o} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	real, _ := filepath.EvalSymlinks(d)
	c := server.Confinamiento{ConRed: true, Lectura: []string{k}, Escritura: []string{o}}
	pares, err := Parametros("/r", "/r/m", "", c)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for i := 0; i+1 < len(pares); i += 2 {
		m[pares[i]] = pares[i+1]
	}
	// Todos los parámetros presentes (el perfil los lee sin comprobar), las
	// rutas reales (el TempDir de macOS cuelga de /var, que es /private/var)
	// y un broker que no abre nada.
	if m["NET"] != "1" || m["LOOP"] != "0" || m["L0"] != filepath.Join(real, "k") || m["E0"] != filepath.Join(real, "o") ||
		m["L15"] != "" || !strings.HasPrefix(m["BROKER"], "/dev/null/") {
		t.Fatalf("parámetros: %v", m)
	}
	c.Lectura = make([]string, MaxFicheros+1)
	if _, err := Parametros("/r", "/r/m", "", c); err == nil {
		t.Fatal("más ficheros de los que admite el perfil")
	}
}
