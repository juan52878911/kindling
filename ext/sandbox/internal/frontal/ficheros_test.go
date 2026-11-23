package frontal

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
	// El efecto: el fichero está extraído dentro del directorio.
	resp = pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+sb.ID+"/files?path=/work/a.txt", nil)
	esperaCodigo(t, resp, 200)
	if b, _ := io.ReadAll(resp.Body); string(b) != "hi" {
		t.Fatalf("a.txt extraído = %q", b)
	}
	resp.Body.Close()
	// Y se baja como tar con ?tar=1.
	resp = pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+sb.ID+"/files?path=/work&tar=1", nil)
	esperaCodigo(t, resp, 200)
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-tar" {
		t.Errorf("Content-Type %q", ct)
	}
	tr := tar.NewReader(resp.Body)
	h, err := tr.Next()
	if err != nil || h.Name != "a.txt" {
		t.Fatalf("tar bajado: %v %+v", err, h)
	}
	if b, _ := io.ReadAll(tr); string(b) != "hi" {
		t.Errorf("contenido %q", b)
	}
	resp.Body.Close()
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+sb.ID+"/files?path=/nada&tar=1", nil), 404)
	// Un tar que pasa del tope de subida: 413, sin llegar al daemon.
	grande := bytes.Repeat([]byte{0}, int(api.FileMaxUpload)+1)
	esperaCodigo(t, pide(t, srv, tokenAlice, "PUT", "/v1/sandboxes/"+sb.ID+"/files?path=/work&tar=1", grande), 413)
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
	// El efecto: el directorio existe.
	resp = pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+sb.ID+"/files?path=/work/x/y&stat=1", nil)
	esperaCodigo(t, resp, 200)
	if st := decodifica[api.FileStat](t, resp); !st.IsDir {
		t.Errorf("no es un directorio: %+v", st)
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
	// Un destino que existe no se pisa sin pedirlo (409); con overwrite sí.
	pisa := map[string]any{"from": filepath.Join(dir, "dos.log"), "to": filepath.Join(dir, "tres.txt")}
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "POST", b.base+"/files/move", pisa), 409)
	if _, err := os.Stat(pisa["from"].(string)); err != nil {
		t.Fatalf("el 409 movió el origen: %v", err)
	}
	pisa["overwrite"] = true
	resp = pide(t, b.srv, tokenAlice, "POST", b.base+"/files/move", pisa)
	esperaCodigo(t, resp, 200)
	resp.Body.Close()
	if _, err := os.Stat(pisa["from"].(string)); err == nil {
		t.Fatal("con overwrite el origen sigue ahí")
	}
	// Origen que no existe: 404 con code, no un 200 ni un 502.
	resp = pide(t, b.srv, tokenAlice, "POST", b.base+"/files/move", map[string]string{"from": filepath.Join(dir, "nada"), "to": filepath.Join(dir, "x")})
	esperaCodigo(t, resp, 404)
	resp.Body.Close()
	// Un nombre que parece opción no se interpreta como tal.
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "POST", b.base+"/files/move", map[string]string{"from": "-rf", "to": "/x"}), 400)
	esperaCodigo(t, pide(t, b.srv, tokenAlice, "POST", b.base+"/files/move", `{"from":"/a"}`), 400)
}

// R4-9: un destino que aparece entre la comprobación y el mv (mv -n sale con 0
// sin mover) no es un 200; y overwrite sobre un directorio no mete el origen
// dentro.
func TestMoverCarreraYDirectorios(t *testing.T) {
	b := montarCodigo(t)
	dir := t.TempDir()
	escribe := func(n string) string {
		p := filepath.Join(dir, n)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mueve := func(m map[string]any) *http.Response {
		return pide(t, b.srv, tokenAlice, "POST", b.base+"/files/move", m)
	}

	// Carrera: el destino aparece justo antes del mv.
	origen, destino := escribe("a.txt"), filepath.Join(dir, "b.txt")
	b.d.mu.Lock()
	b.d.antesMv = func(api.ExecRequest) { _ = os.WriteFile(destino, []byte("otro"), 0o644) }
	b.d.mu.Unlock()
	resp := mueve(map[string]any{"from": origen, "to": destino})
	esperaCodigo(t, resp, 409)
	resp.Body.Close()
	b.d.mu.Lock()
	b.d.antesMv = nil
	b.d.mu.Unlock()
	if got, _ := os.ReadFile(destino); string(got) != "otro" {
		t.Errorf("el destino que apareció se pisó: %q", got)
	}

	if runtime.GOOS == "darwin" {
		t.Skip("el mv de BSD no tiene -T: la parte de directorios es del invitado Linux")
	}
	// overwrite con un fichero sobre un directorio: 409, y nada dentro.
	f := escribe("f.txt")
	d := filepath.Join(dir, "d")
	_ = os.MkdirAll(d, 0o755)
	resp = mueve(map[string]any{"from": f, "to": d, "overwrite": true})
	esperaCodigo(t, resp, 409)
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(d, "f.txt")); err == nil {
		t.Errorf("overwrite metió el origen dentro del directorio destino")
	}
	// overwrite con un directorio sobre un directorio vacío: lo sustituye.
	orig := filepath.Join(dir, "o")
	escribe("o/x.txt")
	resp = mueve(map[string]any{"from": orig, "to": d, "overwrite": true})
	esperaCodigo(t, resp, 200)
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(d, "x.txt")); err != nil {
		t.Errorf("el directorio no sustituyó al destino: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d, "o")); err == nil {
		t.Errorf("el origen quedó anidado dentro del destino")
	}
}
