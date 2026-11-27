package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// pagina es el tamaño con el que se recorre la memoria del falso: la página
// base de x86_64 y de arm64 con páginas de 4 KiB.
const pagina = 4096

// Falso es un servidor HTTP de mentira con el tamaño de memoria que se le
// pida: ocupa memMiB MiB de páginas anónimas no nulas (las «pesos») y, con
// tocar, cada petición las lee todas, como una inferencia que recorre el
// modelo. Sirve para medir el reparto de páginas entre réplicas y la primera
// petición tras restaurar sin bajar un GGUF.
type Falso struct {
	mem        []byte
	tocar      bool
	retardo    time.Duration
	peticiones atomic.Int64
}

func nuevoFalso(memMiB int, tocar bool, retardo time.Duration) *Falso {
	f := &Falso{mem: make([]byte, memMiB<<20), tocar: tocar, retardo: retardo}
	// Contenido determinista y distinto en cada página (xorshift): ni la página
	// cero ni KSM pueden juntarlas, y dos arranques dan los mismos bytes.
	x := uint64(0x9e3779b97f4a7c15)
	for i := 0; i+8 <= len(f.mem); i += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		v := x | 1
		for j := 0; j < 8; j++ {
			f.mem[i+j] = byte(v>>(8*j)) | 1
		}
	}
	return f
}

// leer recorre una vez cada página y devuelve cuántas leyó.
func (f *Falso) leer() (int, byte) {
	var s byte
	n := 0
	for i := 0; i < len(f.mem); i += pagina {
		s ^= f.mem[i]
		n++
	}
	return n, s
}

func (f *Falso) trabajo() map[string]any {
	f.peticiones.Add(1)
	out := map[string]any{"paginas_leidas": 0}
	if f.tocar {
		n, s := f.leer()
		out["paginas_leidas"], out["suma"] = n, s
	}
	if f.retardo > 0 {
		time.Sleep(f.retardo)
	}
	return out
}

func escribir(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Falso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == "/healthz"):
		escribir(w, map[string]any{"status": "ok"})
	case r.Method == http.MethodGet && r.URL.Path == "/stats":
		escribir(w, map[string]any{"mem_mib": len(f.mem) >> 20, "peticiones": f.peticiones.Load()})
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions"):
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
		if req.MaxTokens <= 0 {
			req.MaxTokens = 1
		}
		t0 := time.Now()
		extra := f.trabajo()
		s := time.Since(t0).Seconds()
		if s <= 0 {
			s = 1e-6
		}
		// La forma de llama-server, con los timings que lee 96-von-bench.sh.
		escribir(w, map[string]any{
			"object":  "chat.completion",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "falso"}, "finish_reason": "length"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": req.MaxTokens},
			"timings": map[string]any{"prompt_n": 1, "predicted_n": req.MaxTokens, "prompt_per_second": 1 / s, "predicted_per_second": float64(req.MaxTokens) / s},
			"falso":   extra,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/embeddings":
		extra := f.trabajo()
		escribir(w, map[string]any{"object": "list", "data": []any{map[string]any{"index": 0, "embedding": []float64{0.1, 0.2, 0.3}}}, "falso": extra})
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/decide" || r.URL.Path == "/v1/classify"):
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		out := f.trabajo()
		out["decision"] = "falso"
		escribir(w, out)
	default:
		http.NotFound(w, r)
	}
}
