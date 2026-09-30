package mcp

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// Ocho SetLink a la vez no pierden ninguno: mcp/links es un documento entero
// que se lee, se cambia y se escribe, y sin cerrojo cada uno pisaba a los
// demás (medido: 2 de 9).
func TestSetLinkConcurrenteNoPierdeEnlaces(t *testing.T) {
	_, c := newFakeDaemon(t, true)
	ctx := context.Background()
	if _, err := SetLink(ctx, c, &Link{Name: "base", URL: "http://x/mcp"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := SetLink(ctx, c, &Link{Name: fmt.Sprintf("l%d", i), URL: "http://x/mcp"}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	ls, err := Links(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 9 {
		t.Fatalf("links = %d, want 9 (base + 8)", len(ls))
	}
}
