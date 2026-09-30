package registry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// registroFalso pagina: cada página es una lista de nombres.
func registroFalso(t *testing.T, paginas ...[]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			fmt.Sscanf(c, "p%d", &i)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"servers":[`)
		for j, n := range paginas[i] {
			if j > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"server":{"name":%q}}`, n)
		}
		next := ""
		if i+1 < len(paginas) {
			next = fmt.Sprintf("p%d", i+1)
		}
		fmt.Fprintf(w, `],"metadata":{"nextCursor":%q}}`, next)
	}))
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, HTTP: srv.Client()}
}

// Un nombre corto con un solo final en la página 1 y otro de otro autor en la
// página 2 NO es inequívoco: antes se instalaba el de la página 1.
func TestGetRecorreTodasLasPaginas(t *testing.T) {
	rc := registroFalso(t,
		[]string{"io.github.a/github-tools", "com.mcparmory/github", "io.github.b/gh"},
		[]string{"io.github.c/github-issues", "io.github.Abhishekkumar2021/github"},
	)
	srv, cands, err := rc.Get(context.Background(), "github", 3)
	if err == nil {
		t.Fatalf("Get(github) = %s, want an ambiguity error", srv.Name)
	}
	if len(cands) != 2 {
		t.Errorf("candidates = %v, want the two */github", cands)
	}

	// Con un único final en todas las páginas, sí se resuelve.
	rc = registroFalso(t, []string{"io.github.a/github-tools"}, []string{"com.x/filesystem"})
	srv, _, err = rc.Get(context.Background(), "filesystem", 1)
	if err != nil || srv.Name != "com.x/filesystem" {
		t.Fatalf("Get(filesystem) = %v, %v", srv, err)
	}
}
