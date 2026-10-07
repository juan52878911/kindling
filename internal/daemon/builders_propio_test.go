package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain deja que el binario de los tests haga de `kling builder <nombre>`:
// el daemon se ejecuta a sí mismo para los constructores del núcleo, y aquí
// "sí mismo" es este binario. Apunta su argv en $KLING_TEST_ARGV y deja una
// imagen donde la deja un constructor aislado.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "builder" {
		if p := os.Getenv("KLING_TEST_ARGV"); p != "" {
			_ = os.WriteFile(p, []byte(strings.Join(os.Args[1:], " ")), 0o600)
		}
		out := os.Getenv("KLING_OUT_DIR")
		if err := os.MkdirAll(out, 0o755); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(filepath.Join(out, os.Getenv("KLING_IMAGE_NAME")+".ext4"), []byte("propio\n"), 0o644); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Un constructor del núcleo sin root lo hace el propio binario del daemon,
// aunque en el directorio por defecto haya uno instalado: ese lanzaría el
// kling del sistema, y un daemon privado o recién compilado construiría con
// otro binario. Solo un KLING_BUILDERS_DIR explícito manda sobre eso.
func TestConstructorDelNucleoUsaElPropioBinario(t *testing.T) {
	s, h := testServer(t)
	os.MkdirAll(filepath.Join(s.root, "images"), 0o755)
	bdir := t.TempDir()
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("KLING_TEST_ARGV", argv)
	t.Setenv("KLING_BUILDERS_INSECURE", "1")
	// El "instalado": un envoltorio que apunta su argv, como el
	// scripts/builders/oci que hace exec del kling del sistema.
	instalarConstructor(t, bdir, "oci", `
echo "instalado $*" > "$KLING_TEST_ARGV"
mkdir -p "$KLING_OUT_DIR"
echo instalado > "$KLING_OUT_DIR/$KLING_IMAGE_NAME.ext4"
`)
	viejo := buildersDirPorDefecto
	buildersDirPorDefecto = bdir
	t.Cleanup(func() { buildersDirPorDefecto = viejo })

	t.Setenv("KLING_BUILDERS_DIR", "")
	os.Unsetenv("KLING_BUILDERS_DIR")
	if rr := call(t, h, "POST", "/images", `{"name":"a","builder":"oci","spec":{}}`); rr.Code != 200 {
		t.Fatalf("oci: %d %s", rr.Code, rr.Body)
	}
	got, _ := os.ReadFile(argv)
	if !strings.HasPrefix(string(got), "builder oci ") {
		t.Fatalf("sin KLING_BUILDERS_DIR tiene que construir el propio daemon, no el instalado: argv %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(s.root, "images", "a.ext4")); string(b) != "propio\n" {
		t.Fatalf("imagen: %q", b)
	}

	// Con KLING_BUILDERS_DIR puesto a propósito, el que haya allí.
	t.Setenv("KLING_BUILDERS_DIR", bdir)
	if rr := call(t, h, "POST", "/images", `{"name":"b","builder":"oci","spec":{}}`); rr.Code != 200 {
		t.Fatalf("oci instalado: %d %s", rr.Code, rr.Body)
	}
	if got, _ := os.ReadFile(argv); !strings.HasPrefix(string(got), "instalado "+filepath.Join(s.root, "build")) {
		t.Fatalf("con KLING_BUILDERS_DIR manda el instalado: argv %q", got)
	}
	// Y si allí no está, otra vez el propio.
	os.Remove(filepath.Join(bdir, "oci"))
	if rr := call(t, h, "POST", "/images", `{"name":"c","builder":"oci","spec":{}}`); rr.Code != 200 {
		t.Fatalf("oci sin instalar: %d %s", rr.Code, rr.Body)
	}
	if got, _ := os.ReadFile(argv); !strings.HasPrefix(string(got), "builder oci ") {
		t.Fatalf("sin instalar en KLING_BUILDERS_DIR, el propio: argv %q", got)
	}
}

// Con usuario de construcción, el propio binario tiene que estar a su
// alcance: si no, el error lo dice en vez de un "permission denied".
func TestConstructorPropioFueraDeAlcance(t *testing.T) {
	s, h := testServer(t)
	t.Setenv("KLING_BUILDERS_DIR", "")
	os.Unsetenv("KLING_BUILDERS_DIR")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	otro := &usuarioConstructor{Nombre: "kb", UID: 4242, GID: 4242}
	if atravesable(self, otro) {
		t.Skip("el binario de los tests está al alcance de cualquiera")
	}
	s.constructor = otro
	rr := call(t, h, "POST", "/images", `{"name":"a","builder":"oci","spec":{}}`)
	if rr.Code != 412 || !strings.Contains(rr.Body.String(), "can't execute the daemon binary") {
		t.Fatalf("binario fuera de alcance: %d %s", rr.Code, rr.Body)
	}
}
