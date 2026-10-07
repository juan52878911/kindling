package daemon

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// yo es un usuario de construcción con el uid del test: sin root no se puede
// ceder nada a otro.
func yo() *usuarioConstructor {
	return &usuarioConstructor{Nombre: "yo", UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
}

func TestPrepararTrabajo(t *testing.T) {
	root := t.TempDir()
	work, err := prepararTrabajo(root, "pg", []byte(`{"name":"pg"}`), []byte(`{"ghcr.io":{}}`), yo())
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "build")); fi.Mode().Perm() != 0o711 {
		t.Fatalf("build/ con usuario de construcción: %o, quiero 0711 (se atraviesa, no se lista)", fi.Mode().Perm())
	}
	if !strings.HasPrefix(filepath.Base(work), "pg.") {
		t.Fatalf("directorio de trabajo %s", work)
	}
	for _, p := range []string{work, filepath.Join(work, "request.json"), filepath.Join(work, FicheroCredencialesConstructor)} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != uint32(os.Getuid()) {
			t.Fatalf("%s: uid %d", p, st.Uid)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(work, "request.json")); string(b) != `{"name":"pg"}` {
		t.Fatalf("request.json: %s", b)
	}

	if fi, _ := os.Lstat(filepath.Join(work, FicheroCredencialesConstructor)); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v, quiero 0600", FicheroCredencialesConstructor, fi)
	}

	// Sin usuario, como siempre: build/ cerrado.
	root2 := t.TempDir()
	if _, err := prepararTrabajo(root2, "x", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(root2, "build")); fi.Mode().Perm() != 0o700 {
		t.Fatalf("build/ sin usuario: %o", fi.Mode().Perm())
	}
}

func TestPrepararCacheMigra(t *testing.T) {
	root := t.TempDir()
	vieja := filepath.Join(root, "cache", "oci", "sha256")
	os.MkdirAll(vieja, 0o755)
	os.WriteFile(filepath.Join(vieja, "aaaa"), []byte("blob"), 0o644)
	os.WriteFile(filepath.Join(vieja, "bbbb.part"), []byte("a medias"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(vieja, "cccc"))

	d, err := prepararCache(root, yo())
	if err != nil {
		t.Fatal(err)
	}
	if d != filepath.Join(root, "cache", "builder") {
		t.Fatalf("caché %s", d)
	}
	if fi, _ := os.Lstat(d); fi.Mode().Perm() != 0o700 {
		t.Fatalf("caché del constructor %o, quiero 0700", fi.Mode().Perm())
	}
	a, _ := os.Stat(filepath.Join(vieja, "aaaa"))
	b, err := os.Lstat(filepath.Join(d, "oci", "sha256", "aaaa"))
	if err != nil || !os.SameFile(a, b) {
		t.Fatalf("el blob de la caché de root tiene que estar enlazado: %v", err)
	}
	for _, n := range []string{"bbbb.part", "cccc"} {
		if _, err := os.Lstat(filepath.Join(d, "oci", "sha256", n)); err == nil {
			t.Fatalf("%s no se migra", n)
		}
	}
	// La segunda vez no migra ni falla.
	os.WriteFile(filepath.Join(vieja, "dddd"), []byte("nuevo"), 0o644)
	if _, err := prepararCache(root, yo()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(d, "oci", "sha256", "dddd")); err == nil {
		t.Fatal("solo se migra al crear la caché")
	}

	// Un enlace en el sitio de la caché se rechaza.
	root2 := t.TempDir()
	os.MkdirAll(filepath.Join(root2, "cache"), 0o755)
	os.Symlink(t.TempDir(), filepath.Join(root2, "cache", "builder"))
	if _, err := prepararCache(root2, yo()); err == nil {
		t.Fatal("cache/builder como enlace aceptado")
	}
}

func TestAdoptarSalida(t *testing.T) {
	uid := uint32(os.Getuid())
	nuevo := func(t *testing.T) (out, images string) {
		d := t.TempDir()
		out, images = filepath.Join(d, "out"), filepath.Join(d, "images")
		os.Mkdir(out, 0o700)
		os.Mkdir(images, 0o755)
		return
	}

	out, images := nuevo(t)
	os.WriteFile(filepath.Join(out, "pg.ext4"), []byte("ext4"), 0o600)
	os.WriteFile(filepath.Join(out, "otra.ext4"), []byte("no"), 0o600)
	n, err := adoptarSalida(out, images, "pg", uid)
	if err != nil || n != 1 {
		t.Fatalf("adoptar: %d %v", n, err)
	}
	fi, err := os.Lstat(filepath.Join(images, "pg.ext4"))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("imagen movida: %v %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(images, "otra.ext4")); err == nil {
		t.Fatal("solo se mueve <name>.ext4 / <name>.layer.ext4")
	}

	// Sin salida no es error: el daemon dirá luego que no hay imagen.
	if n, err := adoptarSalida(filepath.Join(t.TempDir(), "out"), images, "pg", uid); n != 0 || err != nil {
		t.Fatalf("sin out/: %d %v", n, err)
	}

	casos := map[string]func(out string){
		"enlace simbólico": func(out string) { os.Symlink("/etc/passwd", filepath.Join(out, "pg.ext4")) },
		"enlace duro": func(out string) {
			p := filepath.Join(out, "pg.layer.ext4")
			os.WriteFile(p, []byte("x"), 0o644)
			os.Link(p, filepath.Join(out, "otro"))
		},
		"fifo":       func(out string) { syscall.Mkfifo(filepath.Join(out, "pg.ext4"), 0o600) },
		"directorio": func(out string) { os.Mkdir(filepath.Join(out, "pg.ext4"), 0o700) },
	}
	for nombre, plantar := range casos {
		out, images := nuevo(t)
		plantar(out)
		if _, err := adoptarSalida(out, images, "pg", uid); err == nil {
			t.Fatalf("%s aceptado", nombre)
		}
		if e, _ := os.ReadDir(images); len(e) != 0 {
			t.Fatalf("%s: algo llegó a images/", nombre)
		}
	}

	// De otro dueño.
	out, images = nuevo(t)
	os.WriteFile(filepath.Join(out, "pg.ext4"), []byte("ext4"), 0o600)
	if _, err := adoptarSalida(out, images, "pg", uid+1); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("dueño ajeno: %v", err)
	}

	// out/ como enlace a otro sitio.
	d := t.TempDir()
	otro := t.TempDir()
	os.WriteFile(filepath.Join(otro, "pg.ext4"), []byte("ext4"), 0o600)
	os.Symlink(otro, filepath.Join(d, "out"))
	if _, err := adoptarSalida(filepath.Join(d, "out"), t.TempDir(), "pg", uid); err == nil {
		t.Fatal("out/ como enlace aceptado")
	}
}

func TestRecipeHintsSinEnlaces(t *testing.T) {
	d := t.TempDir()
	ajena := filepath.Join(d, "otra.recipe.json")
	os.WriteFile(ajena, []byte(`{"base":"secreta"}`), 0o600)
	os.Symlink(ajena, filepath.Join(d, "recipe.json"))
	if h, err := readRecipeHints(filepath.Join(d, "recipe.json")); err == nil {
		t.Fatalf("recipe.json como enlace aceptado: %+v", h)
	}
	os.Remove(filepath.Join(d, "recipe.json"))
	syscall.Mkfifo(filepath.Join(d, "recipe.json"), 0o600)
	if _, err := readRecipeHints(filepath.Join(d, "recipe.json")); err == nil {
		t.Fatal("recipe.json como fifo aceptado")
	}
}

// El proceso del constructor nace con el usuario de construcción, sin grupos
// suplementarios, y con un entorno de lista blanca: nada del daemon que no
// haga falta (puede llevar secretos).
func TestComandoConstructorSinRoot(t *testing.T) {
	t.Setenv("KLING_TOKEN", "secreto-del-daemon")
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	u := &usuarioConstructor{Nombre: "kindling-build", UID: 990, GID: 989}
	req := api.BuildImageRequest{Name: "pg", Builder: "oci"}
	cmd := comandoConstructor(context.Background(), "/bin/kling", []string{"builder", "oci"}, "/var/lib/kindling",
		"/var/lib/kindling/build/pg.1", req, true, u, "/var/lib/kindling/cache/builder")

	c := cmd.SysProcAttr.Credential
	if c == nil || c.Uid != 990 || c.Gid != 989 || c.Groups == nil || len(c.Groups) != 0 || c.NoSetGroups {
		t.Fatalf("credencial %+v", c)
	}
	env := strings.Join(cmd.Env, "\n") + "\n"
	for _, quiero := range []string{"HOME=/var/lib/kindling/build/pg.1\n", "KLING_OUT_DIR=/var/lib/kindling/build/pg.1/out\n",
		"KLING_CACHE_DIR=/var/lib/kindling/cache/builder\n", "KLING_BUILD_LIMITS=1\n", "HTTPS_PROXY=http://proxy:3128\n",
		"KLING_IMAGE_NAME=pg\n"} {
		if !strings.Contains(env, quiero) {
			t.Fatalf("falta %q en el entorno:\n%s", quiero, env)
		}
	}
	if strings.Contains(env, "secreto-del-daemon") {
		t.Fatalf("el entorno del daemon llega al constructor:\n%s", env)
	}
	if got := strings.Join(cmd.Args, " "); got != "/bin/kling builder oci /var/lib/kindling/build/pg.1" {
		t.Fatalf("argv %s", got)
	}

	// Sin usuario: el entorno del daemon y su identidad, como siempre.
	cmd = comandoConstructor(context.Background(), "/bin/kling", nil, "/r", "/r/build/pg.1", req, false, nil, "")
	if cmd.SysProcAttr != nil || !strings.Contains(strings.Join(cmd.Env, "\n"), "secreto-del-daemon") ||
		strings.Contains(strings.Join(cmd.Env, "\n"), "KLING_OUT_DIR") {
		t.Fatalf("sin usuario: %+v %v", cmd.SysProcAttr, cmd.Env)
	}
}

// Como root en Linux: el proceso hijo nace de verdad con el uid, el gid y sin
// grupos ni capacidades.
func TestConstructorCredencialDeVerdad(t *testing.T) {
	if os.Geteuid() != 0 || runtime.GOOS != "linux" {
		t.Skip("solo como root en Linux")
	}
	u := &usuarioConstructor{Nombre: "nobody", UID: 65534, GID: 65534}
	work := t.TempDir()
	os.Chmod(filepath.Dir(work), 0o755) // nobody tiene que poder entrar
	os.Chmod(work, 0o755)
	script := `grep -E "^(Uid|Gid|Groups|CapEff):" /proc/self/status; true`
	cmd := comandoConstructor(context.Background(), "/bin/sh", []string{"-c", script},
		"/r", work, api.BuildImageRequest{Name: "x"}, true, u, "/c")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	s := strings.Join(strings.Fields(string(out)), " ")
	if !strings.Contains(s, "Uid: 65534 65534 65534 65534 Gid: 65534 65534 65534 65534 Groups: CapEff: 0000000000000000") {
		t.Fatalf("identidad del hijo: %s", s)
	}
}

func TestResolverUsuarioConstructor(t *testing.T) {
	if u, aviso := resolverUsuarioConstructor("", 0, false); u != nil || aviso != "" {
		t.Fatal("sin nombre: ni usuario ni aviso")
	}
	if os.Geteuid() != 0 {
		// Sin root no se baja de usuario: el comportamiento de siempre.
		if u, _ := resolverUsuarioConstructor("kindling-build", 0, false); u != nil {
			t.Fatal("un daemon sin root no puede cambiar de usuario")
		}
		return
	}
	viejo := lookupUser
	t.Cleanup(func() { lookupUser = viejo })
	lookupUser = func(string) (*user.User, error) { return &user.User{Uid: "990", Gid: "989"}, nil }
	if u, aviso := resolverUsuarioConstructor("kindling-build", 991, true); u == nil || u.UID != 990 || aviso != "" {
		t.Fatalf("%+v %s", u, aviso)
	}
	if u, aviso := resolverUsuarioConstructor("kindling", 990, true); u != nil || !strings.Contains(aviso, "Firecracker") {
		t.Fatalf("el mismo usuario que el VMM: %+v %s", u, aviso)
	}
	// Un usuario de persona (o nobody): cada construcción mataría sus procesos.
	for _, uid := range []string{"1000", "65534"} {
		lookupUser = func(string) (*user.User, error) { return &user.User{Uid: uid, Gid: "1000"}, nil }
		if u, aviso := resolverUsuarioConstructor("juan", 991, true); u != nil || !strings.Contains(aviso, "system user") {
			t.Fatalf("uid %s aceptado: %+v %s", uid, u, aviso)
		}
	}
}

// El constructor tiene que poder llegar a la raíz de datos.
func TestAtravesable(t *testing.T) {
	base := t.TempDir()
	abierto := filepath.Join(base, "srv", "kt")
	cerrado := filepath.Join(base, "root", "kt")
	for _, d := range []string{abierto, cerrado} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(base, "root"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(base, "root"), 0o755) })
	_ = os.Chmod(base, 0o755)
	otro := &usuarioConstructor{Nombre: "kb", UID: 4242, GID: 4242}
	if !atravesable(abierto, otro) {
		t.Skip("algún directorio por encima del temporal no deja pasar a otros")
	}
	if atravesable(cerrado, otro) {
		t.Fatal("un directorio 0700 de otro dueño se dio por atravesable")
	}
}

// El cerrojo de host pone en fila las construcciones de dos daemons.
func TestBloquearConstructorHost(t *testing.T) {
	viejo := dirCerrojoConstructor
	dirCerrojoConstructor = t.TempDir()
	t.Cleanup(func() { dirCerrojoConstructor = viejo })
	uid := uint32(4242)
	soltar, err := bloquearConstructorHost(uid)
	if err != nil {
		t.Fatal(err)
	}
	tomado := make(chan struct{})
	go func() {
		s2, err := bloquearConstructorHost(uid)
		if err == nil {
			s2()
		}
		close(tomado)
	}()
	select {
	case <-tomado:
		t.Fatal("dos construcciones a la vez con el mismo usuario")
	case <-time.After(200 * time.Millisecond):
	}
	soltar()
	select {
	case <-tomado:
	case <-time.After(5 * time.Second):
		t.Fatal("el cerrojo no se soltó")
	}
}

func TestProcesoDeUID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status")
	os.WriteFile(p, []byte("Name:\tkling\nUid:\t990\t990\t990\t990\nGid:\t989\n"), 0o644)
	if !procesoDeUID(p, 990) || procesoDeUID(p, 0) {
		t.Fatal("Uid de /proc/<pid>/status")
	}
	os.WriteFile(p, []byte("Uid:\t0\t0\t990\t0\n"), 0o644)
	if !procesoDeUID(p, 990) {
		t.Fatal("un uid guardado también cuenta")
	}
}

// Un constructor aislado (aquí uno instalado que hace de "oci") deja la imagen
// en KLING_OUT_DIR y el daemon la mueve; uno que deja un enlace no cuela.
func TestConstructorAisladoSalida(t *testing.T) {
	s, h := testServer(t)
	bdir := t.TempDir()
	t.Setenv("KLING_BUILDERS_DIR", bdir)
	t.Setenv("KLING_BUILDERS_INSECURE", "1")
	os.MkdirAll(filepath.Join(s.root, "images"), 0o755)
	instalarConstructor(t, bdir, "oci", `
set -e
mkdir -p "$KLING_OUT_DIR"
echo raiz > "$KLING_OUT_DIR/$KLING_IMAGE_NAME.ext4"
`)
	if rr := call(t, h, "POST", "/images", `{"name":"pg","builder":"oci","spec":{}}`); rr.Code != 200 {
		t.Fatalf("oci: %d %s", rr.Code, rr.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(s.root, "images", "pg.ext4")); string(b) != "raiz\n" {
		t.Fatalf("la imagen no llegó a images/: %q", b)
	}

	instalarConstructor(t, bdir, "oci", `
mkdir -p "$KLING_OUT_DIR"
ln -s /etc/passwd "$KLING_OUT_DIR/$KLING_IMAGE_NAME.ext4"
`)
	if rr := call(t, h, "POST", "/images", `{"name":"mal","builder":"oci","spec":{}}`); rr.Code != 500 {
		t.Fatalf("un enlace como salida: %d %s", rr.Code, rr.Body)
	}
	if _, err := os.Lstat(filepath.Join(s.root, "images", "mal.ext4")); err == nil {
		t.Fatal("el enlace llegó a images/")
	}
}

// Reconstruir una imagen que ya existe (import -replace) pasa por la misma
// comprobación que PUT /images/{name}/blob: si un dorado la usa, 409 y la
// imagen no cambia.
func TestConstructorAisladoNoPisaUnaImagenEnUso(t *testing.T) {
	s, h := testServer(t)
	bdir := t.TempDir()
	t.Setenv("KLING_BUILDERS_DIR", bdir)
	t.Setenv("KLING_BUILDERS_INSECURE", "1")
	os.MkdirAll(filepath.Join(s.root, "images"), 0o755)
	instalarConstructor(t, bdir, "oci", `
set -e
mkdir -p "$KLING_OUT_DIR"
echo "$CONTENIDO" > "$KLING_OUT_DIR/$KLING_IMAGE_NAME.ext4"
`)
	t.Setenv("CONTENIDO", "v1")
	if rr := call(t, h, "POST", "/images", `{"name":"pg","builder":"oci","spec":{"ref":"a"}}`); rr.Code != 200 {
		t.Fatalf("primera construcción: %d %s", rr.Code, rr.Body)
	}
	dir := filepath.Join(s.root, "snapshots", "svc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"name":"svc","image":"pg"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTENIDO", "v2")
	rr := call(t, h, "POST", "/images", `{"name":"pg","builder":"oci","spec":{"ref":"b"}}`)
	if rr.Code != 409 || !strings.Contains(rr.Body.String(), "svc") {
		t.Fatalf("reconstruir una imagen que usa un dorado = %d %s; quería 409", rr.Code, rr.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(s.root, "images", "pg.ext4")); string(b) != "v1\n" {
		t.Fatalf("la imagen en uso cambió: %q", b)
	}
}

// Como root, las credenciales del registro se ceden al usuario de construcción
// (0600, suyas): solo él las lee.
func TestPrepararTrabajoCedeCredenciales(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ceder un fichero a otro uid necesita root")
	}
	u := &usuarioConstructor{Nombre: "x", UID: 54321, GID: 54321}
	work, err := prepararTrabajo(t.TempDir(), "pg", []byte(`{}`), []byte(`{"ghcr.io":{}}`), u)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(work, FicheroCredencialesConstructor))
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != u.UID || st.Gid != u.GID || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: uid %d gid %d modo %o", FicheroCredencialesConstructor, st.Uid, st.Gid, fi.Mode().Perm())
	}
}
