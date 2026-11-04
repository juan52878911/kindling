// Package digest calcula el hash de ficheros.
//
// Existía por separado en internal/machine (fileSHA256, fileDigest),
// internal/machine/volcado.go (sha256Fichero) e internal/daemon/blobs.go
// (sha256File): cuatro copias idénticas de "abrir, sha256, io.Copy, hex". Un
// solo sitio para arreglarlo si algún día hace falta (por ejemplo, para no
// cargar el fichero entero en páginas de la cache si es muy grande).
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// File devuelve el sha256 de path en hexadecimal.
func File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
