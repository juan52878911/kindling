package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// fail pone en el cuerpo el code estable de la causa (capacidad
// "error-codes"), sin tocar message: los específicos solo con su código HTTP
// documentado, y si no el genérico.
func TestFailPoneElCodigo(t *testing.T) {
	for _, c := range []struct {
		status int
		err    error
		code   int
		quiero api.ErrorCode
	}{
		{500, &api.StatusError{Code: api.StatusMachineLimit, Message: "machine limit reached: 5 of 5"}, 409, api.CodeMachineLimit},
		{500, &api.StatusError{Code: api.StatusTenantQuota, Message: "tenant a: " + api.TenantQuotaMark + " (machines 2/2)"}, 429, api.CodeTenantQuota},
		{500, &api.StatusError{Code: api.StatusDiskFull, Message: "only 10 MiB of disk left under /x"}, 503, api.CodeDiskFull},
		{500, &api.StatusError{Code: api.StatusInsufficientMemory, Message: "no room"}, 507, api.CodeInsufficientMemory},
		{409, fmt.Errorf("%w: run -from x", machine.ErrDoradoObsoleto), 409, api.CodeStaleGolden},
		{409, fmt.Errorf("%q: %w", "a", machine.ErrNameTaken), 409, api.CodeNameTaken},
		{409, errors.New("graph is busy"), 409, api.CodeConflict},
		// La causa con otro código HTTP no es la específica.
		{400, machine.ErrDoradoObsoleto, 400, api.CodeBadRequest},
		{503, errors.New("draining"), 503, api.CodeUnavailable},
		{404, errors.New("nope"), 404, api.CodeNotFound},
	} {
		rr := httptest.NewRecorder()
		fail(rr, c.status, c.err)
		var e api.Error
		if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if rr.Code != c.code || e.Code != c.quiero || e.Message != c.err.Error() {
			t.Errorf("%v: %d %+v, quería %d %q", c.err, rr.Code, e, c.code, c.quiero)
		}
	}
}
