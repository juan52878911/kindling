package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/pkg/api"
)

const passPrueba = "s3cr3t-registry-pass"

// login, ls y logout: el fichero es 0600, la lista no lleva contraseñas y una
// petición mala no cita la que traía.
func TestRegistriesAPI(t *testing.T) {
	s, h := testServer(t)
	if rr := call(t, h, "GET", "/registries", ""); rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("sin credenciales: %d %s", rr.Code, rr.Body)
	}
	rr := call(t, h, "POST", "/registries", `{"host":"https://GHCR.io/","username":"juan","password":"`+passPrueba+`"}`)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"host":"ghcr.io"`) || strings.Contains(rr.Body.String(), passPrueba) {
		t.Fatalf("login: %d %s", rr.Code, rr.Body)
	}
	call(t, h, "POST", "/registries", `{"host":"localhost:5000","username":"a","password":"b"}`)
	st, err := os.Stat(s.rutaRegistros())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("registries.json: %v %v", st, err)
	}
	rr = call(t, h, "GET", "/registries", "")
	var l []api.Registry
	json.Unmarshal(rr.Body.Bytes(), &l)
	if len(l) != 2 || l[0].Host != "ghcr.io" || l[0].Username != "juan" || l[1].Host != "localhost:5000" ||
		strings.Contains(rr.Body.String(), passPrueba) {
		t.Fatalf("ls: %s", rr.Body)
	}

	for _, body := range []string{
		`{"host":"ghcr.io","username":"juan","password":""}`,
		`{"host":"ghcr.io","username":"ju:an","password":"` + passPrueba + `"}`,
		`{"host":"ghcr.io","username":"juan","password":"` + passPrueba + `\n"}`,
		`{"host":"a b","username":"juan","password":"` + passPrueba + `"}`,
		`{"host":"ghcr.io","password":` + passPrueba + `}`,
	} {
		rr := call(t, h, "POST", "/registries", body)
		if rr.Code != 400 || strings.Contains(rr.Body.String(), passPrueba) {
			t.Errorf("%s: %d %s", body, rr.Code, rr.Body)
		}
	}

	if rr := call(t, h, "DELETE", "/registries/ghcr.io", ""); rr.Code != 204 {
		t.Fatalf("logout: %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "DELETE", "/registries/ghcr.io", ""); rr.Code != 404 {
		t.Fatalf("logout otra vez: %d %s", rr.Code, rr.Body)
	}
	if b, _ := os.ReadFile(s.rutaRegistros()); strings.Contains(string(b), passPrueba) {
		t.Fatalf("tras el logout sigue la contraseña: %s", b)
	}
}

// Una construcción oci recibe SOLO las credenciales del registro de su
// referencia, en un fichero 0600 de su directorio de trabajo, nunca en el
// entorno; y no acaban en la receta ni en la respuesta. Una de otro registro
// no recibe ninguna.
func TestConstruccionRecibeSoloSusCredenciales(t *testing.T) {
	s, h := testServer(t)
	os.MkdirAll(filepath.Join(s.root, "images"), 0o755)
	bdir := t.TempDir()
	t.Setenv("KLING_BUILDERS_DIR", bdir)
	t.Setenv("KLING_BUILDERS_INSECURE", "1")
	copia := filepath.Join(t.TempDir(), "auth")
	t.Setenv("KLING_TEST_AUTH", copia)
	instalarConstructor(t, bdir, "oci", `
set -e
rm -f "$KLING_TEST_AUTH"
if [ -f "$1/registry-auth.json" ]; then
  cp "$1/registry-auth.json" "$KLING_TEST_AUTH"
  stat -c %a "$1/registry-auth.json" > "$KLING_TEST_AUTH.mode" 2>/dev/null || stat -f %Lp "$1/registry-auth.json" > "$KLING_TEST_AUTH.mode"
fi
env > "$KLING_TEST_AUTH.env"
mkdir -p "$KLING_OUT_DIR"
echo img > "$KLING_OUT_DIR/$KLING_IMAGE_NAME.ext4"
`)
	call(t, h, "POST", "/registries", `{"host":"localhost:5000","username":"juan","password":"`+passPrueba+`"}`)
	call(t, h, "POST", "/registries", `{"host":"ghcr.io","username":"otro","password":"otra-pass"}`)

	rr := call(t, h, "POST", "/images", `{"name":"priv","builder":"oci","spec":{"ref":"localhost:5000/priv/app:v1"}}`)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), passPrueba) {
		t.Fatalf("build: %d %s", rr.Code, rr.Body)
	}
	b, err := os.ReadFile(copia)
	if err != nil {
		t.Fatalf("el constructor no recibió las credenciales: %v", err)
	}
	var m map[string]oci.Credential
	if json.Unmarshal(b, &m) != nil || len(m) != 1 || m["localhost:5000"].Password != passPrueba {
		t.Fatalf("credenciales recibidas: %s", b)
	}
	if mode, _ := os.ReadFile(copia + ".mode"); strings.TrimSpace(string(mode)) != "600" {
		t.Fatalf("permisos del fichero de credenciales: %q", mode)
	}
	if env, _ := os.ReadFile(copia + ".env"); strings.Contains(string(env), passPrueba) {
		t.Fatal("la contraseña llegó por el entorno")
	}
	if rec, _ := os.ReadFile(s.recipePath("priv")); strings.Contains(string(rec), passPrueba) || len(rec) == 0 {
		t.Fatalf("receta: %s", rec)
	}
	if left, _ := filepath.Glob(filepath.Join(s.root, "build", "priv.*")); len(left) > 0 {
		t.Fatalf("quedó el directorio de trabajo: %v", left)
	}

	if rr := call(t, h, "POST", "/images", `{"name":"pub","builder":"oci","spec":{"ref":"docker.io/library/redis:7"}}`); rr.Code != 200 {
		t.Fatalf("build pub: %d %s", rr.Code, rr.Body)
	}
	if _, err := os.Stat(copia); !os.IsNotExist(err) {
		t.Fatal("una imagen de otro registro recibió credenciales")
	}

	// Un archivo importado cuya etiqueta (sacada del propio archivo) nombra
	// el registro privado: es offline, no recibe ninguna.
	if rr := call(t, h, "POST", "/images", `{"name":"arch","builder":"oci","spec":{"ref":"localhost:5000/priv/app:v1",`+
		`"source":"archive","digest":"sha256:`+strings.Repeat("a", 64)+`"}}`); rr.Code != 200 {
		t.Fatalf("build arch: %d %s", rr.Code, rr.Body)
	}
	if _, err := os.Stat(copia); !os.IsNotExist(err) {
		t.Fatal("una construcción de un archivo recibió credenciales")
	}
}
