package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// Chocar con el tope de réplicas es un 503 ("lleno, reintenta"), no un 502, y
// NO marca el servicio como roto: está sano, solo que no caben más sesiones.
func TestTopeDeReplicasEs503YNoAnotaFallo(t *testing.T) {
	g := New(nil, 0, false, 0, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/echo", nil)
	err := fmt.Errorf("%w: service %q has 16 instance(s) awake or starting (max 16)", scheduler.ErrMaxReplicas, "echo")
	g.newSessionError(rec, req, "echo", err)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, quería 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "all replicas full") {
		t.Errorf("cuerpo = %q, quería que dijera 'all replicas full'", rec.Body.String())
	}
	g.saludMu.Lock()
	_, anotado := g.saludVista["echo"]
	g.saludMu.Unlock()
	if anotado {
		t.Error("el tope de réplicas anotó la salud del servicio: está lleno, no roto")
	}

	// Un error cualquiera sigue siendo 502 y sí se anota.
	rec = httptest.NewRecorder()
	g.newSessionError(rec, req, "echo", errors.New("restore failed"))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("error genérico: code = %d, quería 502", rec.Code)
	}
}

// Los errores del gateway llevan el cuerpo del daemon, {"message","code"}: un
// cliente distingue la cuota del inquilino de un 429 cualquiera por el code,
// no por la frase.
func TestErroresDelGatewayLlevanCodeEstable(t *testing.T) {
	g := New(nil, 0, false, 0, "")
	req := httptest.NewRequest(http.MethodPost, "/mcp/echo", nil)
	for _, c := range []struct {
		err    error
		status int
		code   api.ErrorCode
	}{
		{fmt.Errorf("%w: tenant t has 2 instance(s)", scheduler.ErrTenantInstances), http.StatusTooManyRequests, api.CodeTenantQuota},
		{fmt.Errorf("%w: max 16", scheduler.ErrMaxReplicas), http.StatusServiceUnavailable, api.CodeUnavailable},
		{errors.New("restore failed"), http.StatusBadGateway, api.CodeBadGateway},
	} {
		rec := httptest.NewRecorder()
		g.newSessionError(rec, req, "echo", c.err)
		var e api.Error
		if rec.Code != c.status || json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Code != c.code || e.Message == "" {
			t.Errorf("%v: %d %q; quería %d con code %s", c.err, rec.Code, rec.Body.String(), c.status, c.code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%v: Content-Type %q", c.err, ct)
		}
	}
}
