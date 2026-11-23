package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPercentil(t *testing.T) {
	ms := []float64{5, 1, 4, 2, 3, 10, 9, 8, 7, 6}
	for _, c := range []struct {
		p, want float64
	}{{50, 5}, {95, 10}, {99, 10}, {0, 1}, {100, 10}} {
		if got := percentil(ms, c.p); got != c.want {
			t.Errorf("p%v = %v, quería %v", c.p, got, c.want)
		}
	}
	if got := percentil(nil, 50); got != 0 {
		t.Errorf("sin muestras: %v", got)
	}
	// No reordena la entrada de quien llama.
	if ms[0] != 5 {
		t.Error("percentil desordenó la entrada")
	}
}

func TestParseClientes(t *testing.T) {
	got, err := parseClientes("1,4 16")
	if err != nil || len(got) != 3 || got[0] != 1 || got[1] != 4 || got[2] != 16 {
		t.Fatalf("parseClientes: %v %v", got, err)
	}
	for _, mal := range []string{"", "0", "-1", "a", "1,,x", "100000"} {
		if _, err := parseClientes(mal); err == nil {
			t.Errorf("parseClientes(%q) debía fallar", mal)
		}
	}
}

// La carga cuenta códigos y errores por separado, respeta el número de
// clientes simultáneos y para en N peticiones.
func TestCargaCuentaYAcotaConcurrencia(t *testing.T) {
	var vivas, max, total atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := vivas.Add(1)
		defer vivas.Add(-1)
		for {
			m := max.Load()
			if v <= m || max.CompareAndSwap(m, v) {
				break
			}
		}
		n := total.Add(1)
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"x":1}` || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "cuerpo", 400)
			return
		}
		time.Sleep(2 * time.Millisecond)
		if n%10 == 0 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p := cargar(context.Background(), srv.Client(), Peticion{URL: srv.URL + "/v1/m/x/v1/decide", Metodo: "POST", Cuerpo: []byte(`{"x":1}`)}, 4, 0, 40)
	if p.N != 40 || total.Load() != 40 {
		t.Fatalf("N=%d servidas=%d, quería 40", p.N, total.Load())
	}
	if p.Codigos["200"] != 36 || p.Codigos["503"] != 4 || p.Errores != 4 {
		t.Errorf("códigos %v errores %d", p.Codigos, p.Errores)
	}
	if max.Load() > 4 {
		t.Errorf("más de 4 en vuelo: %d", max.Load())
	}
	if p.P50 <= 0 || p.P99 < p.P50 || p.Max < p.P99 || p.RPS <= 0 {
		t.Errorf("cifras incoherentes: %+v", p)
	}
}

// Con duración en vez de N, para al cumplirla aunque el servidor tarde.
func TestCargaPorDuracion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
	}))
	defer srv.Close()
	t0 := time.Now()
	p := cargar(context.Background(), srv.Client(), Peticion{URL: srv.URL, Metodo: "GET"}, 2, 100*time.Millisecond, 0)
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("tardó %v", d)
	}
	if p.N == 0 || p.Errores != 0 {
		t.Errorf("%+v", p)
	}
}

// Por socket Unix (el gateway de IA escucha así por defecto).
func TestClientePorSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "iab")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("sin sockets Unix:", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })}
	go srv.Serve(ln)
	defer srv.Close()
	c := clienteHTTP(sock, 5*time.Second)
	p := cargar(context.Background(), c, Peticion{URL: "http://gw/healthz", Metodo: "GET"}, 1, 0, 3)
	if p.Codigos["200"] != 3 {
		t.Fatalf("%+v", p)
	}
}

// El servidor de mentira imita a llama-server (timings, /health) y a
// chispa-server (/healthz, /v1/decide), y su memoria ocupa lo pedido con
// páginas no nulas (si fueran ceros, el núcleo las compartiría con la página
// cero y no se mediría nada).
func TestFalso(t *testing.T) {
	f := nuevoFalso(2, true, 0)
	if len(f.mem) != 2<<20 {
		t.Fatalf("memoria %d", len(f.mem))
	}
	ceros := 0
	for i := 0; i < len(f.mem); i += pagina {
		if f.mem[i] == 0 && f.mem[i+pagina/2] == 0 {
			ceros++
		}
	}
	if ceros > 0 {
		t.Errorf("%d páginas a cero", ceros)
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	for _, ruta := range []string{"/health", "/healthz"} {
		r, err := http.Get(srv.URL + ruta)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("%s: %v %v", ruta, r, err)
		}
		r.Body.Close()
	}
	r, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"hola"}],"max_tokens":3}`))
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("chat: %v %v", r, err)
	}
	var chat struct {
		Choices []struct {
			Message struct{ Content string }
		}
		Timings struct {
			PromptN     int     `json:"prompt_n"`
			PredictedN  int     `json:"predicted_n"`
			PromptPerS  float64 `json:"prompt_per_second"`
			PredictedPS float64 `json:"predicted_per_second"`
		}
	}
	if err := json.NewDecoder(r.Body).Decode(&chat); err != nil || len(chat.Choices) != 1 || chat.Timings.PredictedN != 3 || chat.Timings.PredictedPS <= 0 {
		t.Errorf("chat: %+v %v", chat, err)
	}
	r.Body.Close()
	r, err = http.Post(srv.URL+"/v1/decide", "application/json", bytes.NewReader([]byte(`{"tarea":"t","texto":"x"}`)))
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("decide: %v %v", r, err)
	}
	var dec map[string]any
	json.NewDecoder(r.Body).Decode(&dec)
	r.Body.Close()
	if dec["paginas_leidas"] != float64(2<<20/pagina) {
		t.Errorf("con -tocar cada petición lee todas las páginas: %v", dec)
	}
	r, _ = http.Get(srv.URL + "/no-existe")
	if r.StatusCode != 404 {
		t.Errorf("ruta desconocida: %d", r.StatusCode)
	}
	r.Body.Close()
}

// Dos arranques llenan igual la memoria: el contenido no depende del reloj ni
// del azar, así que un dorado hecho dos veces pesa lo mismo y se compara.
func TestFalsoDeterminista(t *testing.T) {
	a, b := nuevoFalso(1, false, 0), nuevoFalso(1, false, 0)
	if !bytes.Equal(a.mem, b.mem) {
		t.Error("dos falsos del mismo tamaño deben llenar igual la memoria")
	}
}
