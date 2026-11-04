package upgrade

import (
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Prueba cruzada con el minisign de verdad (el que usa install.sh cuando está
// y el que tendría un usuario que verifica a mano, docs/firmas.md). Sin el
// binario en el PATH se salta; en CI lo instala el job `modulo` del núcleo.

func minisignBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("minisign")
	if err != nil {
		t.Skip("minisign is not on PATH")
	}
	return bin
}

// minisign ejecuta bin con args y la entrada vacía (una clave cifrada pediría
// contraseña: así falla en vez de colgarse).
func minisign(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdin = bytes.NewReader(nil)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("minisign %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

const sumasCruce = "0123456789abcdef  kling-linux-amd64\nfedcba9876543210  kling-darwin-arm64\n"

// Lo que firma el paso de release.yml (Firmar, con prehash) lo acepta
// `minisign -V`, comentario de confianza incluido; y la clave secreta de
// Fichero la lee `minisign -S` y su firma la verifica VerificarFirma.
func TestMinisignVerificaNuestraFirma(t *testing.T) {
	bin := minisignBin(t)
	dir := t.TempDir()
	k, err := GenerarClave(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := filepath.Join(dir, "kling-release.pub")
	datos := filepath.Join(dir, "SHA256SUMS")
	firma := datos + ".minisig"
	if err := os.WriteFile(pub, k.Publica().Fichero(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(datos, []byte(sumasCruce), 0o644); err != nil {
		t.Fatal(err)
	}
	comentario := "timestamp:1767225600\tfile:SHA256SUMS\ttag:v1.0.0"
	f, err := Firmar(k, []byte(sumasCruce), comentario)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firma, f, 0o644); err != nil {
		t.Fatal(err)
	}
	out := minisign(t, bin, "-V", "-p", pub, "-m", datos, "-x", firma)
	if !strings.Contains(out, comentario) {
		t.Fatalf("minisign -V did not print the trusted comment:\n%s", out)
	}
	// Con la línea base64 (la de install.sh, `minisign -P`) también.
	minisign(t, bin, "-V", "-P", k.Publica().String(), "-m", datos, "-x", firma, "-q")

	// Y una alteración la rechaza minisign, no solo nosotros.
	if err := os.WriteFile(datos, []byte(sumasCruce+"x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-V", "-p", pub, "-m", datos, "-x", firma)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("minisign accepted an altered SHA256SUMS:\n%s", out)
	}
	if err := os.WriteFile(datos, []byte(sumasCruce), 0o644); err != nil {
		t.Fatal(err)
	}

	// La clave secreta en el formato de Fichero (la de scripts/firma-clave.sh)
	// es una de `minisign -G -W`: minisign firma con ella.
	sec := filepath.Join(dir, "kling-release.key")
	if err := os.WriteFile(sec, k.Fichero(), 0o600); err != nil {
		t.Fatal(err)
	}
	firma2 := filepath.Join(dir, "con-minisign.minisig")
	minisign(t, bin, "-S", "-s", sec, "-m", datos, "-x", firma2, "-t", "tag:v1.0.1")
	b, err := os.ReadFile(firma2)
	if err != nil {
		t.Fatal(err)
	}
	c, err := VerificarFirma([]byte(sumasCruce), b, []ClavePublica{k.Publica()})
	if err != nil || CampoComentario(c, "tag") != "v1.0.1" {
		t.Fatalf("minisign signature with our secret key: %q %v", c, err)
	}
}

// Una clave hecha por minisign (-G -W, sin cifrar) y su firma (-S, con el
// prehash por defecto): ParsearClavePublica lee su .pub y VerificarFirma
// acepta la firma y su comentario; ParsearClavePrivada lee su clave secreta y
// lo que firmamos con ella lo acepta `minisign -V`.
func TestNuestroVerificadorAceptaMinisign(t *testing.T) {
	bin := minisignBin(t)
	dir := t.TempDir()
	pub := filepath.Join(dir, "k.pub")
	sec := filepath.Join(dir, "k.key")
	minisign(t, bin, "-G", "-W", "-p", pub, "-s", sec)
	datos := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(datos, []byte(sumasCruce), 0o644); err != nil {
		t.Fatal(err)
	}
	firma := datos + ".minisig"
	minisign(t, bin, "-S", "-s", sec, "-m", datos, "-x", firma, "-t", "timestamp:1\tfile:SHA256SUMS\ttag:v1.0.0")

	pb, err := os.ReadFile(pub)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsearClavePublica(string(pb))
	if err != nil {
		t.Fatalf("minisign's .pub: %v", err)
	}
	f, err := os.ReadFile(firma)
	if err != nil {
		t.Fatal(err)
	}
	c, err := VerificarFirma([]byte(sumasCruce), f, []ClavePublica{p})
	if err != nil {
		t.Fatalf("minisign's signature: %v", err)
	}
	if CampoComentario(c, "tag") != "v1.0.0" || CampoComentario(c, "file") != "SHA256SUMS" {
		t.Fatalf("trusted comment: %q", c)
	}
	if _, err := VerificarFirma([]byte(sumasCruce+"x"), f, []ClavePublica{p}); err == nil {
		t.Fatal("altered SHA256SUMS accepted")
	}
	// Otra clave (la nuestra de test) no la acepta: la id no es de confianza.
	_, otra := claveTest(t, "a")
	if _, err := VerificarFirma([]byte(sumasCruce), f, []ClavePublica{otra}); err == nil {
		t.Fatal("minisign's signature accepted with another key")
	}

	// Su clave secreta la leemos y lo firmado con ella lo verifica minisign.
	sb, err := os.ReadFile(sec)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParsearClavePrivada(sb)
	if err != nil {
		t.Fatalf("minisign's secret key: %v", err)
	}
	if k.Publica().String() != p.String() {
		t.Fatalf("the secret key read gives another public key: %s vs %s", k.Publica(), p)
	}
	nuestra, err := Firmar(k, []byte(sumasCruce), "tag:v1.0.2")
	if err != nil {
		t.Fatal(err)
	}
	firma3 := filepath.Join(dir, "nuestra.minisig")
	if err := os.WriteFile(firma3, nuestra, 0o644); err != nil {
		t.Fatal(err)
	}
	minisign(t, bin, "-V", "-p", pub, "-m", datos, "-x", firma3, "-q")
}
