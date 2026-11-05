package api

import (
	"context"
	"errors"
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
		{"invitado lleno con code", IsGuestBusy, &StatusError{Code: 429, Message: "x", ErrorCode: CodeGuestBusy}, true},
		{"invitado lleno sin code", IsGuestBusy, &StatusError{Code: 429, Message: "guest agent: too many shells open in this microVM (limit 8)"}, true},
		{"cuota no es invitado lleno", IsGuestBusy, &StatusError{Code: 429, Message: "x", ErrorCode: CodeTenantQuota}, false},
		{"invitado lleno no es cuota", IsTenantQuota, &StatusError{Code: 429, Message: "x", ErrorCode: CodeGuestBusy}, false},
		{"502 con el texto no es invitado lleno", IsGuestBusy, &StatusError{Code: 502, Message: "guest agent: too many commands running in this microVM (limit 64)", ErrorCode: CodeBadGateway}, false},
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

// El gateway y el scheduler escriben sus errores con WriteError/HTTPError: el
// cliente tiene que leerles el code igual que al daemon.
func TestWriteErrorLoLeeElCliente(t *testing.T) {
	for _, c := range []struct {
		escribir func(http.ResponseWriter)
		status   int
		code     ErrorCode
		cuota    bool
	}{
		{func(w http.ResponseWriter) {
			WriteError(w, StatusTenantQuota, CodeTenantQuota, `tenant "a" has reached its quota`)
		}, 429, CodeTenantQuota, true},
		{func(w http.ResponseWriter) { HTTPError(w, "tool \"x\" did not respond", http.StatusBadGateway) }, 502, CodeBadGateway, false},
		{func(w http.ResponseWriter) { HTTPError(w, "missing Authorization header", http.StatusUnauthorized) }, 401, CodeUnauthorized, false},
		// Un 429 sin code propio no es la cuota del inquilino.
		{func(w http.ResponseWriter) { HTTPError(w, "slow down", http.StatusTooManyRequests) }, 429, CodeTooManyRequests, false},
	} {
		cl := clienteDePrueba(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { c.escribir(w) }))
		_, err := cl.Get(context.Background(), "x")
		var se *StatusError
		if !errors.As(err, &se) || se.Code != c.status || CodeOf(err) != c.code || IsTenantQuota(err) != c.cuota {
			t.Errorf("%d %s: %#v", c.status, c.code, err)
		}
	}
}

func TestCodeForStatus(t *testing.T) {
	for st, want := range map[int]ErrorCode{400: CodeBadRequest, 404: CodeNotFound, 409: CodeConflict, 500: CodeInternal, 507: CodeInsufficientMemory, 200: "", 418: ""} {
		if got := CodeForStatus(st); got != want {
			t.Errorf("%d: %q, quería %q", st, got, want)
		}
	}
}
