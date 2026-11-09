package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
)

// El gateway lee el resultado de una herramienta con el MISMO tope con el que
// el puente lo deja salir del servidor MCP (mcp.MaxToolResult). Con 8 MiB aquí
// y 16 allí, un resultado de 10 MiB pasaba el puente y moría en el gateway.
func TestResultadoGrandeCabeEnElGateway(t *testing.T) {
	grande := strings.Repeat("x", 10<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":%q}]}}`, grande)
	}))
	t.Cleanup(srv.Close)

	raw, err := mcpCallAt(t.Context(), srv.URL, "s", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`)
	if err != nil {
		t.Fatalf("resultado de 10 MiB rechazado: %v", err)
	}
	if len(raw) < 10<<20 {
		t.Fatalf("resultado recortado a %d bytes", len(raw))
	}

	// Pasado el tope sí falla, sin truncar.
	srvEnorme := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + strings.Repeat("x", mcp.MaxToolResult) + `"}`))
	}))
	t.Cleanup(srvEnorme.Close)
	if _, err := mcpCallAt(t.Context(), srvEnorme.URL, "s", `{}`); err == nil {
		t.Fatal("un resultado mayor que mcp.MaxToolResult tenía que fallar")
	}
}
