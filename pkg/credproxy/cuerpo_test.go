package credproxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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

// Un cuerpo grande CON Content-Length se sustituye y sale CON su Content-Length
// exacto (por fichero temporal: no cabe en memoria) porque el invitado lo
// declaró; uno pequeño chunked sale con la suya, calculada en memoria.
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
	want := bytes.ReplaceAll(raw, []byte(testPlace2), []byte(testSecret2))
	if !bytes.Equal(recibido, want) || declarada != int64(len(want)) {
		t.Errorf("grande con longitud: %d bytes, Content-Length %d; quería %d bytes con esa misma longitud declarada",
			len(recibido), declarada, len(want))
	}

	pequeño := "secret=" + testPlace
	enviar(io.MultiReader(strings.NewReader(pequeño)), -1)
	if string(recibido) != "secret="+testSecret || declarada != int64(len("secret="+testSecret)) {
		t.Errorf("pequeño chunked: %q con Content-Length %d", recibido, declarada)
	}
}

// ficherosEn cuenta lo que queda en dir: el temporal del cuerpo debe
// desaparecer en cuanto termina la petición, la reciba el proveedor o no.
func ficherosEn(t *testing.T, dir string) []string {
	t.Helper()
	ent, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var nombres []string
	for _, e := range ent {
		nombres = append(nombres, e.Name())
	}
	return nombres
}

// Un cuerpo de 3 MiB (por encima de MaxSwapBody) CON Content-Length llega al
// proveedor con el Content-Length exacto y el sha256 esperado, pasando por el
// fichero temporal (cuerpo.go); y ese fichero no sobrevive a la petición.
func TestProxyCuerpoGrandeConLongitudLlegaPorFicheroYSeBorra(t *testing.T) {
	dir := t.TempDir()
	var recibido []byte
	var declarada int64
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		declarada = r.ContentLength
		recibido, _ = io.ReadAll(r.Body)
	}))
	t.Cleanup(up.Close)
	tr := up.Client().Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, up.Listener.Addr().String())
	}
	p := New(Options{Transport: tr, TempDir: dir})
	if _, err := p.SetCredentials([]Credential{{Domain: "example.com", Placeholder: testPlace, Secret: testSecret}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	const n = 3 << 20
	raw := cuerpoGrande(n, testPlace)
	want := bytes.ReplaceAll(raw, []byte(testPlace), []byte(testSecret))
	req, _ := http.NewRequest("POST", srv.URL+"/", bytes.NewReader(raw))
	req.Host = "example.com"
	req.ContentLength = int64(len(raw))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if declarada != int64(len(want)) {
		t.Errorf("Content-Length %d; quería %d (el del cuerpo sustituido)", declarada, len(want))
	}
	if sha256.Sum256(recibido) != sha256.Sum256(want) {
		t.Fatalf("sha256 no coincide: %d bytes recibidos, quería %d", len(recibido), len(want))
	}
	if f := ficherosEn(t, dir); len(f) != 0 {
		t.Errorf("el temporal no se borró: quedan %v en %s", f, dir)
	}
}

// fallaSiempre es un http.RoundTripper que nunca llega al "proveedor": simula
// que la salida falla después de haber preparado el cuerpo (fichero temporal
// incluido).
type fallaSiempre struct{}

func (fallaSiempre) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("fallaSiempre: no hay proveedor")
}

// Si la petición al proveedor falla tras preparar el cuerpo por fichero
// temporal, ese fichero se borra igual: no debe quedar la clave en disco
// porque la petición no llegó a ningún sitio.
func TestProxyBorraElTemporalSiElProveedorFalla(t *testing.T) {
	dir := t.TempDir()
	p := New(Options{Transport: fallaSiempre{}, TempDir: dir})
	if _, err := p.SetCredentials([]Credential{{Domain: "example.com", Placeholder: testPlace, Secret: testSecret}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	raw := cuerpoGrande(3<<20, testPlace)
	req, _ := http.NewRequest("POST", srv.URL+"/", bytes.NewReader(raw))
	req.Host = "example.com"
	req.ContentLength = int64(len(raw))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, quería 502 (el proveedor falla)", resp.StatusCode)
	}
	if f := ficherosEn(t, dir); len(f) != 0 {
		t.Errorf("el temporal no se borró tras el fallo: quedan %v en %s", f, dir)
	}
}
