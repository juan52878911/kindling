package frontal

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Range en la descarga: 206 con Content-Range, 416 fuera de rango.
func TestDescargaConRange(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	base := "/v1/sandboxes/" + sb.ID
	esperaCodigo(t, pide(t, srv, tokenAlice, "PUT", base+"/files?path=/tmp/n.txt", "0123456789"), 200)

	get := func(rango string) (*http.Response, string) {
		req := peticion(t, srv, tokenAlice, "GET", base+"/files?path=/tmp/n.txt", nil)
		if rango != "" {
			req.Header.Set("Range", rango)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := get("")
	if resp.StatusCode != 200 || body != "0123456789" || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("completo: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	for _, c := range []struct{ rango, body, cr string }{
		{"bytes=2-5", "2345", "bytes 2-5/10"},
		{"bytes=7-", "789", "bytes 7-9/10"},
		{"bytes=-3", "789", "bytes 7-9/10"},
		{"bytes=8-100", "89", "bytes 8-9/10"},
	} {
		resp, body := get(c.rango)
		if resp.StatusCode != 206 || body != c.body || resp.Header.Get("Content-Range") != c.cr {
			t.Errorf("%s: %d %q %q", c.rango, resp.StatusCode, body, resp.Header.Get("Content-Range"))
		}
	}
	for _, rango := range []string{"bytes=10-", "bytes=20-30"} {
		resp, _ := get(rango)
		if resp.StatusCode != 416 || resp.Header.Get("Content-Range") != "bytes */10" {
			t.Errorf("%s: %d %q", rango, resp.StatusCode, resp.Header.Get("Content-Range"))
		}
	}
	// Un Range que no entendemos se ignora, como manda el RFC: 200 entero.
	if resp, body := get("items=1-2"); resp.StatusCode != 200 || body != "0123456789" {
		t.Errorf("unidad rara: %d %q", resp.StatusCode, body)
	}
}

func TestSubidaTar(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	resp := pide(t, srv, tokenAlice, "PUT", "/v1/sandboxes/"+sb.ID+"/files?path=/work&tar=1&mkdir=1", buf.Bytes())
	esperaCodigo(t, resp, 200)
	r := decodifica[api.TarResult](t, resp)
	f.mu.Lock()
	q := f.ultimoTar
	f.mu.Unlock()
	if r.Entries != 1 || q.Get("format") != "tar" || q.Get("mkdir") != "1" || q.Get("path") != "/work" {
		t.Fatalf("%+v %v", r, q)
	}
	// Ajeno: 404.
	esperaCodigo(t, pide(t, srv, tokenBob, "PUT", "/v1/sandboxes/"+sb.ID+"/files?path=/work&tar=1", buf.Bytes()), 404)
}

func TestMkdirDeFicheros(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	base := "/v1/sandboxes/" + sb.ID + "/files/mkdir"
	resp := pide(t, srv, tokenAlice, "POST", base, map[string]string{"path": "/work/x/y"})
	esperaCodigo(t, resp, 201)
	resp.Body.Close()
	f.mu.Lock()
	q := f.ultimoTar
	f.mu.Unlock()
	if q.Get("path") != "/work/x/y" || q.Get("mkdir") != "1" {
		t.Fatalf("mkdir fue %v", q)
	}
	for _, malo := range []string{`{}`, `{"path":"rel/x"}`, `{"path":"/a","otro":1}`} {
		esperaCodigo(t, pide(t, srv, tokenAlice, "POST", base, malo), 400)
	}
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", base, nil), 405)
}

// move y search corren de verdad (mv y find de esta máquina) contra un
// directorio temporal: el daemon real del banco ejecuta lo que le llega.
func TestMoverYBuscarFicheros(t *testing.T) {
	b := montarCodigo(t)
	dir := t.TempDir()
	for _, n := range []string{"uno.log", "dos.log", "tres.txt", "sub/cuatro.log"} {
		p := filepath.Join(dir, n)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resp := pide(t, b.srv, tokenAlice, "GET", b.base+"/files/search?path="+dir+"&glob=*.log", nil)
	esperaCodigo(t, resp, 200)
	res := decodifica[BusquedaFicheros](t, resp)
	if len(res.Matches) != 3 || res.Truncated || !strings.HasPrefix(res.Matches[0], dir) {
		t.Fatalf("search: %+v", res)
	}
	for _, malo := range []string{"?glob=*.log", "?path=" + dir, "?path=rel&glob=*", "?path=" + dir + "&glob=a/b"} {
		esperaCodigo(t, pide(t, b.srv, tokenAlice, "GET", b.base+"/files/search"+malo, nil), 400)
	}

	mover := map[string]string{"from": filepath.Join(dir, "uno.log"), "to": filepath.Join(dir, "movido.log")}
	resp = pide(t, b.srv, tokenAlice, "POST", b.base+"/files/move", mover)
	esperaCodigo(t, resp, 200)
	resp.Body.Close()
	if _, err := os.Stat(mover["to"]); err != nil {
		t.Fatalf("no se movió: %v", err)
	}
	if _, err := os.Stat(mover["from"]); err == nil {
		t.Fatal("el origen sigue ahí")
	}
	// Origen que no existe: 404 con code, no un 200 ni un 502.
	resp = pide(t, b.srv, tokenAlice, "POST", b.base+"/files/move", map[string]string{"from": filepath.Join(dir, "nada"), "to": filepath.Join(dir, "x")})
	esperaCodigo(t, resp, 404)
	resp.Body.Close()
	// Un nombre que parece opción no se interpreta como tal.
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "POST", b.base+"/files/move", map[string]string{"from": "-rf", "to": "/x"}), 400)
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "POST", b.base+"/files/move", `{"from":"/a"}`), 400)
}
