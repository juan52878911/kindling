package daemon

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// DELETE /machines/{ref}/credentials/{env} existe (kling db detach) y se
// anuncia con la capacidad db-attach.
func TestRemoveCredentialRutaYCapacidad(t *testing.T) {
	if !slices.Contains(Capabilities, "db-attach") {
		t.Error(`la capacidad "db-attach" no se anuncia en GET /info`)
	}
	_, h := testServer(t)
	c := clientePara(t, h)
	_, err := c.RemoveCredential(t.Context(), "nada", "PGPASSWORD", "0123456789abcdef")
	var se *api.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("RemoveCredential de algo que no existe: %v", err)
	}
}
