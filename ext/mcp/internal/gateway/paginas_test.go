package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// El catálogo sigue nextCursor: 4 herramientas en 2 páginas son 4, no 2; y un
// cursor sin fin es un error, no un bucle.
func TestListToolsSiguePaginas(t *testing.T) {
	sinFin := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case sinFin:
			fmt.Fprint(w, `{"result":{"tools":[{"name":"x"}],"nextCursor":"siempre"}}`)
		case req.Params.Cursor == "":
			fmt.Fprint(w, `{"result":{"tools":[{"name":"a"},{"name":"b"}],"nextCursor":"p2"}}`)
		default:
			fmt.Fprint(w, `{"result":{"tools":[{"name":"c"},{"name":"d"}]}}`)
		}
	}))
	defer srv.Close()

	tools, err := listTools(context.Background(), srv.URL, "sid")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 4 || tools[2].Name != "c" {
		t.Fatalf("tools = %+v, want a b c d", tools)
	}
	sinFin = true
	if _, err := listTools(context.Background(), srv.URL, "sid"); err == nil {
		t.Error("an endless cursor was accepted")
	}
}
