package upgrade

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestBlake2bVectores(t *testing.T) {
	// RFC 7693 apéndice A ("abc") y hashlib.blake2b de bytes(i%251) para
	// longitudes alrededor del bloque de 128.
	if got := hex.EncodeToString(blake2b([]byte("abc"), 64)); got != "ba80a53f981c4d0d6a2797b69f12f6e94c212f14685ac4b74b12bb6fdbffa2d17d87c5392aab792dc252d5de4533cc9518d38aa8dbf1925ab92386edd4009923" {
		t.Fatalf("abc: %s", got)
	}
	casos := []struct {
		n          int
		h512, h256 string
	}{
		{0, "786a02f742015903c6c6fd852552d272912f4740e15847618a86e217f71f5419d25e1031afee585313896444934eb04b903a685b1448b755d56f701afe9be2ce", "0e5751c026e543b2e8ab2eb06099daa1d1e5df47778f7787faab45cdf12fe3a8"},
		{127, "b6292669ccd38d5f01caae96ba272c76a879a45743afa0725d83b9ebb26665b731f1848c52f11972b6644f554c064fa90780dbbbf3a89d4fc31f67df3e5857ef", "f2fe67ff342e21b8f45e8f2e0bcd1d9243245d50ee6c78042e9c491388791c72"},
		{128, "2319e3789c47e2daa5fe807f61bec2a1a6537fa03f19ff32e87eecbfd64b7e0e8ccff439ac333b040f19b0c4ddd11a61e24ac1fe0f10a039806c5dcc0da3d115", "c3582f71ebb2be66fa5dd750f80baae97554f3b015663c8be377cfcb2488c1d1"},
		{129, "f59711d44a031d5f97a9413c065d1e614c417ede998590325f49bad2fd444d3e4418be19aec4e11449ac1a57207898bc57d76a1bcf3566292c20c683a5c4648f", "f7f3c46ba2564ff4c4c162da1f5b605f9f1c4aa6a20652a9f9a337c1a2f5b9c9"},
		{256, "93463ac058b6163eb43be3f5bb32b28541498f4e3366f1effe253ad44e1e076e41c3616046027c82a7124f8f4746668ad10b12e8e25a95ac8f3151df01cd5a93", "582f782226018ec33076bd8d1c42413530ac7e1126260ffc0f306ba3befc3f24"},
		{1000, "c11e1c0340bd7e5a1b275f1230c962fad215ecb1391486e74e31b960a2f2996381a5fad092da06841d5f26e38f6ecfeaf441acbcd1c2de61aef121e7927175f5", "b372d0608f720c8c3dd41e9c8eecb10143b41abe520b616607e754bf79c08331"},
	}
	for _, c := range casos {
		d := make([]byte, c.n)
		for i := range d {
			d[i] = byte(i % 251)
		}
		if got := hex.EncodeToString(blake2b(d, 64)); got != c.h512 {
			t.Errorf("n=%d 512: %s", c.n, got)
		}
		if got := hex.EncodeToString(blake2b(d, 32)); got != c.h256 {
			t.Errorf("n=%d 256: %s", c.n, got)
		}
	}
}

// claveTest lee un par de testdata/firma: SOLO de test, nunca de confianza fuera.
func claveTest(t *testing.T, n string) (ClavePrivada, ClavePublica) {
	t.Helper()
	sk, err := os.ReadFile(filepath.Join("testdata", "firma", "test-"+n+".key"))
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParsearClavePrivada(sk)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := os.ReadFile(filepath.Join("testdata", "firma", "test-"+n+".pub"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsearClavePublica(string(pk))
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != k.Publica().String() {
		t.Fatalf("test-%s.pub no es la pública de test-%s.key", n, n)
	}
	return k, p
}

func TestClavesDeTestNoSonDeConfianza(t *testing.T) {
	_, a := claveTest(t, "a")
	_, b := claveTest(t, "b")
	for _, c := range ClavesRelease() {
		if c.String() == a.String() || c.String() == b.String() {
			t.Fatal("una clave de testdata está en clavesRelease")
		}
	}
}

func TestFirmaValidaYAlteraciones(t *testing.T) {
	ka, pa := claveTest(t, "a")
	_, pb := claveTest(t, "b")
	datos := []byte("abc  kling-linux-amd64\n")
	for _, prehash := range []bool{true, false} {
		firma, err := firmarCon(ka, datos, "timestamp:1\tfile:SHA256SUMS\ttag:v0.21.0", prehash)
		if err != nil {
			t.Fatal(err)
		}
		c, err := VerificarFirma(datos, firma, []ClavePublica{pa})
		if err != nil || CampoComentario(c, "tag") != "v0.21.0" {
			t.Fatalf("prehash=%v: %q %v", prehash, c, err)
		}
		// Otra clave.
		if _, err := VerificarFirma(datos, firma, []ClavePublica{pb}); !errors.Is(err, ErrClaveDesconocida) {
			t.Fatalf("otra clave: %v", err)
		}
		// Misma id y otra clave (una id falsificada) tampoco pasa.
		falsa := ClavePublica{ID: pa.ID, Clave: pb.Clave}
		if _, err := VerificarFirma(datos, firma, []ClavePublica{falsa}); err == nil {
			t.Fatal("id de A con la clave de B aceptada")
		}
		// SHA256SUMS alterado.
		if _, err := VerificarFirma(append(bytes.Clone(datos), 'x'), firma, []ClavePublica{pa}); err == nil {
			t.Fatal("datos alterados aceptados")
		}
		// Comentario de confianza alterado (otra etiqueta).
		otra := bytes.Replace(firma, []byte("tag:v0.21.0"), []byte("tag:v0.22.0"), 1)
		if _, err := VerificarFirma(datos, otra, []ClavePublica{pa}); err == nil {
			t.Fatal("comentario alterado aceptado")
		}
	}
}

func TestFicherosClave(t *testing.T) {
	ka, pa := claveTest(t, "a")
	// Ida y vuelta del fichero secreto y del público.
	k2, err := ParsearClavePrivada(ka.Fichero())
	if err != nil || !bytes.Equal(k2.Clave, ka.Clave) || k2.ID != ka.ID {
		t.Fatalf("ida y vuelta: %v", err)
	}
	p2, err := ParsearClavePublica(pa.String())
	if err != nil || p2.String() != pa.String() {
		t.Fatalf("pública: %v", err)
	}
	if !regexp.MustCompile(`^RW[A-Za-z0-9+/]{54}$`).MatchString(pa.String()) {
		t.Fatalf("no parece una clave de minisign: %s", pa)
	}
	// Una suma de control rota se rechaza.
	roto := ka.Fichero()
	i := bytes.IndexByte(roto, '\n') + 20
	roto[i] ^= 1
	if _, err := ParsearClavePrivada(roto); err == nil {
		t.Fatal("clave secreta alterada aceptada")
	}
}

// releaseFirmada sirve SHA256SUMS y, si firma no es nil, SHA256SUMS.minisig.
func releaseFirmada(t *testing.T, etiqueta string, sumas, firma []byte) *Fuente {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/download/" + etiqueta + "/SHA256SUMS":
			w.Write(sumas)
		case "/releases/download/" + etiqueta + "/" + NombreFirma:
			if firma == nil {
				http.NotFound(w, r)
				return
			}
			w.Write(firma)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Fuente{Repo: srv.URL, permitirHTTP: true}
}

func TestPoliticaDeFirma(t *testing.T) {
	ka, pa := claveTest(t, "a")
	kb, pb := claveTest(t, "b")
	sumas := []byte(sha([]byte("x")) + "  kling-linux-amd64\n")
	firmar := func(k ClavePrivada, tag string) []byte {
		f, err := Firmar(k, sumas, fmt.Sprintf("timestamp:1\tfile:SHA256SUMS\ttag:%s\thashed", tag))
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	casos := []struct {
		nombre, etiqueta, propia string
		firma                    []byte
		datos                    []byte // nil = sumas
		claves                   []ClavePublica
		ok, aviso                bool
	}{
		{"firma valida", "v0.21.0", "", firmar(ka, "v0.21.0"), nil, []ClavePublica{pa}, true, false},
		{"firma de otra clave", "v0.21.0", "", firmar(kb, "v0.21.0"), nil, []ClavePublica{pa}, false, false},
		{"SHA256SUMS alterado", "v0.21.0", "", firmar(ka, "v0.21.0"), []byte(sha([]byte("y")) + "  kling-linux-amd64\n"), []ClavePublica{pa}, false, false},
		{"firma de otra release", "v0.21.0", "", firmar(ka, "v0.20.0"), nil, []ClavePublica{pa}, false, false},
		{"sin firma antes de 1.0", "v0.21.0", "v0.21.0", nil, nil, []ClavePublica{pa}, true, true},
		{"sin firma en 1.0", "v1.0.0", "v0.21.0", nil, nil, []ClavePublica{pa}, false, false},
		{"sin firma en un rc de 1.0", "v1.0.0-rc1", "", nil, nil, []ClavePublica{pa}, false, false},
		{"sin firma desde un kling 1.x", "v0.21.0", "v1.2.0", nil, nil, []ClavePublica{pa}, false, false},
		{"rotacion: firma vieja", "v0.21.0", "", firmar(ka, "v0.21.0"), nil, []ClavePublica{pa, pb}, true, false},
		{"rotacion: firma nueva", "v0.21.0", "", firmar(kb, "v0.21.0"), nil, []ClavePublica{pa, pb}, true, false},
		{"firmada sin claves antes de 1.0", "v0.21.0", "", firmar(ka, "v0.21.0"), nil, []ClavePublica{}, true, true},
		{"firmada sin claves en 1.0", "v1.0.0", "", firmar(ka, "v1.0.0"), nil, []ClavePublica{}, false, false},
		{"firma basura", "v1.0.0", "", []byte("hola\n"), nil, []ClavePublica{pa}, false, false},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			datos := c.datos
			if datos == nil {
				datos = sumas
			}
			f := releaseFirmada(t, c.etiqueta, datos, c.firma)
			var avisos bytes.Buffer
			f.Avisos, f.VersionPropia, f.claves = &avisos, c.propia, c.claves
			m, err := f.sumas(context.Background(), c.etiqueta)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, ok esperado %v", err, c.ok)
			}
			if c.ok && m["kling-linux-amd64"] == "" {
				t.Fatalf("sumas: %v", m)
			}
			if got := strings.Contains(avisos.String(), "warning:"); got != c.aviso {
				t.Fatalf("aviso = %q", avisos.String())
			}
		})
	}
}

func TestFirmaEnDirectorio(t *testing.T) {
	ka, pa := claveTest(t, "a")
	dir := t.TempDir()
	sumas := []byte(sha([]byte("x")) + "  kling\n")
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), sumas, 0o644); err != nil {
		t.Fatal(err)
	}
	var avisos bytes.Buffer
	// Sin firma: se acepta con aviso, también desde un kling 1.x.
	f := &Fuente{Dir: dir, Avisos: &avisos, VersionPropia: "v1.0.0", claves: []ClavePublica{pa}}
	if _, err := f.sumas(context.Background(), ""); err != nil || !strings.Contains(avisos.String(), "warning:") {
		t.Fatalf("sin firma: %v %q", err, avisos.String())
	}
	// Con firma válida (sin comprobar etiqueta: un directorio no la tiene).
	firma, err := Firmar(ka, sumas, "tag:v0.30.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, NombreFirma), firma, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sumas(context.Background(), ""); err != nil {
		t.Fatalf("firma válida: %v", err)
	}
	// Alterado: error.
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), append(sumas, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sumas(context.Background(), ""); err == nil {
		t.Fatal("SHA256SUMS alterado aceptado")
	}
}

func TestFirmaCaidaNoEsSinFirma(t *testing.T) {
	// Un 500 al pedir la firma no rebaja a «sin firma».
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/SHA256SUMS") {
			w.Write([]byte(sha([]byte("x")) + "  kling-linux-amd64\n"))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	f := &Fuente{Repo: srv.URL, permitirHTTP: true, Avisos: &bytes.Buffer{}, claves: []ClavePublica{}}
	if _, err := f.sumas(context.Background(), "v0.21.0"); err == nil {
		t.Fatal("un 500 en la firma se tomó por release sin firmar")
	}
}

func TestVersionAlMenos(t *testing.T) {
	for _, c := range []struct {
		v  string
		ok bool
	}{{"v1.0.0", true}, {"v1.0.0-rc.1", true}, {"v0.99.9", false}, {"v2.0.0", true}, {"1.0.1", true}, {"dev", false}, {"", false}, {"v0.20.0-3-gabc", false}} {
		if got := versionAlMenos(c.v, FirmaObligatoriaDesde); got != c.ok {
			t.Errorf("%q: %v", c.v, got)
		}
	}
}

// Las claves de install.sh y las de clavesRelease son las mismas: si no,
// install.sh rechazaría (o aceptaría) lo que kling upgrade no.
func TestClavesInstallSHIgualesQueLasDeKling(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^KLING_RELEASE_PUBKEYS="([^"]*)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal(`install.sh no define KLING_RELEASE_PUBKEYS="…"`)
	}
	sh := strings.Fields(string(m[1]))
	if strings.Join(sh, " ") != strings.Join(clavesRelease, " ") {
		t.Fatalf("install.sh %v, clavesRelease %v", sh, clavesRelease)
	}
	_ = ClavesRelease() // y que se leen
}
