package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// counting escribe en d una extensión kling-cnt que apunta cada vez que se le
// pide el manifiesto y contesta con la versión v. Devuelve su ruta.
func counting(t *testing.T, d, v string) string {
	t.Helper()
	p := filepath.Join(d, "kling-cnt")
	body := "#!/bin/sh\necho x >> \"$0.count\"\n" +
		`echo '{"manifest_version":2,"name":"cnt","version":"` + v + `","commands":[{"name":"go"}]}'` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// asked es cuántas veces se ejecutó bin con --kling-manifest.
func asked(t *testing.T, bin string) int {
	t.Helper()
	b, err := os.ReadFile(bin + ".count")
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x")
}

// cachePath es una caché en un directorio que aún no existe: lo crea privado
// quien la escriba (el de t.TempDir no lo es).
func cachePath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "c", "manifests.json")
}

// noRacy desactiva la ventana de cambios recientes: los binarios de los tests
// se acaban de escribir.
func noRacy(t *testing.T) {
	t.Helper()
	old := racyWindow
	racyWindow = 0
	t.Cleanup(func() { racyWindow = old })
}

func discoverCached(t *testing.T, path []string, cache, version string) *Registry {
	t.Helper()
	return Discover(context.Background(), Options{
		Version: version, Path: path, ManifestCache: cache,
	})
}

func cntVersion(t *testing.T, r *Registry) string {
	t.Helper()
	p := find(r, "cnt")
	if p == nil || p.Err != nil {
		t.Fatalf("kling-cnt: %+v", p)
	}
	return p.Manifest.Version
}

func TestCacheAhorraElManifiesto(t *testing.T) {
	noRacy(t)
	d, cache := t.TempDir(), cachePath(t)
	bin := counting(t, d, "1")

	for i := 0; i < 3; i++ {
		if v := cntVersion(t, discoverCached(t, []string{d}, cache, "0.5.0")); v != "1" {
			t.Fatalf("versión %q", v)
		}
	}
	if n := asked(t, bin); n != 1 {
		t.Fatalf("se pidió el manifiesto %d veces, se esperaba 1", n)
	}

	fi, err := os.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("caché con permisos %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(cache))
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("directorio de caché con permisos %v", di.Mode().Perm())
	}
}

func TestCacheSinFicheroEsSinCache(t *testing.T) {
	d := t.TempDir()
	bin := counting(t, d, "1")
	for i := 0; i < 2; i++ {
		discoverCached(t, []string{d}, "", "0.5.0")
	}
	if n := asked(t, bin); n != 2 {
		t.Fatalf("sin caché se pidió %d veces, se esperaban 2", n)
	}
}

func TestCacheInvalidaSiCambiaElBinario(t *testing.T) {
	noRacy(t)
	d, cache := t.TempDir(), cachePath(t)
	bin := counting(t, d, "1")
	discoverCached(t, []string{d}, cache, "0.5.0")

	// Sobrescrito en su sitio con el mismo tamaño: mismo inodo, otro ctime.
	counting(t, d, "2")
	if v := cntVersion(t, discoverCached(t, []string{d}, cache, "0.5.0")); v != "2" {
		t.Fatalf("tras sobrescribir en su sitio salió la versión %q", v)
	}

	// Reemplazado con rename, como hace `kling plugins install`.
	nd := t.TempDir()
	counting(t, nd, "3")
	if err := os.Rename(filepath.Join(nd, "kling-cnt"), bin); err != nil {
		t.Fatal(err)
	}
	if v := cntVersion(t, discoverCached(t, []string{d}, cache, "0.5.0")); v != "3" {
		t.Fatalf("tras reemplazar salió la versión %q", v)
	}

	// Un chmod también cambia la identidad.
	os.Chmod(bin, 0o700)
	discoverCached(t, []string{d}, cache, "0.5.0")
	// Una vez por cada binario distinto: v1, v2, v3 y el chmod.
	if n := asked(t, bin); n != 4 {
		t.Fatalf("se pidió el manifiesto %d veces, se esperaban 4", n)
	}
}

func TestCacheInvalidaSiCambiaKling(t *testing.T) {
	noRacy(t)
	d, cache := t.TempDir(), cachePath(t)
	bin := counting(t, d, "1")
	discoverCached(t, []string{d}, cache, "0.5.0")
	discoverCached(t, []string{d}, cache, "0.6.0")
	discoverCached(t, []string{d}, cache, "0.6.0")
	if n := asked(t, bin); n != 2 {
		t.Fatalf("se pidió el manifiesto %d veces, se esperaban 2", n)
	}
}

func TestCacheNoGuardaBinariosRecienCambiados(t *testing.T) {
	// Con la ventana de verdad, un binario de hace un instante no se guarda:
	// otra escritura en el mismo tic no cambiaría su ctime.
	d, cache := t.TempDir(), cachePath(t)
	bin := counting(t, d, "1")
	discoverCached(t, []string{d}, cache, "0.5.0")
	discoverCached(t, []string{d}, cache, "0.5.0")
	if n := asked(t, bin); n != 2 {
		t.Fatalf("se pidió el manifiesto %d veces, se esperaban 2", n)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("se escribió la caché de un binario recién cambiado: %v", err)
	}
}

func TestCacheNoGuardaManifiestosRotos(t *testing.T) {
	noRacy(t)
	cache := cachePath(t)
	for i := 0; i < 2; i++ {
		r := discoverCached(t, []string{dir}, cache, "0.5.0")
		if p := find(r, "broken"); p == nil || p.Err == nil {
			t.Fatalf("kling-broken: %+v", p)
		}
		if p := find(r, "old"); p == nil || p.Err == nil {
			t.Fatalf("kling-old debería seguir pidiendo un kling más nuevo: %+v", p)
		}
		if p := find(r, "liar"); p == nil || p.Err == nil {
			t.Fatalf("kling-liar debería seguir rechazado: %+v", p)
		}
	}
	f := readCache(t, cache)
	if _, ok := f.Entries[filepath.Join(dir, "kling-broken")]; ok {
		t.Fatal("se guardó el manifiesto de kling-broken")
	}
}

func readCache(t *testing.T, p string) cacheFile {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var f cacheFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func writeCache(t *testing.T, p string, f cacheFile, perm os.FileMode) {
	t.Helper()
	b, _ := json.Marshal(f)
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, b, perm); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, perm)
}

// La caché nunca se salta la validación: lo que viene de ella pasa por lo mismo
// que lo recién ejecutado.
func TestCacheEnvenenadaNoSeSaltaLaValidacion(t *testing.T) {
	noRacy(t)
	d, cache := t.TempDir(), cachePath(t)
	bin := counting(t, d, "1")
	id, ok := statID(bin)
	if !ok {
		t.Fatal("sin identidad")
	}
	entry := func(m string) cacheFile {
		return cacheFile{Format: cacheFormat, Entries: map[string]*cacheEntry{
			bin: {ID: id, Kling: "0.5.0", API: APIVersion, Manifest: json.RawMessage(m)},
		}}
	}

	// Un manifiesto que no valida: se vuelve a preguntar al binario.
	writeCache(t, cache, entry(`{"manifest_version":2,"name":"cnt","version":"1","commands":[{"name":"NO VALE"}]}`), 0o600)
	if v := cntVersion(t, discoverCached(t, []string{d}, cache, "0.5.0")); v != "1" || asked(t, bin) != 1 {
		t.Fatalf("una entrada inválida no se volvió a pedir: versión %q, %d veces", v, asked(t, bin))
	}

	// Uno válido pero de otra extensión: el nombre se sigue comprobando.
	writeCache(t, cache, entry(`{"manifest_version":2,"name":"otra","version":"9","commands":[{"name":"go"}]}`), 0o600)
	p := find(discoverCached(t, []string{d}, cache, "0.5.0"), "cnt")
	if p == nil || p.Err == nil || !strings.Contains(p.Err.Error(), "otra") {
		t.Fatalf("un manifiesto con otro nombre se aceptó: %+v", p)
	}

	// Uno que pide un kling más nuevo: MinKling se sigue comprobando.
	writeCache(t, cache, entry(`{"manifest_version":2,"name":"cnt","version":"9","min_kling":"99.0.0","commands":[{"name":"go"}]}`), 0o600)
	p = find(discoverCached(t, []string{d}, cache, "0.5.0"), "cnt")
	if p == nil || p.Err == nil {
		t.Fatalf("min_kling de la caché no se comprobó: %+v", p)
	}
}

func TestCacheAjenaSeIgnora(t *testing.T) {
	noRacy(t)
	d := t.TempDir()
	bin := counting(t, d, "1")
	id, _ := statID(bin)
	fake := cacheFile{Format: cacheFormat, Entries: map[string]*cacheEntry{
		bin: {ID: id, Kling: "0.5.0", API: APIVersion,
			Manifest: json.RawMessage(`{"manifest_version":2,"name":"cnt","version":"FALSA","commands":[{"name":"go"}]}`)},
	}}

	// want es la versión que sale: "FALSA" solo si la caché es de fiar.
	cases := []struct {
		name string
		want string
		mk   func(t *testing.T) string
	}{
		{"privada (control)", "FALSA", func(t *testing.T) string {
			p := cachePath(t)
			writeCache(t, p, fake, 0o600)
			return p
		}},
		{"fichero legible por otros", "1", func(t *testing.T) string {
			p := cachePath(t)
			writeCache(t, p, fake, 0o644)
			return p
		}},
		{"directorio abierto a otros", "1", func(t *testing.T) string {
			p := cachePath(t)
			writeCache(t, p, fake, 0o600)
			os.Chmod(filepath.Dir(p), 0o755)
			return p
		}},
		{"enlace simbólico", "1", func(t *testing.T) string {
			p := cachePath(t)
			real := filepath.Join(filepath.Dir(p), "real.json")
			writeCache(t, real, fake, 0o600)
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.mk(t)
			if v := cntVersion(t, discoverCached(t, []string{d}, p, "0.5.0")); v != tc.want {
				t.Fatalf("caché %s: versión %q, se esperaba %q", tc.name, v, tc.want)
			}
		})
	}
}

func TestCachePodaLosQueYaNoEstan(t *testing.T) {
	noRacy(t)
	d, cache := t.TempDir(), cachePath(t)
	bin := counting(t, d, "1")
	discoverCached(t, []string{d}, cache, "0.5.0")
	if _, ok := readCache(t, cache).Entries[bin]; !ok {
		t.Fatal("no se guardó kling-cnt")
	}
	os.Remove(bin)
	d2 := t.TempDir()
	counting(t, d2, "2")
	discoverCached(t, []string{d2}, cache, "0.5.0")
	f := readCache(t, cache)
	if _, ok := f.Entries[bin]; ok {
		t.Fatal("la entrada de un binario borrado sigue en la caché")
	}
	if len(f.Entries) != 1 {
		t.Fatalf("entradas: %d", len(f.Entries))
	}
}

// Varios kling a la vez (git checkout con el gancho de db) comparten el
// fichero: nadie lee uno a medias ni sale con un manifiesto equivocado.
func TestCacheConcurrente(t *testing.T) {
	noRacy(t)
	d, cache := t.TempDir(), cachePath(t)
	counting(t, d, "1")
	hello, _ := os.ReadFile(filepath.Join(dir, "kling-hello"))
	os.WriteFile(filepath.Join(d, "kling-hello"), hello, 0o755)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				r := Discover(context.Background(), Options{Version: "0.5.0", Path: []string{d}, ManifestCache: cache})
				if p := find(r, "cnt"); p == nil || p.Err != nil || p.Manifest.Version != "1" {
					errs <- "kling-cnt mal"
				}
				if p := find(r, "hello"); p == nil || p.Err != nil || p.Manifest.Version != "1.2.3" {
					errs <- "kling-hello mal"
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	readCache(t, cache) // JSON entero
}

func TestCacheIdentidadReciente(t *testing.T) {
	now := time.Now()
	old := fileID{Mtime: now.Add(-time.Hour).UnixNano(), Ctime: now.Add(-time.Hour).UnixNano()}
	if old.recent(now) {
		t.Fatal("un binario de hace una hora cuenta como reciente")
	}
	fresh := old
	fresh.Ctime = now.Add(-time.Second).UnixNano()
	if !fresh.recent(now) {
		t.Fatal("un ctime de hace un segundo no cuenta como reciente")
	}
}

// BenchmarkDiscover mide el descubrimiento con kling-hello (un binario Go) con
// y sin caché: es lo que paga cada `kling <comando de extensión>`.
func BenchmarkDiscover(b *testing.B) {
	d := b.TempDir()
	src, _ := os.ReadFile(filepath.Join(dir, "kling-hello"))
	os.WriteFile(filepath.Join(d, "kling-hello"), src, 0o755)
	old := racyWindow
	racyWindow = 0
	defer func() { racyWindow = old }()
	for _, tc := range []struct{ name, cache string }{
		{"sin-cache", ""},
		{"con-cache", filepath.Join(b.TempDir(), "c", "manifests.json")},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				r := Discover(context.Background(), Options{Version: "0.5.0", Path: []string{d}, ManifestCache: tc.cache})
				if p := find(r, "hello"); p == nil || p.Err != nil {
					b.Fatal(p)
				}
			}
		})
	}
}
