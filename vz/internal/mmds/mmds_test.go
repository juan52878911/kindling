package mmds

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func do(t *testing.T, h http.Handler, method, path string, hdr map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b)
}

func TestTokenFlow(t *testing.T) {
	s := NewStore()
	s.Put(map[string]any{"env": map[string]any{"A": "1"}, "sessions": map[string]any{}})
	h := s.Handler()

	if code, _ := do(t, h, "GET", "/", nil); code != http.StatusUnauthorized {
		t.Fatalf("read without token: %d, want 401", code)
	}
	if code, _ := do(t, h, "GET", "/", map[string]string{"X-metadata-token": "forged"}); code != http.StatusUnauthorized {
		t.Fatalf("read with a forged token: %d", code)
	}
	if code, _ := do(t, h, "PUT", "/latest/api/token", nil); code != http.StatusBadRequest {
		t.Fatalf("token without ttl: %d, want 400", code)
	}
	for _, ttl := range []string{"0", "21601", "x"} {
		if code, _ := do(t, h, "PUT", "/latest/api/token", map[string]string{"X-metadata-token-ttl-seconds": ttl}); code != http.StatusBadRequest {
			t.Fatalf("ttl %s: %d, want 400", ttl, code)
		}
	}
	if code, _ := do(t, h, "PUT", "/latest/api/token", map[string]string{
		"X-metadata-token-ttl-seconds": "60", "X-Forwarded-For": "1.2.3.4"}); code != http.StatusBadRequest {
		t.Fatalf("forwarded token request: %d, want 400", code)
	}
	code, tok := do(t, h, "PUT", "/latest/api/token", map[string]string{"X-metadata-token-ttl-seconds": "60"})
	if code != http.StatusOK || tok == "" {
		t.Fatalf("token: %d %q", code, tok)
	}
	// Lo mismo que hace pkg/guest/mmds.go: GET / con Accept JSON.
	code, body := do(t, h, "GET", "/", map[string]string{"X-metadata-token": tok, "Accept": "application/json"})
	if code != http.StatusOK {
		t.Fatalf("read: %d %s", code, body)
	}
	var got struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Env["A"] != "1" {
		t.Fatalf("store = %s (%v)", body, err)
	}
	if code, _ := do(t, h, "POST", "/", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", code)
	}
}

func TestTokenExpiresAndIMDSFormat(t *testing.T) {
	now := time.Unix(0, 0)
	s := NewStore()
	s.now = func() time.Time { return now }
	s.Put(map[string]any{"env": map[string]any{"A": "1"}, "name": "vm", "n": 3.0})
	h := s.Handler()
	_, tok := do(t, h, "PUT", "/latest/api/token", map[string]string{"X-metadata-token-ttl-seconds": "10"})
	auth := map[string]string{"X-metadata-token": tok}

	if code, body := do(t, h, "GET", "/", auth); code != 200 || body != "env/\nn\nname" {
		t.Fatalf("listing = %d %q", code, body)
	}
	if code, body := do(t, h, "GET", "/env/A", auth); code != 200 || body != "1" {
		t.Fatalf("leaf = %d %q", code, body)
	}
	if code, _ := do(t, h, "GET", "/n", auth); code != http.StatusBadRequest {
		t.Fatalf("a number has no IMDS representation: %d", code)
	}
	if code, _ := do(t, h, "GET", "/missing", auth); code != http.StatusNotFound {
		t.Fatalf("missing path: %d", code)
	}
	now = now.Add(11 * time.Second)
	if code, _ := do(t, h, "GET", "/", auth); code != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", code)
	}
}

func TestTokenCap(t *testing.T) {
	s := NewStore()
	for i := 0; i < maxTokens; i++ {
		if _, ok := s.newToken(time.Hour); !ok {
			t.Fatalf("token %d refused", i)
		}
	}
	if _, ok := s.newToken(time.Hour); ok {
		t.Fatal("the token map must be capped")
	}
}

func TestMergePatch(t *testing.T) {
	s := NewStore()
	s.Put(map[string]any{"env": map[string]any{"A": "1", "B": "2"}, "keep": "x"})
	var patch any
	_ = json.NewDecoder(strings.NewReader(`{"env": {"A": null, "C": "3"}}`)).Decode(&patch)
	s.Patch(patch)
	b, _ := json.Marshal(s.Get())
	if string(b) != `{"env":{"B":"2","C":"3"},"keep":"x"}` {
		t.Fatalf("after patch: %s", b)
	}
}
