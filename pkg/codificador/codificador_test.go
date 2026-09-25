package codificador

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// datos sintéticos: K nubes en D dimensiones, deterministas. Los centros son
// siempre los mismos; seed cambia solo el ruido (train y validación).
func nubes(n, K, D int, seed uint64) Dataset {
	r := &splitmix{s: 99}
	centros := make([][]float64, K)
	for k := range centros {
		centros[k] = make([]float64, D)
		for d := range centros[k] {
			centros[k][d] = 4 * (r.uniform() - 0.5)
		}
	}
	r = &splitmix{s: seed}
	var ds Dataset
	for i := 0; i < n; i++ {
		k := i % K
		v := make([]float32, D)
		for d := range v {
			v[d] = float32(centros[k][d] + (r.uniform() - 0.5))
		}
		ds.X, ds.Y = append(ds.X, v), append(ds.Y, k)
	}
	return ds
}

func entrenar(t *testing.T, hidden int) *TrainResult {
	t.Helper()
	labels := []string{"a", "b", "c", "d"}
	res, err := Train(labels, nubes(600, 4, 16, 1), nubes(200, 4, 16, 2), TrainConfig{
		Hidden: hidden, Epochs: 20, Meta: Meta{Encoder: "test:q8_0", Prefix: "query: "}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestTrainLogistica(t *testing.T) {
	for _, h := range []int{0, 8} {
		res := entrenar(t, h)
		if res.ValidAccuracy < 0.95 || res.Agreement < 0.99 {
			t.Fatalf("hidden %d: acierto %.3f, acuerdo int16/float %.3f", h, res.ValidAccuracy, res.Agreement)
		}
		// Determinista: el mismo entrenamiento da el mismo fichero, bit a bit.
		a, _ := res.Head.Marshal()
		b, _ := entrenar(t, h).Head.Marshal()
		if !bytes.Equal(a, b) {
			t.Fatalf("hidden %d: dos entrenamientos iguales dan ficheros distintos", h)
		}
		p, err := res.Head.Predict(nubes(4, 4, 16, 2).X[1])
		if err != nil || p.Label != "b" || p.Prob <= 0 || p.Prob > 1 || len(p.Probs) != 4 {
			t.Fatalf("predicción: %+v %v", p, err)
		}
		if _, err := res.Head.Predict(make([]float32, 3)); err == nil {
			t.Fatal("un vector de otra dimensión debería fallar")
		}
	}
}

func TestFormato(t *testing.T) {
	for _, h := range []int{0, 8} {
		head := entrenar(t, h).Head
		path := filepath.Join(t.TempDir(), "h.jenc")
		if err := head.Save(path); err != nil {
			t.Fatal(err)
		}
		got, err := LoadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := head.Marshal()
		b, _ := got.Marshal()
		if !bytes.Equal(a, b) || got.Meta.Prefix != "query: " || got.Meta.Encoder != "test:q8_0" {
			t.Fatalf("hidden %d: ida y vuelta distinta", h)
		}
		// Cualquier byte cambiado lo detecta el CRC; recortado, también.
		for _, i := range []int{0, 9, 20, len(a) / 2, len(a) - 5} {
			c := append([]byte(nil), a...)
			c[i] ^= 0x40
			if _, err := Unmarshal(c); err == nil {
				t.Errorf("byte %d cambiado y se acepta", i)
			}
		}
		if _, err := Unmarshal(a[:len(a)-7]); err == nil {
			t.Error("recortado y se acepta")
		}
	}
}

func FuzzUnmarshal(f *testing.F) {
	for _, h := range []int{0, 4} {
		res, err := Train([]string{"x", "y"}, nubes(40, 2, 4, 3), nubes(20, 2, 4, 4), TrainConfig{Hidden: h, Epochs: 2})
		if err != nil {
			f.Fatal(err)
		}
		b, _ := res.Head.Marshal()
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := Unmarshal(b)
		if err != nil {
			return
		}
		// Lo que se acepta tiene que poder usarse sin reventar.
		_, _ = h.Predict(make([]float32, h.Dim))
	})
}

func TestCache(t *testing.T) {
	c := NewCache(CacheHeader{Model: "m:q8_0", SHA256: "ab", Dim: 3})
	if err := c.Put("query: hola", []float32{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("x", []float32{1}); err == nil {
		t.Fatal("otra dimensión debería fallar")
	}
	c.Put("query: adiós", []float32{-1, 0.5, 0})
	path := filepath.Join(t.TempDir(), "c.jemb")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCache(path)
	if err != nil || got.Len() != 2 || got.Header.Model != "m:q8_0" {
		t.Fatalf("%+v %v", got, err)
	}
	if v, ok := got.Get("query: adiós"); !ok || v[1] != 0.5 {
		t.Fatalf("entrada: %v %v", v, ok)
	}
	// Sin Next, lo que falta es un error; con Next, se pide y se guarda.
	cached := &Cached{Cache: got}
	if _, err := cached.Embed(context.Background(), []string{"query: nuevo"}); err == nil {
		t.Fatal("sin codificador detrás, lo que no está debería fallar")
	}
	cached.Next = fakeEmbedder{dim: 3}
	vs, err := cached.Embed(context.Background(), []string{"query: hola", "query: nuevo"})
	if err != nil || vs[0][2] != 3 || got.Len() != 3 {
		t.Fatalf("%v %v (%d)", vs, err, got.Len())
	}
}

func FuzzUnmarshalCache(f *testing.F) {
	c := NewCache(CacheHeader{Model: "m", Dim: 2})
	c.Put("a", []float32{1, 2})
	path := filepath.Join(f.TempDir(), "c.jemb")
	if err := c.Save(path); err != nil {
		f.Fatal(err)
	}
	got, _ := LoadCache(path)
	_ = got
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = UnmarshalCache(b) })
}

type fakeEmbedder struct{ dim int }

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, f.dim)
		out[i][0] = float32(len(texts[i]))
	}
	return out, nil
}

func TestHTTPEmbedder(t *testing.T) {
	var cuerpo string
	respuesta := `{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		var b struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		cuerpo = strings.Join(b.Input, "|")
		w.Write([]byte(respuesta))
	}))
	defer srv.Close()
	e := &HTTPEmbedder{URL: srv.URL, Dim: 2}
	vs, err := e.Embed(context.Background(), []string{"uno", "  "})
	if err != nil || vs[0][0] != 1 || vs[1][1] != 1 {
		t.Fatalf("%v %v", vs, err)
	}
	if cuerpo != "uno| " {
		t.Fatalf("una entrada vacía va como un espacio: %q", cuerpo)
	}
	for _, mala := range []string{
		`{"data":[{"index":0,"embedding":[1,0]}]}`,                                   // falta uno
		`{"data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[1,0]}]}`,     // repetido
		`{"data":[{"index":0,"embedding":[1,0,0]},{"index":1,"embedding":[1,0,0]}]}`, // otra dimensión
		`{"data":[{"index":0,"embedding":[1,0]},{"index":7,"embedding":[1,0]}]}`,     // índice fuera
		`not json`,
	} {
		respuesta = mala
		if _, err := e.Embed(context.Background(), []string{"a", "b"}); err == nil {
			t.Errorf("debería fallar: %s", mala)
		}
	}
	if _, err := ParseEmbeddings(200, strings.NewReader(strings.Repeat(" ", 200<<10)), 1, 2); err == nil {
		t.Error("una respuesta desmedida debería cortarse")
	}
	if _, err := e.Embed(context.Background(), make([]string, MaxBatch+1)); err == nil {
		t.Error("más de MaxBatch textos debería fallar")
	}
}

func TestLayer(t *testing.T) {
	head := entrenar(t, 0).Head
	ds := nubes(8, 4, 16, 2)
	c := NewCache(CacheHeader{Model: "test:q8_0", Dim: 16})
	c.Put("query: tres", ds.X[3])
	l := &Layer{Head: head, Embedder: &Cached{Cache: c}}
	intent, p, _, err := l.ClassifyIntent(context.Background(), "tres")
	if err != nil || intent != "d" || !(p > 0.5) || math.IsNaN(p) {
		t.Fatalf("%s %.3f %v", intent, p, err)
	}
	if _, _, _, err := l.ClassifyIntent(context.Background(), "otra"); err == nil {
		t.Fatal("un texto sin vector debería fallar")
	}
}

// bloqueaEmbedder no contesta nunca por su cuenta: solo vuelve cuando el
// contexto que le pasan termina, para probar el plazo de Layer con un plazo
// de verdad (context.DeadlineExceeded) y no uno simulado.
type bloqueaEmbedder struct{}

func (bloqueaEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestLayerTimeout(t *testing.T) {
	head := entrenar(t, 0).Head
	l := &Layer{Head: head, Embedder: bloqueaEmbedder{}, Timeout: 10 * time.Millisecond}
	t0 := time.Now()
	_, _, _, err := l.ClassifyIntent(context.Background(), "algo")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("esperaba context.DeadlineExceeded, got %v", err)
	}
	if el := time.Since(t0); el < l.Timeout || el > 2*time.Second {
		t.Fatalf("el plazo tardó %v, esperaba ~%v", el, l.Timeout)
	}
	// El plazo por defecto (Timeout 0) es 2 s: sin uno explícito no se puede
	// esperar 2 s de verdad en un test, pero sí comprobar que Classify lo pone
	// en el contexto que le llega al codificador.
	var got time.Duration
	deadline := deadlineCatcher(func(ctx context.Context) {
		if dl, ok := ctx.Deadline(); ok {
			got = time.Until(dl)
		}
	})
	l2 := &Layer{Head: head, Embedder: deadline}
	_, _ = l2.Classify(context.Background(), "algo")
	if got <= 0 || got > 2*time.Second {
		t.Fatalf("plazo por defecto: %v, esperaba algo <= 2s", got)
	}
}

// deadlineCatcher es un Embedder que le pasa su contexto a fn y devuelve un
// error (no hace falta un vector de verdad: la prueba solo mira el plazo).
type deadlineCatcher func(ctx context.Context)

func (d deadlineCatcher) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	d(ctx)
	return nil, errors.New("solo para inspeccionar el plazo")
}

func TestKNN(t *testing.T) {
	tr := nubes(200, 4, 16, 1)
	k := NewKNN(5, []string{"a", "b", "c", "d"}, tr)
	va := nubes(40, 4, 16, 2)
	ok := 0
	for i, v := range va.X {
		if y, _ := k.Predict(v); y == va.Y[i] {
			ok++
		}
	}
	if ok < 36 {
		t.Fatalf("k-NN acierta %d de 40", ok)
	}
}
