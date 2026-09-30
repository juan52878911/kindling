package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/plugin"
)

// `kling mcp heal` con el único servicio caído y la salud sin poder grabarse:
// antes decía "all healthy" y salía con 0.
func TestHealNoDiceTodoSanoConElServicioCaido(t *testing.T) {
	sock := daemonRoto(t, `[{"name":"svc"}]`, http.StatusForbidden)
	var err error
	out, _ := capturarSalida(t, func() { err = mcpHeal([]string{"-H", sock, "-wait", "1s", "-wait-daemon", "2s"}) })
	if strings.Contains(out, "all healthy") {
		t.Errorf("heal says all healthy:\n%s", out)
	}
	if err == nil {
		t.Fatalf("heal returned nil:\n%s", out)
	}
	var ee *plugin.ExitError
	if errors.As(err, &ee) && ee.Code == 3 {
		t.Errorf("a record failure must not be exit 3 (treated as success by the unit): %v", err)
	}
}
