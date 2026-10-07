package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func digestDe(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// cachesDePrueba deja una raíz con la caché del constructor (de quien corre
// el test) y la verificada, y devuelve las dos y el directorio de trabajo.
func cachesDePrueba(t *testing.T) (cache, verificada, work string) {
	t.Helper()
	root := t.TempDir()
	cache, err := prepararCache(root, yo())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cache, "oci", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	if verificada, err = prepararVerificada(root); err != nil {
		t.Fatal(err)
	}
	return cache, verificada, t.TempDir()
}

func blobDelConstructor(t *testing.T, cache, digest string, cuerpo []byte) string {
	t.Helper()
	p := filepath.Join(cache, "oci", "sha256", strings.TrimPrefix(digest, "sha256:"))
	if err := os.WriteFile(p, cuerpo, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func verificadoEn(verificada, digest string) string {
	return filepath.Join(verificada, "oci", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

// Un blob que cuadra pasa a la verificada (copia nueva, 0644) y sale de la
// caché del constructor. El ATAQUE: el constructor cambia un blob de su caché
// (contenido de otro con el nombre del bueno) y ese no entra: se borra.
func TestPromoverCache(t *testing.T) {
	cache, verificada, work := cachesDePrueba(t)
	bueno := []byte("capa buena")
	dBueno := digestDe(bueno)
	srcBueno := blobDelConstructor(t, cache, dBueno, bueno)
	dEnvenenado := digestDe([]byte("capa legítima"))
	srcEnv := blobDelConstructor(t, cache, dEnvenenado, []byte("capa ENVENENADA"))
	dFalta := digestDe([]byte("no está"))
	os.WriteFile(filepath.Join(work, ficheroUsados),
		[]byte(dBueno+"\n"+dEnvenenado+"\n"+dFalta+"\nbasura\n../../etc/passwd\n"), 0o600)

	usados, n, err := promoverCache(work, cache, verificada, uint32(os.Getuid()), 1<<30)
	if n != 1 || err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("promovidos %d, err %v", n, err)
	}
	if len(usados) != 3 {
		t.Fatalf("usados %v", usados)
	}
	b, err := os.ReadFile(verificadoEn(verificada, dBueno))
	if err != nil || string(b) != string(bueno) {
		t.Fatalf("el bueno en la verificada: %q %v", b, err)
	}
	fi, _ := os.Lstat(verificadoEn(verificada, dBueno))
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("modo %o", fi.Mode().Perm())
	}
	if _, err := os.Lstat(srcBueno); !os.IsNotExist(err) {
		t.Fatal("el verificado sigue en la caché del constructor")
	}
	if _, err := os.Lstat(verificadoEn(verificada, dEnvenenado)); !os.IsNotExist(err) {
		t.Fatal("un blob cambiado entró en la caché verificada")
	}
	if _, err := os.Lstat(srcEnv); !os.IsNotExist(err) {
		t.Fatal("el blob cambiado sigue en la caché del constructor")
	}
	if es, _ := os.ReadDir(filepath.Join(verificada, "oci", "sha256")); len(es) != 1 {
		t.Fatalf("en la verificada: %v", es)
	}

	// Otra construcción que usa el mismo: se queda, con la fecha de hoy, y
	// la copia que hubiera vuelto a la caché del constructor sobra.
	viejo := time.Now().Add(-48 * time.Hour)
	os.Chtimes(verificadoEn(verificada, dBueno), viejo, viejo)
	otra := blobDelConstructor(t, cache, dBueno, bueno)
	os.WriteFile(filepath.Join(work, ficheroUsados), []byte(dBueno+"\n"), 0o600)
	if _, n, err := promoverCache(work, cache, verificada, uint32(os.Getuid()), 1<<30); n != 0 || err != nil {
		t.Fatalf("promovidos %d, err %v", n, err)
	}
	if fi, _ := os.Lstat(verificadoEn(verificada, dBueno)); time.Since(fi.ModTime()) > time.Hour {
		t.Fatal("usar un verificado no le pone la fecha")
	}
	if _, err := os.Lstat(otra); !os.IsNotExist(err) {
		t.Fatal("la copia del constructor de un verificado sigue ahí")
	}
}

// Lo que no es un fichero regular del constructor no entra, ni una lista
// que es un enlace se sigue.
func TestPromoverCacheRechaza(t *testing.T) {
	cache, verificada, work := cachesDePrueba(t)
	secreto := filepath.Join(t.TempDir(), "secreto")
	os.WriteFile(secreto, []byte("privado"), 0o600)
	d := digestDe([]byte("privado"))
	os.Symlink(secreto, filepath.Join(cache, "oci", "sha256", strings.TrimPrefix(d, "sha256:")))
	os.WriteFile(filepath.Join(work, ficheroUsados), []byte(d+"\n"), 0o600)
	if _, n, err := promoverCache(work, cache, verificada, uint32(os.Getuid()), 1<<30); n != 0 || err == nil {
		t.Fatalf("un enlace promovido: %d %v", n, err)
	}
	if _, err := os.Lstat(verificadoEn(verificada, d)); !os.IsNotExist(err) {
		t.Fatal("un enlace entró en la verificada")
	}

	// Una FIFO con el nombre del blob vacío: no es un fichero regular.
	vacio := digestDe(nil)
	if err := syscall.Mkfifo(filepath.Join(cache, "oci", "sha256", strings.TrimPrefix(vacio, "sha256:")), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, ficheroUsados), []byte(vacio+"\n"), 0o600)
	if _, n, err := promoverCache(work, cache, verificada, uint32(os.Getuid()), 1<<30); n != 0 || err == nil {
		t.Fatalf("una FIFO promovida: %d %v", n, err)
	}

	// De otro dueño (ni el constructor ni root): fuera. Como root, los
	// ficheros de prueba son de root, que vale: lo cubre
	// TestVerificadaIntocableParaElConstructor.
	c2, v2, w2 := cachesDePrueba(t)
	cuerpo := []byte("ajeno")
	d2 := digestDe(cuerpo)
	blobDelConstructor(t, c2, d2, cuerpo)
	os.WriteFile(filepath.Join(w2, ficheroUsados), []byte(d2+"\n"), 0o600)
	if os.Geteuid() != 0 {
		if _, n, _ := promoverCache(w2, c2, v2, uint32(os.Getuid())+1, 1<<30); n != 0 {
			t.Fatalf("de otro dueño promovido: %d", n)
		}
		if _, err := os.Lstat(verificadoEn(v2, d2)); !os.IsNotExist(err) {
			t.Fatal("de otro dueño entró en la verificada")
		}
	}

	// La lista como enlace no se sigue.
	c3, v3, w3 := cachesDePrueba(t)
	lista := filepath.Join(t.TempDir(), "lista")
	os.WriteFile(lista, []byte(d2+"\n"), 0o600)
	os.Symlink(lista, filepath.Join(w3, ficheroUsados))
	if _, _, err := promoverCache(w3, c3, v3, uint32(os.Getuid()), 1<<30); err == nil {
		t.Fatal("una lista que es un enlace se siguió")
	}

	// La caché del constructor con un enlace en medio (oci -> otra parte):
	// nada se copia ni se borra por ahí.
	c4, v4, w4 := cachesDePrueba(t)
	fuera := t.TempDir()
	os.MkdirAll(filepath.Join(fuera, "sha256"), 0o755)
	os.WriteFile(filepath.Join(fuera, "sha256", strings.TrimPrefix(d2, "sha256:")), cuerpo, 0o644)
	os.RemoveAll(filepath.Join(c4, "oci"))
	os.Symlink(fuera, filepath.Join(c4, "oci"))
	os.WriteFile(filepath.Join(w4, ficheroUsados), []byte(d2+"\n"), 0o600)
	if _, n, _ := promoverCache(w4, c4, v4, uint32(os.Getuid()), 1<<30); n != 0 {
		t.Fatal("se promovió a través de un enlace")
	}
	if _, err := os.Stat(filepath.Join(fuera, "sha256", strings.TrimPrefix(d2, "sha256:"))); err != nil {
		t.Fatal("se borró a través de un enlace")
	}
}

func TestPrepararVerificada(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "cache"), 0o755)
	v, err := prepararVerificada(root)
	if err != nil || v != filepath.Join(root, "cache", "verified") {
		t.Fatal(v, err)
	}
	for _, d := range []string{v, filepath.Join(v, "oci"), filepath.Join(v, "oci", "sha256")} {
		if fi, err := os.Lstat(d); err != nil || fi.Mode().Perm() != 0o755 || !fi.IsDir() {
			t.Fatalf("%s: %v %v", d, fi, err)
		}
	}
	root2 := t.TempDir()
	os.MkdirAll(filepath.Join(root2, "cache"), 0o755)
	os.Symlink(t.TempDir(), filepath.Join(root2, "cache", "verified"))
	if _, err := prepararVerificada(root2); err == nil {
		t.Fatal("cache/verified como enlace aceptado")
	}
}

// El barrido: fuera lo a medias, lo viejo y, pasado el tope, lo más antiguo,
// primero lo no verificado; lo que acaba de usar una construcción se queda.
func TestBarrerCachesConstruccion(t *testing.T) {
	cache, verificada, _ := cachesDePrueba(t)
	ahora := time.Now()
	poner := func(dir, nombre string, tam int, edad time.Duration) string {
		p := filepath.Join(dir, "oci", "sha256", nombre)
		os.WriteFile(p, make([]byte, tam), 0o644)
		os.Chtimes(p, ahora.Add(-edad), ahora.Add(-edad))
		return p
	}
	hex := func(c byte) string { return strings.Repeat(string(c), 64) }
	parte := poner(cache, hex('a')+".part", 10, 0)
	tmp := poner(verificada, ".tmp-123", 10, 0)
	viejoV := poner(verificada, hex('b'), 10, 40*24*time.Hour)
	viejoUsado := poner(verificada, hex('c'), 10, 40*24*time.Hour)
	nuevoC := poner(cache, hex('d'), 1<<20, time.Hour)
	nuevoV1 := poner(verificada, hex('e'), 1<<20, 3*time.Hour)
	nuevoV2 := poner(verificada, hex('f'), 1<<20, 2*time.Hour)
	// El del constructor se pone una fecha futura para no ser el primero:
	// sale antes que cualquier verificado igual.
	futuro := poner(cache, hex('9'), 1<<20, -24*time.Hour)

	// Tope de 0 = 20 GiB: solo lo a medias y lo viejo.
	const mes = 30 * 24 * time.Hour
	b, _ := barrerCachesConstruccion(cache, verificada, uint32(os.Getuid()), 20<<30, mes,
		[]string{"sha256:" + hex('c')}, ahora)
	for _, p := range []string{parte, tmp, viejoV} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s sigue", filepath.Base(p))
		}
	}
	if b != 3 {
		t.Fatalf("borrados %d", b)
	}
	for _, p := range []string{viejoUsado, nuevoC, nuevoV1, nuevoV2, futuro} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s no debería borrarse", filepath.Base(p))
		}
	}

	// Con tope: hay 4 MiB y algo; bajo el tope no se toca nada más.
	if b, _ := barrerCachesConstruccion(cache, verificada, uint32(os.Getuid()), 5<<20, mes,
		[]string{"sha256:" + hex('c')}, ahora); b != 0 {
		t.Fatalf("bajo el tope se borraron %d", b)
	}
	quedan := barrerHasta(t, cache, verificada, 2<<20+10, hex('c'), ahora)
	// Fuera los dos del constructor primero (aunque uno diga ser del futuro):
	// quedan los dos verificados nuevos y el usado.
	if strings.Join(quedan, ",") != strings.Join([]string{hex('c'), hex('e'), hex('f')}, ",") {
		t.Fatalf("quedan %v", quedan)
	}
	quedan = barrerHasta(t, cache, verificada, 1<<20+10, hex('c'), ahora)
	if strings.Join(quedan, ",") != strings.Join([]string{hex('c'), hex('f')}, ",") {
		t.Fatalf("quedan %v (se va el verificado más viejo)", quedan)
	}
}

// barrerHasta barre con un tope en bytes (lo que en producción son GiB) y
// devuelve los nombres que quedan en las dos cachés, ordenados.
func barrerHasta(t *testing.T, cache, verificada string, tope int64, conservar string, ahora time.Time) []string {
	t.Helper()
	barrerCachesConstruccion(cache, verificada, uint32(os.Getuid()), tope, 30*24*time.Hour, []string{"sha256:" + conservar}, ahora)
	var out []string
	for _, d := range []string{filepath.Join(cache, "oci", "sha256"), filepath.Join(verificada, "oci", "sha256")} {
		es, _ := os.ReadDir(d)
		for _, e := range es {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func TestLimitesCacheEfectivos(t *testing.T) {
	b, e := LimitesCacheConstruccion{}.efectivos()
	if b != 20<<30 || e != 30*24*time.Hour {
		t.Fatal(b, e)
	}
	b, e = LimitesCacheConstruccion{MaxGiB: 2, MaxDays: 1}.efectivos()
	if b != 2<<30 || e != 24*time.Hour {
		t.Fatal(b, e)
	}
	// Un tope enorme quiere decir guardar más: sin el recorte, días*24h y
	// GiB<<30 desbordan a negativo o 0 y el barrido lo borraría todo.
	for _, l := range []LimitesCacheConstruccion{{MaxGiB: 1 << 40, MaxDays: 1 << 20}, {MaxGiB: 1 << 34, MaxDays: 200000}} {
		b, e = l.efectivos()
		if b < 20<<30 || e < 30*24*time.Hour {
			t.Fatalf("%+v: %d bytes, %s", l, b, e)
		}
	}
}

// La lista de lo usado la escribe el constructor: nombrando blobs
// verificados no mantiene la caché por encima del tope. Lo usado se guarda
// solo mientras quepa.
func TestBarrerUsadosNoPasanDelTope(t *testing.T) {
	cache, verificada, _ := cachesDePrueba(t)
	ahora := time.Now()
	var usados []string
	for i, c := range []byte("abcd") {
		p := filepath.Join(verificada, "oci", "sha256", strings.Repeat(string(c), 64))
		os.WriteFile(p, make([]byte, 1<<20), 0o644)
		os.Chtimes(p, ahora.Add(-time.Duration(4-i)*time.Hour), ahora.Add(-time.Duration(4-i)*time.Hour))
		usados = append(usados, "sha256:"+strings.Repeat(string(c), 64))
	}
	barrerCachesConstruccion(cache, verificada, uint32(os.Getuid()), 2<<20, 30*24*time.Hour, usados, ahora)
	var total int64
	es, _ := os.ReadDir(filepath.Join(verificada, "oci", "sha256"))
	for _, e := range es {
		fi, _ := e.Info()
		total += fi.Size()
	}
	if total > 2<<20 || len(es) != 2 {
		t.Fatalf("quedan %d ficheros, %d bytes con un tope de 2 MiB", len(es), total)
	}
}

// Como root en Linux: el usuario de construcción (aquí nobody) no puede
// escribir, renombrar, borrar ni crear nada en la caché verificada.
func TestVerificadaIntocableParaElConstructor(t *testing.T) {
	if os.Geteuid() != 0 || runtime.GOOS != "linux" {
		t.Skip("solo como root en Linux")
	}
	root := t.TempDir()
	os.Chmod(filepath.Dir(root), 0o755) // nobody tiene que poder llegar
	os.Chmod(root, 0o755)
	u := &usuarioConstructor{Nombre: "nobody", UID: 65534, GID: 65534}
	cache, err := prepararCache(root, u)
	if err != nil {
		t.Fatal(err)
	}
	verificada, err := prepararVerificada(root)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(cache, "oci", "sha256"), 0o755)
	for _, d := range []string{filepath.Join(cache, "oci"), filepath.Join(cache, "oci", "sha256")} {
		os.Lchown(d, 65534, 65534)
	}
	cuerpo := []byte("capa")
	dg := digestDe(cuerpo)
	src := blobDelConstructor(t, cache, dg, cuerpo)
	os.Lchown(src, 65534, 65534)
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, ficheroUsados), []byte(dg+"\n"), 0o600)
	if _, n, err := promoverCache(work, cache, verificada, 65534, 1<<30); n != 1 || err != nil {
		t.Fatalf("promovidos %d: %v", n, err)
	}
	dst := verificadoEn(verificada, dg)
	dir := filepath.Dir(dst)
	// Leerla sí puede: lo que falla abajo no es que no llegue.
	leer := exec.Command("/bin/cat", dst)
	leer.SysProcAttr = u.credencial()
	if out, err := leer.CombinedOutput(); err != nil || string(out) != "capa" {
		t.Fatalf("el constructor no puede leer la verificada: %q %v", out, err)
	}
	for _, script := range []string{
		"echo x > " + dst,
		"echo x >> " + dst,
		"mv " + dst + " " + dst + ".x",
		"rm -f " + dst,
		"touch " + dir + "/nuevo",
		"ln -s /etc/passwd " + dir + "/enlace",
		"chmod 666 " + dst,
		"mv " + filepath.Join(verificada, "oci") + " " + filepath.Join(verificada, "x"),
	} {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.SysProcAttr = u.credencial()
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("el constructor pudo: %s (%s)", script, out)
		}
	}
	if b, _ := os.ReadFile(dst); string(b) != "capa" {
		t.Fatalf("la verificada cambió: %q", b)
	}

	// En la caché del constructor, uno de otro usuario y uno de root que él
	// no puede leer (un enlace duro a algo privado): ninguno entra.
	for _, c := range []struct {
		uid  int
		modo os.FileMode
	}{{12345, 0o644}, {0, 0o600}} {
		cuerpo := []byte("blob de " + strings.Repeat("x", c.uid%7) + c.modo.String())
		dg := digestDe(cuerpo)
		p := blobDelConstructor(t, cache, dg, cuerpo)
		os.Lchown(p, c.uid, c.uid)
		os.Chmod(p, c.modo)
		os.WriteFile(filepath.Join(work, ficheroUsados), []byte(dg+"\n"), 0o600)
		if _, n, err := promoverCache(work, cache, verificada, 65534, 1<<30); n != 0 || err == nil {
			t.Errorf("uid %d modo %v promovido: %d %v", c.uid, c.modo, n, err)
		}
	}
}

// El constructor no llena el disco del host a través de root: un fichero
// disperso (TiB de ceros sin gastar disco, con el sha256 de esos ceros), uno
// mayor que el tope y lo que pasa del tope en una pasada no se copian.
func TestPromoverCacheTope(t *testing.T) {
	cache, verificada, work := cachesDePrueba(t)
	const ceros = 64 << 20
	dDisperso := digestDe(make([]byte, ceros))
	disperso := filepath.Join(cache, "oci", "sha256", strings.TrimPrefix(dDisperso, "sha256:"))
	f, err := os.Create(disperso)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(ceros)
	f.Close()
	var st syscall.Stat_t
	if syscall.Stat(disperso, &st); st.Blocks*512 >= ceros {
		t.Skip("este sistema de ficheros no hace ficheros dispersos")
	}
	grande := make([]byte, 2<<20)
	for i := range grande {
		grande[i] = byte(i * 7)
	}
	dGrande := digestDe(grande)
	srcGrande := blobDelConstructor(t, cache, dGrande, grande)
	uno, otro := make([]byte, 600<<10), make([]byte, 600<<10)
	uno[0], otro[0] = 1, 2
	dUno, dOtro := digestDe(uno), digestDe(otro)
	blobDelConstructor(t, cache, dUno, uno)
	srcOtro := blobDelConstructor(t, cache, dOtro, otro)
	os.WriteFile(filepath.Join(work, ficheroUsados),
		[]byte(dDisperso+"\n"+dGrande+"\n"+dUno+"\n"+dOtro+"\n"), 0o600)

	_, n, err := promoverCache(work, cache, verificada, uint32(os.Getuid()), 1<<20)
	if n != 1 || err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("promovidos %d, err %v", n, err)
	}
	for _, d := range []string{dDisperso, dGrande, dOtro} {
		if _, err := os.Lstat(verificadoEn(verificada, d)); !os.IsNotExist(err) {
			t.Fatalf("%s entró en la verificada", d)
		}
	}
	if _, err := os.Lstat(verificadoEn(verificada, dUno)); err != nil {
		t.Fatal("el que cabía no entró")
	}
	if _, err := os.Lstat(disperso); !os.IsNotExist(err) {
		t.Fatal("el disperso sigue en la caché del constructor")
	}
	// Lo que no cabe se queda en la suya (sin verificar), no se tira.
	for _, p := range []string{srcGrande, srcOtro} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s se borró: %v", filepath.Base(p), err)
		}
	}
}

// El ATAQUE de la carrera: después de que el daemon mire su caché, el
// constructor (un proceso que sobrevivió al barrido) cambia cache/oci por un
// enlace a un directorio de fuera. Ni el barrido ni la promoción borran ni
// leen nada de ahí.
func TestCachePropiaNoSalePorEnlace(t *testing.T) {
	fueraDe := func(t *testing.T) (dir, victima string) {
		dir = t.TempDir()
		os.MkdirAll(filepath.Join(dir, "sha256"), 0o755)
		victima = filepath.Join(dir, "sha256", "passwd")
		os.WriteFile(victima, []byte("root:x:0:0"), 0o644)
		return dir, victima
	}
	cambiar := func(cache, fuera string) {
		os.Rename(filepath.Join(cache, "oci"), filepath.Join(cache, "oci.viejo"))
		os.Symlink(fuera, filepath.Join(cache, "oci"))
	}

	// Barrido, con el cambio justo después de abrirla.
	cache, verificada, _ := cachesDePrueba(t)
	fuera, victima := fueraDe(t)
	parte := blobDelConstructor(t, cache, "sha256:x.part", []byte("a medias"))
	trasAbrirPropia = func() { cambiar(cache, fuera) }
	t.Cleanup(func() { trasAbrirPropia = func() {} })
	barrerCachesConstruccion(cache, verificada, uint32(os.Getuid()), 20<<30, time.Hour, nil, time.Now())
	if _, err := os.Lstat(victima); err != nil {
		t.Fatal("el barrido borró fuera de la caché del constructor:", err)
	}
	if _, err := os.Lstat(filepath.Join(cache, "oci.viejo", "sha256", filepath.Base(parte))); !os.IsNotExist(err) {
		t.Fatal("el barrido no limpió la caché que abrió")
	}
	trasAbrirPropia = func() {}

	// Con el enlace ya puesto: ni se abre.
	cache, verificada, work := cachesDePrueba(t)
	fuera, victima = fueraDe(t)
	cuerpo := []byte("root:x:0:0")
	d := digestDe(cuerpo)
	os.WriteFile(filepath.Join(fuera, "sha256", strings.TrimPrefix(d, "sha256:")), cuerpo, 0o644)
	cambiar(cache, fuera)
	os.WriteFile(filepath.Join(work, ficheroUsados), []byte(d+"\n"), 0o600)
	if _, n, _ := promoverCache(work, cache, verificada, uint32(os.Getuid()), 1<<30); n != 0 {
		t.Fatal("se promovió a través de un enlace")
	}
	barrerCachesConstruccion(cache, verificada, uint32(os.Getuid()), 0, 0, nil, time.Now())
	if _, err := os.Lstat(victima); err != nil {
		t.Fatal("se borró fuera de la caché del constructor:", err)
	}
}

// Si el barrido de procesos no acabó limpio (alguno del constructor sigue
// vivo), su caché no se toca: ni se promueve ni se barre.
func TestCacheConstruccionSinBarridoLimpio(t *testing.T) {
	cache, verificada, work := cachesDePrueba(t)
	cuerpo := []byte("capa")
	d := digestDe(cuerpo)
	src := blobDelConstructor(t, cache, d, cuerpo)
	parte := blobDelConstructor(t, cache, "sha256:y.part", []byte("a medias"))
	os.WriteFile(filepath.Join(work, ficheroUsados), []byte(d+"\n"), 0o600)
	s := &Server{}
	u := &usuarioConstructor{UID: uint32(os.Getuid())}
	s.cacheConstruccion(work, cache, verificada, u, true, false)
	if _, err := os.Lstat(verificadoEn(verificada, d)); !os.IsNotExist(err) {
		t.Fatal("se promovió sin un barrido limpio")
	}
	for _, p := range []string{src, parte} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s se barrió sin un barrido limpio", filepath.Base(p))
		}
	}
	s.cacheConstruccion(work, cache, verificada, u, true, true)
	if _, err := os.Lstat(verificadoEn(verificada, d)); err != nil {
		t.Fatal("con el barrido limpio no se promovió")
	}
}
