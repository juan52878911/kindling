package guest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Con las plazas de exec llenas, /exec y /exec/stream contestan 429 sin
// lanzar nada; al acabar uno, la plaza vuelve.
func TestExecTopeDePlazas(t *testing.T) {
	execLim.mu.Lock()
	viejo := execLim.max
	execLim.max = 1
	execLim.mu.Unlock()
	defer func() {
		execLim.mu.Lock()
		execLim.max = viejo
		execLim.mu.Unlock()
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/exec", ExecHandler(nil))
	mux.HandleFunc("/exec/stream", StreamExecHandler(nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(ctx context.Context, path, body string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+path, strings.NewReader(body))
		return http.DefaultClient.Do(req)
	}

	// Uno que ocupa la plaza hasta que se cancela.
	ctx, cancel := context.WithCancel(context.Background())
	hecho := make(chan struct{})
	go func() {
		defer close(hecho)
		if resp, err := post(ctx, "/exec", `{"cmd":["sleep","30"]}`); err == nil {
			resp.Body.Close()
		}
	}()
	waitUntil(t, "the first exec to take its slot", func() bool {
		execLim.mu.Lock()
		defer execLim.mu.Unlock()
		return execLim.n == 1
	})

	for _, c := range []struct{ path, body string }{
		{"/exec", `{"cmd":["true"]}`},
		{"/exec/stream", `{"cmd":["true"]}`},
	} {
		resp, err := post(context.Background(), c.path, c.body)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("%s with no slots: %d, want 429", c.path, resp.StatusCode)
		}
	}

	cancel()
	<-hecho
	waitUntil(t, "the slot to come back", func() bool {
		execLim.mu.Lock()
		defer execLim.mu.Unlock()
		return execLim.n == 0
	})
	resp, err := post(context.Background(), "/exec", `{"cmd":["true"]}`)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after freeing the slot: %d", resp.StatusCode)
	}
}
