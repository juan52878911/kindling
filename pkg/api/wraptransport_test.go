package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// WrapTransport envuelve el cliente normal (Info) y el de operaciones largas.
func TestWrapTransport(t *testing.T) {
	c := daemonConAPI(t, "", "v")
	var n atomic.Int32
	c.WrapTransport(func(rt http.RoundTripper) http.RoundTripper {
		return rtFunc(func(r *http.Request) (*http.Response, error) {
			n.Add(1)
			r.Header.Set("Traceparent", "x")
			return rt.RoundTrip(r)
		})
	})
	c.WrapTransport(nil) // no-op
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 1 {
		t.Fatalf("el envoltorio vio %d peticiones", n.Load())
	}
	req, _ := http.NewRequest("GET", "http://kling/info", nil)
	resp, err := c.long.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n.Load() != 2 {
		t.Fatalf("el cliente largo no pasó por el envoltorio: %d", n.Load())
	}
}
