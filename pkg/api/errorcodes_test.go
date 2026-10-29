package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// Las Is* miran code cuando el daemon lo manda, aunque el texto cambie; con
// un daemon anterior (sin code) caen al texto de siempre.
func TestIsMiraElCodigoYCaeAlTexto(t *testing.T) {
	for _, c := range []struct {
		nombre string
		es     func(error) bool
		err    *StatusError
		quiero bool
	}{
		{"tope con code y otro texto", IsMachineLimit, &StatusError{Code: 409, Message: "full", ErrorCode: CodeMachineLimit}, true},
		{"tope sin code, por el texto", IsMachineLimit, &StatusError{Code: 409, Message: "machine limit reached (5)"}, true},
		{"409 con otro code y el texto", IsMachineLimit, &StatusError{Code: 409, Message: "machine limit", ErrorCode: CodeConflict}, false},
		{"cuota con code", IsTenantQuota, &StatusError{Code: 429, Message: "x", ErrorCode: CodeTenantQuota}, true},
		{"cuota sin code", IsTenantQuota, &StatusError{Code: 429, Message: "tenant a: " + TenantQuotaMark}, true},
		{"429 ajeno", IsTenantQuota, &StatusError{Code: 429, Message: "slow down", ErrorCode: CodeTooManyRequests}, false},
		{"disco con code", IsDiskFull, &StatusError{Code: 503, Message: "x", ErrorCode: CodeDiskFull}, true},
		{"503 ajeno", IsDiskFull, &StatusError{Code: 503, Message: "x", ErrorCode: CodeUnavailable}, false},
		{"memoria sin code", IsInsufficientMemory, &StatusError{Code: 507, Message: "x"}, true},
		{"nombre con code", IsNameTaken, &StatusError{Code: 409, Message: "x", ErrorCode: CodeNameTaken}, true},
		{"nombre sin code", IsNameTaken, &StatusError{Code: 409, Message: `"a": machine name is taken`}, true},
		{"dorado con code", IsStaleGolden, &StatusError{Code: 409, Message: "x", ErrorCode: CodeStaleGolden}, true},
		{"dorado sin code", IsStaleGolden, &StatusError{Code: 409, Message: "stale golden snapshot: vz"}, true},
		{"dorado con el código HTTP que no es", IsStaleGolden, &StatusError{Code: 400, Message: "x", ErrorCode: CodeStaleGolden}, false},
	} {
		if got := c.es(fmt.Errorf("envuelto: %w", c.err)); got != c.quiero {
			t.Errorf("%s: %v, quería %v", c.nombre, got, c.quiero)
		}
	}
	if CodeOf(fmt.Errorf("x: %w", &StatusError{Code: 404, ErrorCode: CodeNotFound})) != CodeNotFound || CodeOf(fmt.Errorf("x")) != "" {
		t.Error("CodeOf")
	}
}

// El cliente guarda el code del cuerpo en StatusError.ErrorCode.
func TestClienteLeeElCodigo(t *testing.T) {
	c := clienteDePrueba(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"lo que sea","code":"machine_limit"}`))
	}))
	_, err := c.Get(context.Background(), "x")
	if !IsMachineLimit(err) || CodeOf(err) != CodeMachineLimit || err.Error() != "lo que sea" {
		t.Fatalf("%#v", err)
	}
}

func TestCodeForStatus(t *testing.T) {
	for st, want := range map[int]ErrorCode{400: CodeBadRequest, 404: CodeNotFound, 409: CodeConflict, 500: CodeInternal, 507: CodeInsufficientMemory, 200: "", 418: ""} {
		if got := CodeForStatus(st); got != want {
			t.Errorf("%d: %q, quería %q", st, got, want)
		}
	}
}
