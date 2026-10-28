package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Un invitado que responde sin fin no hace que el gateway lo guarde entero:
// pasado maxProxyBody la llamada falla (antes se leía todo a memoria).
func TestMcpCallAtConTope(t *testing.T) {
	trozo := []byte(strings.Repeat("x", 64<<10))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i < (maxProxyBody/len(trozo))+4; i++ {
			if _, err := w.Write(trozo); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	out, err := mcpCallAt(context.Background(), srv.URL+"/mcp", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if err == nil {
		t.Fatalf("mcpCallAt aceptó %d bytes; quería un error de tope", len(out))
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("err = %v, quería el error del tope", err)
	}
}

// Los endpoints agregados (/mcp/_all del gateway y del router) leen la petición
// con tope: un JSON válido pero mayor que maxProxyBody se rechaza con 400 en vez
// de cargarse entero en memoria.
func TestAgregadosLeenLaPeticionConTope(t *testing.T) {
	grande := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":"` +
		strings.Repeat("a", maxProxyBody+1024) + `"}`
	manejadores := map[string]http.HandlerFunc{
		"gateway": (&Gateway{}).handleAggregate,
		"router":  newRouterAgg(NewRouter(nil)).handle,
	}
	for nombre, h := range manejadores {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("POST", "/mcp/_all", strings.NewReader(grande)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: un cuerpo de %d bytes devolvió %d, quería 400", nombre, len(grande), rec.Code)
		}
	}
}
