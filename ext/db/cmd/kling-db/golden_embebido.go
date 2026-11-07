package main

// db-golden.sh va dentro del binario: instalado con `kling plugin install db`
// (o `kling upgrade`) no hay checkout ni share/kindling al lado, y sin el
// script `kling db golden` y `kling db up` sin plantilla no funcionaban fuera
// del repositorio. La copia de embebido/ es idéntica a scripts/db-golden.sh
// (lo comprueba TestGuionEmbebidoIgualAlDelRepo); `go generate` la refresca.
// Solo va este: los de otros motores (-engine mysql...) y `golden image`
// siguen necesitando el checkout, y el error lo dice.

//go:generate cp ../../../../scripts/db-golden.sh embebido/db-golden.sh

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed embebido/db-golden.sh
var guionGolden []byte

// guionEmbebido escribe el script embebido en un directorio temporal propio
// (0700) y devuelve su ruta y cómo borrarlo.
func guionEmbebido() (string, func(), error) {
	dir, err := os.MkdirTemp("", "kling-db-golden-")
	if err != nil {
		return "", func() {}, fmt.Errorf("writing the built-in db-golden.sh: %w", err)
	}
	borrar := func() { os.RemoveAll(dir) }
	p := filepath.Join(dir, "db-golden.sh")
	if err := os.WriteFile(p, guionGolden, 0o700); err != nil {
		borrar()
		return "", func() {}, fmt.Errorf("writing the built-in db-golden.sh: %w", err)
	}
	return p, borrar, nil
}
