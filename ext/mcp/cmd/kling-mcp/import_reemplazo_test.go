package main

import (
	"errors"
	"net/http"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un commit -replace que se niega por instancias vivas se reintenta y, si
// sigue, se dice que se repita; "already exists" y otros fallos no cuentan.
func TestEnUsoAlReemplazar(t *testing.T) {
	casos := []struct {
		err  error
		uso  bool
		caso string
	}{
		{&api.StatusError{Code: http.StatusConflict, Message: `snapshot "svc" has 1 live instance(s) ([svc-1])`}, true, "instancias vivas"},
		{&api.StatusError{Code: http.StatusConflict, Message: `snapshot "svc" is in use right now`}, true, "en uso"},
		{&api.StatusError{Code: http.StatusConflict, Message: `snapshot "svc" already exists`}, false, "ya existe"},
		{&api.StatusError{Code: http.StatusInternalServerError, Message: "disk full"}, false, "otro fallo"},
		{errors.New("connection refused"), false, "sin respuesta"},
	}
	for _, c := range casos {
		if got := enUsoAlReemplazar(c.err); got != c.uso {
			t.Errorf("%s: %v, quería %v", c.caso, got, c.uso)
		}
	}
}
