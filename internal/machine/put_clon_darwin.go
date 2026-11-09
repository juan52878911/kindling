//go:build darwin

package machine

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// clonImagenPut clona image (APFS, clonefile: sin copiar bloques) a un
// temporal a su lado, donde intentarPutDebugfs edita sin tocar la imagen.
// Devuelve "" si no se puede clonar (otro sistema de ficheros): entonces se
// edita en su sitio, como antes.
func clonImagenPut(image string) string {
	in, err := os.Open(image)
	if err != nil {
		return ""
	}
	defer in.Close()
	tmp := filepath.Join(filepath.Dir(image), fmt.Sprintf(".%s.put-%d", filepath.Base(image), time.Now().UnixNano()))
	out, err := clonarADestino(in, tmp)
	if err != nil {
		return ""
	}
	out.Close()
	return tmp
}
