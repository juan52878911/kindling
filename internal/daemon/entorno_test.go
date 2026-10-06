package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// El entorno de la máquina: uno que no vale, o uno con -from, es un 400 que
// nombra la clave y nunca devuelve el valor.
func TestRunConEntornoCodigos(t *testing.T) {
	t.Setenv("KLING_JAILER", "0")
	s, _ := servidorBlobs(t)
	h := s.routes()
	for _, req := range []api.RunRequest{
		{From: "dorado", Env: []string{"PW=valor-secreto"}},
		{Image: "min", Env: []string{"PW=valor-secreto", "MAL CLAVE=valor-secreto"}},
	} {
		b, _ := json.Marshal(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/machines", bytes.NewReader(b)))
		if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "valor-secreto") {
			t.Errorf("from %q: %d %s", req.From, w.Code, w.Body)
		}
	}
}
