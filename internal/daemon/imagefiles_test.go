package daemon

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Todo lo que se valida antes de tocar ninguna imagen: el origen no puede salir
// del directorio de librerías, el contenido tiene tope y los permisos no pueden
// llevar setuid. Montar la imagen necesita root y queda para el laboratorio.
func TestPutImageFileValida(t *testing.T) {
	s, h := testServer(t)
	lib := t.TempDir()
	t.Setenv("KLING_LIB_DIR", lib)
	os.WriteFile(filepath.Join(lib, "kling-bridge"), []byte("bin"), 0o755)
	grande := base64.StdEncoding.EncodeToString(make([]byte, maxImageFileUpload+1))

	casos := []struct {
		body, quiere string
		code         int
	}{
		{`{"path":"/x"}`, "missing from_host or content_b64", 400},
		{`{"path":"/x","from_host":"kling-bridge","content_b64":"eA=="}`, "not both", 400},
		{`{"path":"/x","from_host":"../etc/shadow"}`, "relative to", 400},
		{`{"path":"/x","from_host":"/etc/shadow"}`, "relative to", 400},
		{`{"path":"/x","from_host":"no-existe"}`, "is not a file", 400},
		{`{"path":"/x","content_b64":"no es base64!"}`, "content_b64", 400},
		{`{"path":"/x","content_b64":"` + grande + `"}`, "the limit is", 413},
		{`{"path":"/x","from_host":"kling-bridge","mode":"4755"}`, "no setuid", 400},
		{`{"path":"relativa","from_host":"kling-bridge"}`, "must be absolute", 400},
		{`{"path":"/","from_host":"kling-bridge"}`, "invalid path", 400},
		// Se limpia dentro de la raíz del invitado: /usr/../../x es /x, no fuera.
		{`{"path":"/usr/../../x","from_host":"kling-bridge"}`, "does not exist", 400},
		{`{"path":"/x","from_host":"kling-bridge"}`, "does not exist", 400}, // la imagen "img" no existe
	}
	for _, c := range casos {
		rr := call(t, h, "PUT", "/images/img/files", c.body)
		if rr.Code != c.code || !strings.Contains(rr.Body.String(), c.quiere) {
			t.Errorf("%.60s: %d %s — quería %d con %q", c.body, rr.Code, strings.TrimSpace(rr.Body.String()), c.code, c.quiere)
		}
	}
	// El fichero temporal del contenido inline no se queda en el disco.
	if left, _ := filepath.Glob(filepath.Join(s.root, "build", "imagefile-*")); len(left) > 0 {
		t.Fatalf("quedó el temporal: %v", left)
	}
}
