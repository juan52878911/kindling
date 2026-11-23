package aigw

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// sumidero es un servidor de mentira que lee el cuerpo entero sin guardarlo y
// contesta un JSON fijo: lo que cuesta es solo el camino del gateway.
func sumidero(tb testing.TB) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	tb.Cleanup(srv.Close)
	return srv
}

// cuerpoOpenAI es una petición de chat con un mensaje de n bytes.
func cuerpoOpenAI(n int) []byte {
	return []byte(`{"model":"smol","messages":[{"role":"user","content":"` + strings.Repeat("a", n) + `"}]}`)
}

func benchHandler(b *testing.B, h http.Handler, path string, body []byte) {
	b.Helper()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			b.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
}

// BenchmarkProxyOpenAI mide el proxy OpenAI de hoy: lee el cuerpo entero, lo
// decodifica a un mapa y lo vuelve a serializar, así que asigna en proporción
// al cuerpo.
func BenchmarkProxyOpenAI(b *testing.B) {
	for _, n := range []int{1 << 10, 512 << 10} {
		b.Run(tamano(n), func(b *testing.B) {
			srv := sumidero(b)
			g, err := New(Options{
				Config:   &Config{Models: map[string]*ModelConfig{"smol": {Kind: KindVON, Snapshot: "von-smol"}}},
				Replicas: &fakeReplicas{addr: strings.TrimPrefix(srv.URL, "http://")},
				EvalDir:  b.TempDir(),
			})
			if err != nil {
				b.Fatal(err)
			}
			benchHandler(b, g.Handler(""), "/v1/chat/completions", cuerpoOpenAI(n))
		})
	}
}

func tamano(n int) string { return strconv.Itoa(n>>10) + "KiB" }
