package upgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// La etiqueta acaba en una URL (/releases/download/<etiqueta>/) y en rutas:
// cualquier cosa que no sea vX.Y.Z[-pre] se rechaza, por hostil que sea.
func TestEtiquetaValida(t *testing.T) {
	casos := []struct {
		etiqueta string
		vale     bool
	}{
		{"v1.2.3", true},
		{"v0.0.0", true},
		{"v10.20.30", true},
		{"v1.0.0-rc.1", true},
		{"v1.0.0-beta", true},
		{"v1.0.0-RC1.2", true},

		{"", false},
		{"v", false},
		{"1.2.3", false},
		{"V1.2.3", false},
		{"v1.2", false},
		{"v1.2.3.4", false},
		{"v1.2.3-", false},
		{"v1.2.3+build", false},
		// Rutas: ni / ni "..", ni en el sufijo de prerelease.
		{"v1.2.3/../x", false},
		{"../v1.2.3", false},
		{"v1.2.3/..", false},
		{"v1.2.3-..", false},
		{"v1.2.3-a..b", false},
		{"v1.2.3-a/b", false},
		{"v1.2.3\\..\\x", false},
		{"v1.2.3%2F..%2Fx", false},
		// Lo que cambiaría la URL: query, fragmento, espacios, saltos.
		{"v1.2.3?x=1", false},
		{"v1.2.3#frag", false},
		{"v1.2.3&a=b", false},
		{" v1.2.3", false},
		{"v1.2.3 ", false},
		{"v1.2.3\n", false},
		{"v1.2.3\nv1.2.4", false},
		{"v1.2.3\r\nHost: x", false},
		{"v1.2.3\x00", false},
		{"v1.2.3\t", false},
		// Unicode: dígitos y letras que no son ASCII.
		{"v１.2.3", false},
		{"v٣.2.3", false},
		{"v1.2.3-ñ", false},
		{"v1.2.3-‮", false},
		{"v1․2․3", false}, // U+2024, un «punto» que no lo es
		{"v1.2.3-rc​", false},
	}
	for _, c := range casos {
		if got := EtiquetaValida(c.etiqueta); got != c.vale {
			t.Errorf("EtiquetaValida(%q) = %v, quiero %v", c.etiqueta, got, c.vale)
		}
	}
}

// servidorLatest contesta a /releases/latest con la Location y el estado dados.
func servidorLatest(t *testing.T, estado int, location string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/latest" {
			http.NotFound(w, r)
			return
		}
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(estado)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Lo que diga el servidor en la redirección no se cree: la etiqueta tiene que
// ser válida, y sin redirección no se adivina.
func TestUltimaEtiquetaDesconfiaDeLaRedireccion(t *testing.T) {
	casos := []struct {
		nombre   string
		estado   int
		location string
		quiero   string // vacío = error
	}{
		{"normal", http.StatusFound, "/releases/tag/v1.4.0", "v1.4.0"},
		{"barra final", http.StatusFound, "/releases/tag/v1.4.0/", "v1.4.0"},
		{"absoluta", http.StatusMovedPermanently, "https://github.com/x/y/releases/tag/v0.9.1-rc.2", "v0.9.1-rc.2"},
		{"query", http.StatusFound, "/releases/tag/v1.4.0?x=1", ""},
		{"subida de directorio", http.StatusFound, "/releases/tag/../../etc", ""},
		{"etiqueta con ..", http.StatusFound, "/releases/tag/v1.4.0-..", ""},
		{"página de releases", http.StatusFound, "/releases", ""},
		{"vacía tras la barra", http.StatusFound, "/", ""},
		{"unicode", http.StatusFound, "/releases/tag/v１.4.0", ""},
		{"200 sin redirección", http.StatusOK, "", ""},
		{"200 con Location", http.StatusOK, "/releases/tag/v1.4.0", ""},
		{"302 sin Location", http.StatusFound, "", ""},
		{"404", http.StatusNotFound, "", ""},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			srv := servidorLatest(t, c.estado, c.location)
			f := &Fuente{Repo: srv.URL + "/", permitirHTTP: true}
			got, err := f.UltimaEtiqueta(context.Background())
			if c.quiero == "" {
				if err == nil {
					t.Fatalf("aceptó %q como última etiqueta", got)
				}
				return
			}
			if err != nil || got != c.quiero {
				t.Fatalf("UltimaEtiqueta = %q, %v; quiero %q", got, err, c.quiero)
			}
		})
	}
}

// releaseSimple sirve una release sin firmar (antes de 1.0: se acepta con
// aviso) con los assets dados; sumas es el SHA256SUMS tal cual.
type releaseSimple struct {
	etiqueta string
	assets   map[string][]byte
	sumas    string
	estado   map[string]int // un estado distinto de 200 para un asset

	mu      sync.Mutex
	pedidos []string
}

func (r *releaseSimple) servir(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.pedidos = append(r.pedidos, req.URL.RequestURI())
		r.mu.Unlock()
		pre := "/releases/download/" + r.etiqueta + "/"
		nombre, ok := strings.CutPrefix(req.URL.Path, pre)
		if !ok {
			http.NotFound(w, req)
			return
		}
		if e, ok := r.estado[nombre]; ok {
			w.WriteHeader(e)
			return
		}
		if nombre == "SHA256SUMS" {
			io.WriteString(w, r.sumas)
			return
		}
		b, ok := r.assets[nombre]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *releaseSimple) nPedidos() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pedidos)
}

// restos lista lo que quedó en dest: tras un fallo no puede quedar ni el
// asset ni su .part.
func restos(t *testing.T, dest string) []string {
	t.Helper()
	es, err := os.ReadDir(dest)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var n []string
	for _, e := range es {
		n = append(n, e.Name())
	}
	return n
}

// Una etiqueta hostil no llega a la red: Bajar la rechaza antes de pedir nada.
func TestBajarConEtiquetaHostilNoPideNada(t *testing.T) {
	rel := &releaseSimple{etiqueta: "v0.9.0", sumas: ""}
	srv := rel.servir(t)
	f := &Fuente{Repo: srv.URL, permitirHTTP: true, Avisos: io.Discard}
	for _, e := range []string{"", "v0.9.0/../../x", "v0.9.0?x=", "../SHA256SUMS", "v0.9.0#", "v0.9.0%2f.."} {
		_, err := f.Bajar(context.Background(), e, []Asset{{Nombre: "kling-linux-amd64"}}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "invalid release tag") {
			t.Errorf("Bajar(%q): err = %v, quiero invalid release tag", e, err)
		}
	}
	if n := rel.nPedidos(); n != 0 {
		t.Errorf("se hicieron %d peticiones con etiquetas inválidas: %v", n, rel.pedidos)
	}
}

func TestBajarDeUnaRelease(t *testing.T) {
	bueno := []byte("binario de kling")
	guest := []byte("binario del invitado")
	sumas := fmt.Sprintf("%s  kling-linux-amd64\n%s  kling-guest-linux-amd64\n", sha(bueno), sha(guest))

	t.Run("bien", func(t *testing.T) {
		rel := &releaseSimple{etiqueta: "v0.9.0", sumas: sumas,
			assets: map[string][]byte{"kling-linux-amd64": bueno, "kling-guest-linux-amd64": guest}}
		srv := rel.servir(t)
		var avisos bytes.Buffer
		f := &Fuente{Repo: srv.URL, permitirHTTP: true, Avisos: &avisos}
		dest := filepath.Join(t.TempDir(), "nuevo")
		got, err := f.Bajar(context.Background(), "v0.9.0", []Asset{
			{Nombre: "kling-linux-amd64"},
			{Nombre: "kling-guest-linux-amd64"},
			{Nombre: "kling-hv", Opcional: true}, // no está en SHA256SUMS: se omite
		}, dest)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("bajados = %v, quiero 2", got)
		}
		for nombre, cuerpo := range map[string][]byte{"kling-linux-amd64": bueno, "kling-guest-linux-amd64": guest} {
			b, err := os.ReadFile(got[nombre])
			if err != nil || !bytes.Equal(b, cuerpo) {
				t.Errorf("%s = %q, %v", nombre, b, err)
			}
			fi, err := os.Stat(got[nombre])
			if err != nil || fi.Mode().Perm() != 0o755 {
				t.Errorf("%s: modo %v, %v; quiero 0755", nombre, fi.Mode(), err)
			}
		}
		if r := restos(t, dest); len(r) != 2 {
			t.Errorf("en dest quedó %v", r)
		}
		// Sin firma y antes de 1.0: se acepta, pero avisando.
		if !strings.Contains(avisos.String(), "warning") {
			t.Errorf("sin aviso de release sin firmar: %q", avisos.String())
		}
	})

	fallos := []struct {
		nombre string
		rel    *releaseSimple
		quiero string
	}{
		{"hash distinto", &releaseSimple{sumas: sumas,
			assets: map[string][]byte{"kling-linux-amd64": []byte("cambiado"), "kling-guest-linux-amd64": guest}}, "sha256 mismatch"},
		{"no listado", &releaseSimple{sumas: fmt.Sprintf("%s  otra-cosa\n", sha(bueno)),
			assets: map[string][]byte{"kling-linux-amd64": bueno}}, "not listed"},
		{"asset ausente", &releaseSimple{sumas: sumas, assets: map[string][]byte{}}, "404"},
		{"error del servidor", &releaseSimple{sumas: sumas, assets: map[string][]byte{"kling-linux-amd64": bueno},
			estado: map[string]int{"kling-linux-amd64": http.StatusInternalServerError}}, "500"},
		{"sin SHA256SUMS", &releaseSimple{assets: map[string][]byte{"kling-linux-amd64": bueno},
			estado: map[string]int{"SHA256SUMS": http.StatusNotFound}}, "SHA256SUMS"},
		// Un SHA256SUMS que no lista nada: nada se acepta sin verificar.
		{"SHA256SUMS vacío", &releaseSimple{sumas: "", assets: map[string][]byte{"kling-linux-amd64": bueno}}, "not listed"},
		{"SHA256SUMS basura", &releaseSimple{sumas: "<html>rate limited</html>\n",
			assets: map[string][]byte{"kling-linux-amd64": bueno}}, "not listed"},
	}
	for _, c := range fallos {
		t.Run(c.nombre, func(t *testing.T) {
			c.rel.etiqueta = "v0.9.0"
			srv := c.rel.servir(t)
			f := &Fuente{Repo: srv.URL, permitirHTTP: true, Avisos: io.Discard}
			dest := filepath.Join(t.TempDir(), "nuevo")
			_, err := f.Bajar(context.Background(), "v0.9.0", []Asset{{Nombre: "kling-linux-amd64"}}, dest)
			if err == nil || !strings.Contains(err.Error(), c.quiero) {
				t.Fatalf("err = %v, quiero %q", err, c.quiero)
			}
			if r := restos(t, dest); len(r) != 0 {
				t.Errorf("tras el fallo quedó en dest: %v", r)
			}
		})
	}
}

// Desde FirmaObligatoriaDesde una release sin firma no se acepta, aunque sus
// sumas cuadren.
func TestBajarSinFirmaDesde1(t *testing.T) {
	bueno := []byte("binario")
	rel := &releaseSimple{etiqueta: "v1.0.0", sumas: fmt.Sprintf("%s  kling-linux-amd64\n", sha(bueno)),
		assets: map[string][]byte{"kling-linux-amd64": bueno}}
	srv := rel.servir(t)
	f := &Fuente{Repo: srv.URL, permitirHTTP: true, Avisos: io.Discard}
	dest := t.TempDir()
	_, err := f.Bajar(context.Background(), "v1.0.0", []Asset{{Nombre: "kling-linux-amd64"}}, dest)
	if err == nil || !strings.Contains(err.Error(), "signed") {
		t.Fatalf("err = %v, quiero el rechazo por falta de firma", err)
	}
	if r := restos(t, dest); len(r) != 0 {
		t.Errorf("quedó en dest: %v", r)
	}
}

func TestBajarDeUnDirectorio(t *testing.T) {
	bueno := []byte("binario local")
	t.Run("nombre corto", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "kling"), bueno, 0o644)
		os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sha(bueno)+"  kling\n"), 0o644)
		f := &Fuente{Dir: dir, Avisos: io.Discard}
		got, err := f.Bajar(context.Background(), "", []Asset{{Nombre: "kling-darwin-arm64", Corto: "kling"}}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(got["kling-darwin-arm64"]); !bytes.Equal(b, bueno) {
			t.Errorf("contenido = %q", b)
		}
	})
	t.Run("sin SHA256SUMS", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "kling"), bueno, 0o644)
		a := []Asset{{Nombre: "kling-darwin-arm64", Corto: "kling"}}
		dest := t.TempDir()
		_, err := (&Fuente{Dir: dir, Avisos: io.Discard}).Bajar(context.Background(), "", a, dest)
		if err == nil || !strings.Contains(err.Error(), "-insecure") {
			t.Fatalf("err = %v, quiero el rechazo sin -insecure", err)
		}
		if r := restos(t, dest); len(r) != 0 {
			t.Errorf("quedó en dest: %v", r)
		}
		// Con SinVerificar se acepta tal cual.
		if _, err := (&Fuente{Dir: dir, SinVerificar: true, Avisos: io.Discard}).Bajar(context.Background(), "", a, dest); err != nil {
			t.Fatalf("con SinVerificar: %v", err)
		}
	})
	t.Run("no listado", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "kling"), bueno, 0o644)
		os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sha(bueno)+"  otro\n"), 0o644)
		_, err := (&Fuente{Dir: dir, Avisos: io.Discard}).Bajar(context.Background(), "", []Asset{{Nombre: "kling-darwin-arm64", Corto: "kling"}}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "not listed") {
			t.Fatalf("err = %v, quiero not listed", err)
		}
	})
	t.Run("opcional ausente", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(""), 0o644)
		got, err := (&Fuente{Dir: dir, Avisos: io.Discard}).Bajar(context.Background(), "", []Asset{{Nombre: "kling-hv", Opcional: true}}, t.TempDir())
		if err != nil || len(got) != 0 {
			t.Fatalf("got = %v, %v; quiero nada y sin error", got, err)
		}
	})
}

// Pasarse del límite es un error, no un truncado: un binario cortado tendría
// otro hash y el fallo despistaría.
func TestCopiarConHashLimite(t *testing.T) {
	for _, c := range []struct {
		n     int
		error bool
	}{{0, false}, {9, false}, {10, false}, {11, true}, {1000, true}} {
		var w bytes.Buffer
		h, err := copiarConHash(&w, bytes.NewReader(bytes.Repeat([]byte("x"), c.n)), 10, "asset")
		if c.error {
			if err == nil {
				t.Errorf("%d bytes con límite 10: aceptado", c.n)
			}
			continue
		}
		if err != nil || h != sha(bytes.Repeat([]byte("x"), c.n)) {
			t.Errorf("%d bytes: hash %q, %v", c.n, h, err)
		}
	}
	// Un error de lectura llega tal cual.
	malo := errors.New("se cortó")
	if _, err := copiarConHash(io.Discard, io.MultiReader(strings.NewReader("abc"), &lectorRoto{malo}), 10, "x"); !errors.Is(err, malo) {
		t.Errorf("err = %v, quiero %v", err, malo)
	}
}

type lectorRoto struct{ err error }

func (l *lectorRoto) Read([]byte) (int, error) { return 0, l.err }

func TestLeerLimitado(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "SHA256SUMS")
	os.WriteFile(p, bytes.Repeat([]byte("a"), 16), 0o644)
	if b, err := leerLimitado(p, 16); err != nil || len(b) != 16 {
		t.Errorf("justo en el límite: %d bytes, %v", len(b), err)
	}
	if _, err := leerLimitado(p, 15); err == nil {
		t.Error("un byte de más se aceptó")
	}
	if _, err := leerLimitado(filepath.Join(dir, "no"), 16); !os.IsNotExist(err) {
		t.Errorf("err = %v, quiero que no existe", err)
	}
}
