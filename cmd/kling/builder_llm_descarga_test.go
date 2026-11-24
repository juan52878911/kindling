package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// datosAleatorios da n bytes que no se comprimen ni son todo ceros.
func datosAleatorios(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return b
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// limitado escribe a como mucho bps bytes por segundo: un flujo HTTP de un CDN
// que tiene un techo por conexión (57 MiB/s medidos desde EC2 a HuggingFace).
type limitado struct {
	http.ResponseWriter
	bps int
}

func (l limitado) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := 256 << 10
		if c > len(p) {
			c = len(p)
		}
		t0 := time.Now()
		k, err := l.ResponseWriter.Write(p[:c])
		n += k
		if err != nil {
			return n, err
		}
		if d := time.Duration(float64(c)/float64(l.bps)*float64(time.Second)) - time.Since(t0); d > 0 {
			time.Sleep(d)
		}
		p = p[c:]
	}
	return n, nil
}

// servidorDe sirve data (con rangos, como un CDN) a bps por conexión.
func servidorDe(data []byte, bps int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(limitado{w, bps}, r, "m.gguf", time.Time{}, bytes.NewReader(data))
	}))
}

// trozosPequeños deja los trozos y los flujos en tamaños de test.
func trozosPequeños(t *testing.T, trozo int64, n int) {
	t.Helper()
	a, b := tamTrozo, flujos
	tamTrozo, flujos = trozo, n
	t.Cleanup(func() { tamTrozo, flujos = a, b })
}

// contarHash cuenta cuántas veces se relee entero un fichero para hashearlo.
func contarHash(t *testing.T) *int {
	t.Helper()
	n := new(int)
	antes := hashFichero
	hashFichero = func(p string) (string, error) { *n++; return antes(p) }
	t.Cleanup(func() { hashFichero = antes })
	return n
}

func conBoot(t *testing.T, id string) {
	t.Helper()
	antes := bootID
	bootID = func() string { return id }
	t.Cleanup(func() { bootID = antes })
}

// Un servidor con rangos recibe varias peticiones a la vez, el fichero sale
// entero y con su hash; un trozo manipulado hace fallar la descarga sin dejar
// nada en la caché.
func TestDescargaPorRangosHashCorrecto(t *testing.T) {
	trozosPequeños(t, 64<<10, 4)
	data := datosAleatorios(1<<20 + 123)
	want := sha256Hex(data)
	var mu sync.Mutex
	rangos, enVuelo, maxVuelo := 0, 0, 0
	malo := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Header.Get("Range") != "" {
			rangos++
		}
		enVuelo++
		if enVuelo > maxVuelo {
			maxVuelo = enVuelo
		}
		m := malo
		mu.Unlock()
		defer func() { mu.Lock(); enVuelo--; mu.Unlock() }()
		time.Sleep(5 * time.Millisecond)
		d := data
		if m && strings.HasPrefix(r.Header.Get("Range"), "bytes=131072-") {
			d = append([]byte(nil), data...)
			d[131072+7] ^= 0xff
		}
		http.ServeContent(w, r, "m.gguf", time.Time{}, bytes.NewReader(d))
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "m.gguf")
	if err := fetchVerified(srv.URL, want, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, data) {
		t.Fatal("el fichero descargado por trozos no es el original")
	}
	if rangos < 2 || maxVuelo < 2 {
		t.Fatalf("%d peticiones con rango, %d a la vez como mucho: quería varias en paralelo", rangos, maxVuelo)
	}

	malo = true
	otro := filepath.Join(t.TempDir(), "x.gguf")
	if err := fetchVerified(srv.URL, want, otro); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("un trozo manipulado: %v", err)
	}
	for _, p := range []string{otro, otro + ".part", otro + ".verified"} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("quedó %s tras un hash malo", filepath.Base(p))
		}
	}
}

// Un servidor que no sabe de rangos (contesta 200 con todo) se baja de un
// solo flujo, con la primera respuesta, sin pedir más.
func TestDescargaSinRangosCaeAUnFlujo(t *testing.T) {
	trozosPequeños(t, 64<<10, 4)
	data := datosAleatorios(512<<10 + 5)
	want := sha256Hex(data)
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write(data)
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "m.gguf")
	if err := fetchVerified(srv.URL, want, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, data) {
		t.Fatal("contenido distinto")
	}
	if hits != 1 {
		t.Fatalf("%d peticiones a un servidor sin rangos; quería 1", hits)
	}
}

// Con el sello de una verificación anterior (mismo fichero, mismo arranque),
// reconstruir no relee el modelo entero.
func TestSelloEvitaRehash(t *testing.T) {
	conBoot(t, "arranque-1")
	data := datosAleatorios(300 << 10)
	want := sha256Hex(data)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.ServeContent(w, r, "m.gguf", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "m.gguf")
	if err := fetchVerified(srv.URL, want, dst); err != nil {
		t.Fatal(err)
	}
	primera := hits
	n := contarHash(t)
	for i := 0; i < 3; i++ {
		if err := fetchVerified(srv.URL, want, dst); err != nil {
			t.Fatal(err)
		}
	}
	if *n != 0 {
		t.Fatalf("con el sello, el fichero se rehasheó %d veces", *n)
	}
	if hits != primera {
		t.Fatalf("%d peticiones tras la primera descarga: se volvió a bajar", hits-primera)
	}
}

// El sello no vale si el fichero cambió (aunque conserve el tamaño) ni tras
// otro arranque (un corte pudo dejar en disco algo distinto de lo que se
// verificó en memoria), y nunca sustituye la verificación de una descarga.
func TestSelloNoValeSiCambiaElFichero(t *testing.T) {
	conBoot(t, "arranque-1")
	data := datosAleatorios(200 << 10)
	want := sha256Hex(data)
	sirve := data
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "m.gguf", time.Time{}, bytes.NewReader(sirve))
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "m.gguf")
	if err := fetchVerified(srv.URL, want, dst); err != nil {
		t.Fatal(err)
	}
	n := contarHash(t)

	// Mismo tamaño, otro contenido y otra hora: se rehashea, no cuadra y se
	// vuelve a bajar.
	roto := append([]byte(nil), data...)
	roto[10] ^= 1
	if err := os.WriteFile(dst, roto, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(dst, future, future)
	if err := fetchVerified(srv.URL, want, dst); err != nil {
		t.Fatal(err)
	}
	if *n == 0 {
		t.Fatal("un fichero cambiado pasó por el sello sin rehashear")
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, data) {
		t.Fatal("el fichero cambiado no se sustituyó por el bueno")
	}

	// Otro arranque: se rehashea una vez y se vuelve a sellar.
	*n = 0
	conBoot(t, "arranque-2")
	if err := fetchVerified(srv.URL, want, dst); err != nil || *n != 1 {
		t.Fatalf("tras otro arranque: err %v, %d hashes; quería 1", err, *n)
	}
	if err := fetchVerified(srv.URL, want, dst); err != nil || *n != 1 {
		t.Fatalf("resellado: err %v, %d hashes; quería 1", err, *n)
	}

	// Un sello plantado para otro fichero no hace pasar una descarga mala.
	otro := filepath.Join(t.TempDir(), "o.gguf")
	sello, _ := os.ReadFile(dst + ".verified")
	os.WriteFile(otro+".verified", sello, 0o600)
	sirve = roto
	if err := fetchVerified(srv.URL, want, otro); err == nil {
		t.Fatal("una descarga con el hash malo pasó gracias a un sello ajeno")
	}
}

// La descarga de un modelo con un techo por conexión: lo que cuesta bajar 64
// MiB a 64 MiB/s por flujo.
func BenchmarkDescargaFlujoLimitado(b *testing.B) {
	data := datosAleatorios(64 << 20)
	want := sha256Hex(data)
	srv := servidorDe(data, 64<<20)
	defer srv.Close()
	dir := b.TempDir()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := filepath.Join(dir, "m.gguf")
		os.Remove(dst)
		os.Remove(dst + ".verified")
		if err := fetchVerified(srv.URL, want, dst); err != nil {
			b.Fatal(err)
		}
	}
}

// Reconstruir con el modelo ya en la caché: lo que cuesta comprobarlo (256
// MiB, caché de páginas caliente).
func BenchmarkFetchVerificadoEnCache(b *testing.B) {
	data := datosAleatorios(256 << 20)
	want := sha256Hex(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no debería bajar nada", http.StatusTeapot)
	}))
	defer srv.Close()
	dir := b.TempDir()
	dst := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		b.Fatal(err)
	}
	data = nil
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := fetchVerified(srv.URL, want, dst); err != nil {
			b.Fatal(err)
		}
	}
}
