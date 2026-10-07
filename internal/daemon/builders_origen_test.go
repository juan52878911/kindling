package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// De punta a punta, como root en Linux y con un constructor sin privilegios
// de verdad (un uid que no usa nadie más: al acabar se matan todos sus
// procesos): una construcción solo lee la verificada de todos y la de su
// origen. Lo que bajó una imagen de un registro con credenciales va a la de
// ese registro y no pasa por la caché compartida del constructor; un archivo
// lee sus capas de la suya y no la caché de blobs de root; y una imagen
// pública no llega a ninguna de las dos.
func TestConstruccionSoloLeeSuOrigen(t *testing.T) {
	if os.Geteuid() != 0 || runtime.GOOS != "linux" {
		t.Skip("solo como root en Linux")
	}
	u := &usuarioConstructor{Nombre: "kling-test", UID: 64917, GID: 64917}
	s, h := testServer(t)
	s.constructor = u
	for d := s.root; d != "/"; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && fi.Mode().Perm()&0o001 == 0 {
			os.Chmod(d, fi.Mode().Perm()|0o011)
		}
	}
	os.MkdirAll(filepath.Join(s.root, "images"), 0o755)
	bdir := filepath.Join(s.root, "builders")
	os.MkdirAll(bdir, 0o755)
	t.Setenv("KLING_BUILDERS_DIR", bdir)
	t.Setenv("KLING_BUILDERS_INSECURE", "1")
	salida := filepath.Join(s.root, "salida")
	os.MkdirAll(salida, 0o777)
	os.Chmod(salida, 0o777)

	// Lo que ya hay: una capa en la verificada de otro registro, una en la de
	// un archivo, una en la caché de blobs de root y una en la del registro
	// que se va a construir.
	if rr := call(t, h, "POST", "/registries", `{"host":"localhost:5000","username":"a","password":"b"}`); rr.Code != 200 {
		t.Fatalf("login: %d %s", rr.Code, rr.Body)
	}
	os.MkdirAll(filepath.Join(s.root, "cache"), 0o755)
	v, err := prepararVerificada(s.root, u)
	if err != nil {
		t.Fatal(err)
	}
	plantar := func(dir string, cuerpo string) string {
		os.MkdirAll(dir, 0o750)
		p := filepath.Join(dir, strings.TrimPrefix(digestDe([]byte(cuerpo)), "sha256:"))
		if err := os.WriteFile(p, []byte(cuerpo), 0o640); err != nil {
			t.Fatal(err)
		}
		os.Lchown(p, 0, int(u.GID))
		return p
	}
	abrir := func(ambito string) string {
		d, err := abrirAmbito(v, ambito, u)
		if err != nil {
			t.Fatal(err)
		}
		cerrarAmbito(d)
		return d
	}
	deGhcr := plantar(filepath.Join(abrir(ambitoRegistro("ghcr.io")), "oci", "sha256"), "capa privada de ghcr")
	deArchivo := plantar(filepath.Join(abrir(ambitoArchivo("sha256:"+strings.Repeat("9", 64))), "oci", "sha256"), "capa de otro archivo")
	propio := plantar(filepath.Join(abrir(ambitoRegistro("localhost:5000")), "oci", "sha256"), "capa ya verificada de localhost")
	if err := cerrarCacheOCI(s.root); err != nil {
		t.Fatal(err)
	}
	deRoot := plantar(dirCacheOCI(s.root), "capa subida de un archivo")
	os.Chmod(deRoot, 0o600)

	nueva := func(n string) (cuerpo, hex string) {
		cuerpo = "capa nueva de " + n
		return cuerpo, strings.TrimPrefix(digestDe([]byte(cuerpo)), "sha256:")
	}
	constructor := func(nombre string, leer map[string]string) {
		cuerpo, hex := nueva(nombre)
		var b strings.Builder
		fmt.Fprintf(&b, "o=%s/%s\nrm -rf $o; mkdir -p $o\n", salida, nombre)
		b.WriteString(`echo "$KLING_CACHE_DIR" > $o/cache; echo "$KLING_VERIFIED_SCOPE_DIR" > $o/scope` + "\n")
		for k, p := range leer {
			fmt.Fprintf(&b, "if cat %s >/dev/null 2>&1; then echo ok > $o/%s; else echo no > $o/%s; fi\n", p, k, k)
		}
		fmt.Fprintf(&b, "mkdir -p \"$KLING_CACHE_DIR/oci/sha256\"\nprintf '%%s' '%s' > \"$KLING_CACHE_DIR/oci/sha256/%s\"\n", cuerpo, hex)
		fmt.Fprintf(&b, "echo sha256:%s > \"$1/cache-used\"\n", hex)
		b.WriteString("mkdir -p \"$KLING_OUT_DIR\"\necho img > \"$KLING_OUT_DIR/$KLING_IMAGE_NAME.ext4\"\n")
		instalarConstructor(t, bdir, "oci", b.String())
	}
	leido := func(nombre, k string) string {
		b, _ := os.ReadFile(filepath.Join(salida, nombre, k))
		return strings.TrimSpace(string(b))
	}
	ajenas := map[string]string{"ghcr": deGhcr, "archivo": deArchivo, "root": deRoot}
	conPropia := func() map[string]string {
		m := map[string]string{"propia": propio}
		for k, p := range ajenas {
			m[k] = p
		}
		return m
	}

	// Una de localhost:5000 (con credenciales).
	constructor("priv", conPropia())
	if rr := call(t, h, "POST", "/images", `{"name":"priv","builder":"oci","spec":{"ref":"localhost:5000/priv/app:v1"}}`); rr.Code != 200 {
		t.Fatalf("priv: %d %s", rr.Code, rr.Body)
	}
	alcance := filepath.Join(v, ambitoRegistro("localhost:5000"))
	if leido("priv", "propia") != "ok" || leido("priv", "scope") != alcance {
		t.Fatalf("priv no lee lo de su registro: %q %q", leido("priv", "propia"), leido("priv", "scope"))
	}
	if c := leido("priv", "cache"); !strings.HasPrefix(c, filepath.Join(s.root, "build")+"/") {
		t.Fatalf("priv usa la caché compartida del constructor: %s", c)
	}
	for k := range ajenas {
		if leido("priv", k) != "no" {
			t.Errorf("priv leyó %s", k)
		}
	}
	_, hexPriv := nueva("priv")
	if _, err := os.Lstat(filepath.Join(alcance, "oci", "sha256", hexPriv)); err != nil {
		t.Error("lo bajado por priv no llegó a la verificada de su registro")
	}
	for _, p := range []string{filepath.Join(v, "oci", "sha256", hexPriv), filepath.Join(s.root, "cache", "builder", "oci", "sha256", hexPriv)} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("lo bajado por priv está en %s", p)
		}
	}
	if fi, _ := os.Lstat(alcance); fi.Mode().Perm() != 0o700 {
		t.Errorf("la verificada del registro se quedó %o", fi.Mode().Perm())
	}

	// Una pública: ni lo de localhost:5000 ni nada ajeno.
	constructor("pub", conPropia())
	if rr := call(t, h, "POST", "/images", `{"name":"pub","builder":"oci","spec":{"ref":"docker.io/library/redis:7"}}`); rr.Code != 200 {
		t.Fatalf("pub: %d %s", rr.Code, rr.Body)
	}
	if leido("pub", "scope") != "" || leido("pub", "cache") != filepath.Join(s.root, "cache", "builder") {
		t.Fatalf("pub: scope %q cache %q", leido("pub", "scope"), leido("pub", "cache"))
	}
	for k := range conPropia() {
		if leido("pub", k) != "no" {
			t.Errorf("pub leyó %s", k)
		}
	}
	_, hexPub := nueva("pub")
	if _, err := os.Lstat(filepath.Join(v, "oci", "sha256", hexPub)); err != nil {
		t.Error("lo bajado por pub no llegó a la verificada de todos")
	}

	// Un archivo: su manifiesto y su capa, subidos, los lee de la suya.
	capa := "capa del archivo"
	man := `{"schemaVersion":2,"config":{"digest":"` + digestDe([]byte("{}")) + `"},"layers":[{"digest":"` + digestDe([]byte(capa)) + `"}]}`
	for _, b := range []string{man, "{}", capa} {
		if rr := putOCIBlob(s, digestDe([]byte(b)), strings.NewReader(b), int64(len(b))); rr.Code != http.StatusCreated {
			t.Fatalf("PUT: %d %s", rr.Code, rr.Body)
		}
	}
	dirArch := filepath.Join(v, ambitoArchivo(digestDe([]byte(man))))
	leer := conPropia()
	leer["capa"] = filepath.Join(dirArch, "oci", "sha256", strings.TrimPrefix(digestDe([]byte(capa)), "sha256:"))
	constructor("arch", leer)
	if rr := call(t, h, "POST", "/images", `{"name":"arch","builder":"oci","spec":{"source":"archive","digest":"`+digestDe([]byte(man))+`"}}`); rr.Code != 200 {
		t.Fatalf("arch: %d %s", rr.Code, rr.Body)
	}
	if leido("arch", "capa") != "ok" || leido("arch", "scope") != dirArch {
		t.Fatalf("arch no lee su capa: %q %q", leido("arch", "capa"), leido("arch", "scope"))
	}
	for k := range conPropia() {
		if leido("arch", k) != "no" {
			t.Errorf("arch leyó %s", k)
		}
	}
}
