package guest

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

type ent struct {
	name, body, link string
	typ              byte
	size             int64 // si != 0 con typ Reg, la cabecera dice esto (bomba)
}

func mkTar(t *testing.T, es ...ent) string {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range es {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Linkname: e.link}
		if typ == tar.TypeDir {
			h.Mode = 0o755
		}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
			if e.size != 0 {
				h.Size = e.size
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if e.size != 0 {
				continue // la bomba no trae el contenido
			}
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Flush() // sin Close: los casos con cabecera mentirosa no cierran
	return b.String()
}

func putTarTo(t *testing.T, dir string, es ...ent) int {
	t.Helper()
	return files(http.MethodPut, dir, url.Values{"format": {"tar"}}, mkTar(t, es...)).Code
}

func TestTarRoundTripYListado(t *testing.T) {
	src := t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "d", "e"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "d", "a.txt"), []byte("hola"), 0o640)
	_ = os.WriteFile(filepath.Join(src, "top.sh"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.Symlink("d/a.txt", filepath.Join(src, "ln"))

	rec := files(http.MethodGet, src, url.Values{"format": {"tar"}}, "")
	if rec.Code != 200 {
		t.Fatalf("GET tar %d %s", rec.Code, rec.Body.String())
	}
	dst := t.TempDir()
	rec2 := files(http.MethodPut, dst, url.Values{"format": {"tar"}}, rec.Body.String())
	if rec2.Code != 200 {
		t.Fatalf("PUT tar %d %s", rec2.Code, rec2.Body.String())
	}
	var res api.TarResult
	_ = json.Unmarshal(rec2.Body.Bytes(), &res)
	if res.Files != 2 || res.Dirs != 2 || res.Symlinks != 1 || res.Bytes != 14 {
		t.Fatalf("result %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "d", "a.txt")); string(b) != "hola" {
		t.Fatalf("content %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dst, "top.sh")); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if l, _ := os.Readlink(filepath.Join(dst, "ln")); l != "d/a.txt" {
		t.Fatalf("link %q", l)
	}

	rec = files(http.MethodGet, dst, url.Values{"list": {"1"}}, "")
	var fl api.FileList
	if err := json.Unmarshal(rec.Body.Bytes(), &fl); err != nil || rec.Code != 200 {
		t.Fatalf("list %d %v", rec.Code, err)
	}
	var names []string
	for _, e := range fl.Entries {
		names = append(names, e.Name+":"+e.Type)
		if e.Name == "ln" && e.Target != "d/a.txt" {
			t.Fatalf("target %q", e.Target)
		}
	}
	if got := strings.Join(names, ","); got != "d:dir,ln:symlink,top.sh:file" || fl.Truncated {
		t.Fatalf("entries %s truncated=%v", got, fl.Truncated)
	}
}

func TestListadoNoDirectorio(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x")
	_ = os.WriteFile(f, nil, 0o644)
	if rec := files(http.MethodGet, f, url.Values{"list": {"1"}}, ""); rec.Code != 400 {
		t.Fatalf("file list %d", rec.Code)
	}
	if rec := files(http.MethodGet, "/no/existe/xx", url.Values{"list": {"1"}}, ""); rec.Code != 404 {
		t.Fatalf("missing list %d", rec.Code)
	}
	if rec := files(http.MethodGet, f, url.Values{"format": {"zip"}}, ""); rec.Code != 400 {
		t.Fatalf("format zip %d", rec.Code)
	}
}

func TestTarRechazaRutasQueEscapan(t *testing.T) {
	casos := map[string][]ent{
		"punto-punto":       {{name: "../fuera.txt", body: "x"}},
		"punto-punto-hondo": {{name: "a/../../fuera.txt", body: "x"}},
		"absoluta":          {{name: "/tmp/fuera.txt", body: "x"}},
		"enlace-absoluto":   {{name: "l", typ: tar.TypeSymlink, link: "/etc"}},
		"enlace-sale":       {{name: "l", typ: tar.TypeSymlink, link: "../.."}},
		"atraviesa-enlace":  {{name: "d", typ: tar.TypeSymlink, link: "."}, {name: "d/x", body: "x"}},
		"enlace-duro":       {{name: "h", typ: tar.TypeLink, link: "a"}},
		"dispositivo":       {{name: "dev", typ: tar.TypeChar}},
		"fifo":              {{name: "f", typ: tar.TypeFifo}},
		"duplicado":         {{name: "a", body: "1"}, {name: "a", body: "2"}},
	}
	for nombre, es := range casos {
		t.Run(nombre, func(t *testing.T) {
			base := t.TempDir()
			dst := filepath.Join(base, "dst")
			_ = os.Mkdir(dst, 0o755)
			if code := putTarTo(t, dst, es...); code != http.StatusBadRequest {
				t.Fatalf("code %d, want 400", code)
			}
			for _, f := range []string{filepath.Join(base, "fuera.txt"), "/tmp/fuera.txt"} {
				if _, err := os.Lstat(f); err == nil {
					t.Fatalf("%s was created", f)
				}
			}
			if e, _ := os.ReadDir(dst); nombre != "duplicado" && nombre != "atraviesa-enlace" && len(e) > 0 {
				t.Fatalf("left behind: %v", e)
			}
		})
	}
}

func TestTarNoEscribeATravesDeUnEnlacePlantado(t *testing.T) {
	base := t.TempDir()
	dst := filepath.Join(base, "dst")
	fuera := filepath.Join(base, "fuera")
	_ = os.Mkdir(dst, 0o755)
	_ = os.Mkdir(fuera, 0o755)
	// Un enlace que ya estaba en la máquina y apunta fuera del destino.
	_ = os.Symlink(fuera, filepath.Join(dst, "plantado"))
	if code := putTarTo(t, dst, ent{name: "plantado/x.txt", body: "x"}); code == http.StatusOK {
		t.Fatal("wrote through a planted symlink")
	}
	if _, err := os.Lstat(filepath.Join(fuera, "x.txt")); err == nil {
		t.Fatal("file escaped through the planted symlink")
	}
}

func TestTarBombaDeTamano(t *testing.T) {
	dst := t.TempDir()
	// La cabecera dice 1 TiB: se rechaza sin intentar escribirlo.
	if code := putTarTo(t, dst, ent{name: "big", size: 1 << 40}); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d, want 413", code)
	}
	if _, err := os.Lstat(filepath.Join(dst, "big")); err == nil {
		t.Fatal("bomb file created")
	}
	// Y con topes pequeños, la suma de varios ficheros también.
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, n := range []string{"a", "b", "c"} {
		_ = tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: 10})
		_, _ = tw.Write(make([]byte, 10))
	}
	_ = tw.Close()
	_, err := ExtractTar(&b, t.TempDir(), 25, 100)
	if re, ok := err.(*tarRejected); !ok || !re.tooLarge {
		t.Fatalf("sum over the cap: %v", err)
	}
}

func TestTarDemasiadasEntradas(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for i := 0; i < 5; i++ {
		_ = tw.WriteHeader(&tar.Header{Name: string(rune('a' + i)), Typeflag: tar.TypeDir, Mode: 0o755})
	}
	_ = tw.Close()
	_, err := ExtractTar(&b, t.TempDir(), 1<<20, 3)
	if re, ok := err.(*tarRejected); !ok || !re.tooLarge {
		t.Fatalf("entries cap: %v", err)
	}
}

func TestTarMkdirYSobrescribe(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "nuevo", "sub")
	tarBody := mkTar(t, ent{name: "a", body: "1"})
	if rec := files(http.MethodPut, dst, url.Values{"format": {"tar"}}, tarBody); rec.Code == 200 {
		t.Fatal("missing dir without mkdir should fail")
	}
	for _, body := range []string{"uno", "dos"} {
		rec := files(http.MethodPut, dst, url.Values{"format": {"tar"}, "mkdir": {"1"}}, mkTar(t, ent{name: "a", body: body}))
		if rec.Code != 200 {
			t.Fatalf("PUT %d %s", rec.Code, rec.Body.String())
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a")); string(b) != "dos" {
		t.Fatalf("content %q", b)
	}
}

func TestListadoTruncado(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < api.FileListMax+5; i++ {
		f, _ := os.Create(filepath.Join(dir, "f"+strings.Repeat("x", 3)+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676%26))))
		f.Close()
	}
	rec := files(http.MethodGet, dir, url.Values{"list": {"1"}}, "")
	var fl api.FileList
	_ = json.Unmarshal(rec.Body.Bytes(), &fl)
	if !fl.Truncated || len(fl.Entries) != api.FileListMax {
		t.Fatalf("truncated=%v n=%d", fl.Truncated, len(fl.Entries))
	}
}
