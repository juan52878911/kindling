package credproxy

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

// El sustituidor cambia el marcador aunque llegue partido byte a byte, y lo
// que no es marcador sale igual.
func TestSustituidorMarcadorPartido(t *testing.T) {
	cs := []Credential{{Placeholder: testPlace, Secret: testSecret}, {Placeholder: testPlace2, Secret: testSecret2}}
	in := testPlace + " medio kling-cred-no-es " + testPlace2 + " kling-" + testPlace
	want := testSecret + " medio kling-cred-no-es " + testSecret2 + " kling-" + testSecret
	got, err := io.ReadAll(nuevoSustituidor(iotest.OneByteReader(strings.NewReader(in)), cs))
	if err != nil || string(got) != want {
		t.Fatalf("%q, %v\nquería %q", got, err, want)
	}
	// Un error de la fuente llega tal cual, después de lo leído.
	_, err = io.ReadAll(nuevoSustituidor(iotest.TimeoutReader(strings.NewReader(strings.Repeat("x", 10<<10))), cs))
	if err != iotest.ErrTimeout {
		t.Errorf("error %v, quería el de la fuente", err)
	}
}

// cuerpoGrande son n bytes de relleno con el marcador al principio, al final y
// repartido en posiciones que caen a caballo de los trozos de lectura.
func cuerpoGrande(n int, marcador string) []byte {
	b := bytes.Repeat([]byte("abcdefghij"), n/10)
	for _, pos := range []int{0, sustChunk - 10, 3*sustChunk - 1, n / 2, n - len(marcador)} {
		copy(b[pos:], marcador)
	}
	return b
}

// Un cuerpo chunked de más de MaxSwapBody: antes se reenviaba sin tocar;
// ahora el marcador se cambia en todo él, y sale chunked.
func TestProxySustituyeEnUnCuerpoChunkedGrande(t *testing.T) {
	const n = 3 << 20
	raw := cuerpoGrande(n, testPlace)
	want := bytes.ReplaceAll(raw, []byte(testPlace), []byte(testSecret))
	var recibido []byte
	var declarada int64
	srv, _ := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {
		declarada = r.ContentLength
		recibido, _ = io.ReadAll(r.Body)
	})

	// Por una tubería y en trozos de tamaño raro: el cliente lo manda chunked
	// y los marcadores quedan partidos entre escrituras.
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < len(raw); i += 7777 {
			pw.Write(raw[i:min(i+7777, len(raw))])
		}
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/upload", pr)
	req.Host = "example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if declarada != -1 {
		t.Errorf("Content-Length %d; un cuerpo que no cabe en MaxSwapBody debería salir chunked", declarada)
	}
	if sha256.Sum256(recibido) != sha256.Sum256(want) {
		t.Fatalf("el proveedor recibió %d bytes (%d marcadores, %d claves); quería %d con %d claves",
			len(recibido), bytes.Count(recibido, []byte(testPlace)), bytes.Count(recibido, []byte(testSecret)),
			len(want), bytes.Count(want, []byte(testSecret)))
	}
}

// Un cuerpo grande CON Content-Length también se sustituye (y sale chunked,
// porque su longitud cambia); uno pequeño chunked sale con su longitud.
func TestProxyCuerpoConYSinLongitud(t *testing.T) {
	var recibido []byte
	var declarada int64
	srv, _ := proxyContra(t, func(w http.ResponseWriter, r *http.Request) {
		declarada = r.ContentLength
		recibido, _ = io.ReadAll(r.Body)
	})
	enviar := func(body io.Reader, largo int64) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/", body)
		req.Host = "example.com"
		req.ContentLength = largo
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	raw := cuerpoGrande(2<<20, testPlace2)
	enviar(bytes.NewReader(raw), int64(len(raw)))
	if want := bytes.ReplaceAll(raw, []byte(testPlace2), []byte(testSecret2)); !bytes.Equal(recibido, want) || declarada != -1 {
		t.Errorf("grande con longitud: %d bytes, Content-Length %d", len(recibido), declarada)
	}

	pequeño := "secret=" + testPlace
	enviar(io.MultiReader(strings.NewReader(pequeño)), -1)
	if string(recibido) != "secret="+testSecret || declarada != int64(len("secret="+testSecret)) {
		t.Errorf("pequeño chunked: %q con Content-Length %d", recibido, declarada)
	}
}
