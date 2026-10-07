package daemon

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/lazyre"
)

// CACHÉ VERIFICADA DEL CONSTRUCTOR.
//
// El constructor oci sin root tiene su caché de blobs (cache/builder/oci) y,
// como la puede escribir, el cliente OCI rehashea todo lo que saca de ella
// (oci.Client.SiempreRehash): 1-3 s por GiB en cada import. Para no pagarlo
// siempre, el daemon, como root, mantiene otra al lado:
//
//	<root>/cache/verified/oci/sha256/<hex>   root:<grupo del constructor>, directorios 0750, ficheros 0640
//
// Al acabar BIEN una construcción, con los procesos del constructor ya
// barridos y el cerrojo de host aún tomado (nadie suyo vivo que cambie nada a
// medias), el daemon lee la lista de blobs que usó (<trabajo>/cache-used, que
// la deja el constructor) y, por cada uno que aún no esté en la verificada,
// lo COPIA desde la caché del constructor a un fichero nuevo de root,
// hasheando lo que copia: entra solo si el sha256 de esos bytes es el de su
// nombre. Copiar y no mover ni enlazar es a propósito: el inodo nuevo es de
// root desde que nace, sin ACL, sin otros enlaces duros que el constructor
// guarde, sin descriptores ni mmap suyos abiertos; lo que se hashea es
// exactamente lo que queda. El original se borra de la caché del constructor.
// Su caché se toca solo a través de un os.Root (abrirPropia) y solo si el
// barrido de sus procesos acabó limpio; un fichero disperso no se copia, ni
// más bytes por pasada que el tope (el constructor no llena el disco del
// host a través de root).
//
// Lo que usó sin tenerlo en su caché, leído de la de blobs de root
// (<root>/cache/oci: lo que sube `kling image import -archive`, que el
// constructor lee como Seed), pasa con un enlace duro y sale de allí: esa
// caché solo la escribe root y cada blob llegó comprobado (handlePutOCIBlob,
// o una descarga como root). Así reconstruir una imagen de un archivo no
// rehashea sus capas cada vez.
//
// El constructor la lee (KLING_VERIFIED_CACHE_DIR) y no puede escribir en ella
// ni renombrar nada: los directorios no son suyos ni tienen escritura para
// grupo ni otros. El cliente OCI usa un blob de ahí sin rehashear solo si lo
// comprueba así (oci.verificado); si no, es como si no estuviera.
//
// PERMISOS. Las capas de una imagen privada (kling registry login) o de un
// archivo no son para las demás cuentas del host: como images/ (0750), las
// dos cachés de root, la verificada y la de blobs (<root>/cache/oci), son de
// root con el grupo del usuario de construcción, que las lee: directorios
// 0750 y blobs 0640. Sin usuario de construcción, del grupo de root, 0700 y
// 0600 (cerrarDirsCache).
//
// Las cachés crecían sin límite. Después de cada construcción se barren
// (barrerCachesConstruccion): fuera los .part y lo que no es un blob, lo que
// lleva más de daemon.build_cache_max_days sin usarse (en la verificada y en
// la de root, cada uso le pone la fecha) y, si entre todas pasan de
// daemon.build_cache_max_gib, lo más viejo: primero lo no verificado, luego
// lo de root y al final lo verificado. Lo que acaba de usar la construcción
// no se toca, pero solo hasta el tope: la lista la escribe el constructor, y
// uno comprometido no puede así mantener la verificada por encima de
// daemon.build_cache_max_gib. En la de root, que también escriben las
// subidas (PUT /oci/blobs) y los constructores que corren como root, no se
// borra nada de menos de graciaCacheOCI (un import que subió sus blobs y aún
// no ha construido), y solo se barre si nadie la está usando (muCacheOCI).

// LimitesCacheConstruccion son los topes de las cachés de blobs del
// constructor sin root (daemon.build_cache_max_gib y
// daemon.build_cache_max_days). 0 = el de por defecto.
type LimitesCacheConstruccion struct {
	MaxGiB  int
	MaxDays int
}

// Por defecto: 20 GiB entre las dos cachés y 30 días sin usar.
const (
	cacheMaxGiBPorDefecto  = 20
	cacheMaxDiasPorDefecto = 30
)

// BuildCacheMaxGiBTope y BuildCacheMaxDaysTope son lo más que se acepta (1
// PiB, 100 años). Pasado eso, GiB<<30 y días*24h desbordan: un tope a 0 o
// negativo que, en vez de guardar más, lo borraría todo en cada barrido.
const (
	BuildCacheMaxGiBTope  = 1 << 20
	BuildCacheMaxDaysTope = 36500
)

func (l LimitesCacheConstruccion) efectivos() (bytes int64, edad time.Duration) {
	gib, dias := l.MaxGiB, l.MaxDays
	if gib <= 0 {
		gib = cacheMaxGiBPorDefecto
	}
	if dias <= 0 {
		dias = cacheMaxDiasPorDefecto
	}
	gib, dias = min(gib, BuildCacheMaxGiBTope), min(dias, BuildCacheMaxDaysTope)
	return int64(gib) << 30, time.Duration(dias) * 24 * time.Hour
}

// SetBuildCache fija de dónde lee el daemon los topes de las cachés del
// constructor. Se consulta en cada construcción: cambiarlos no pide reiniciar.
func (s *Server) SetBuildCache(f func() LimitesCacheConstruccion) { s.limitesCache = f }

func (s *Server) limitesCacheConstruccion() LimitesCacheConstruccion {
	if s.limitesCache == nil {
		return LimitesCacheConstruccion{}
	}
	return s.limitesCache()
}

// ficheroUsados es la lista de blobs que deja el constructor oci en su
// directorio de trabajo; maxUsados, cuántas líneas se leen como mucho.
const (
	ficheroUsados = "cache-used"
	maxUsados     = 1024
)

var reDigestBlob = lazyre.New(`^sha256:[0-9a-f]{64}$`)
var reHexBlob = lazyre.New(`^[0-9a-f]{64}$`)

// graciaCacheOCI: en la caché de blobs de root no se barre nada más nuevo
// que esto. Lo que sube un import (PUT /oci/blobs) espera ahí a su
// construcción, y una descarga como root a medias es un .part.
const graciaCacheOCI = 2 * time.Hour

// grupoCaches es el grupo y el modo de los directorios de las cachés de root:
// las lee el usuario de construcción por su grupo; sin él, nadie más que root.
func grupoCaches(u *usuarioConstructor) (gid uint32, modo os.FileMode) {
	if u == nil {
		return 0, 0o700
	}
	return u.GID, 0o750
}

// modoBlob es el de un blob en un directorio de caché de modo dir: lectura
// para quien pueda entrar en él, escritura solo para root.
func modoBlob(dir os.FileMode) os.FileMode { return dir & 0o640 }

// cerrarDirsCache deja dirs (de fuera adentro, creándolos) como directorios
// de root (si el daemon lo es) del grupo gid y con el modo modo. Si el último
// no estaba ya así (de otra versión, 0755, o de cuando el grupo era otro),
// también sus ficheros: de root, gid y modoBlob. Un nivel que no sea un
// directorio de verdad (un enlace) es un error: ahí no escribe nadie más que
// root.
func cerrarDirsCache(dirs []string, gid uint32, modo os.FileMode) error {
	raiz := os.Geteuid() == 0
	cambio := false
	for _, d := range dirs {
		if err := os.Mkdir(d, modo); err != nil && !os.IsExist(err) {
			return err
		}
		fi, err := os.Lstat(d)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", d)
		}
		st, _ := fi.Sys().(*syscall.Stat_t)
		cambio = fi.Mode().Perm() != modo || (raiz && (st == nil || st.Uid != 0 || st.Gid != gid))
		if !cambio {
			continue
		}
		if raiz {
			if err := os.Lchown(d, 0, int(gid)); err != nil {
				return err
			}
		}
		if err := os.Chmod(d, modo); err != nil {
			return err
		}
	}
	if cambio && len(dirs) > 0 {
		ajustarBlobs(dirs[len(dirs)-1], gid, modoBlob(modo), raiz)
	}
	return nil
}

// ajustarBlobs pone dueño y modo a los ficheros regulares de dir (sin seguir
// enlaces: dir solo lo escribe root).
func ajustarBlobs(dir string, gid uint32, modo os.FileMode, raiz bool) {
	es, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range es {
		p := filepath.Join(dir, e.Name())
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if raiz {
			_ = os.Lchown(p, 0, int(gid))
		}
		_ = os.Chmod(p, modo)
	}
}

// prepararVerificada deja <root>/cache/verified/oci/sha256 de root, con el
// grupo de u y 0750 en cada nivel (cerrarDirsCache), y devuelve
// <root>/cache/verified (lo que recibe el constructor).
func prepararVerificada(root string, u *usuarioConstructor) (string, error) {
	v := filepath.Join(root, "cache", "verified")
	gid, modo := grupoCaches(u)
	return v, cerrarDirsCache([]string{v, filepath.Join(v, "oci"), filepath.Join(v, "oci", "sha256")}, gid, modo)
}

// dirCacheOCI es la caché de blobs de root: la de los constructores que
// corren como root y adonde suben los imports de un archivo.
func dirCacheOCI(root string) string { return filepath.Join(root, "cache", "oci", "sha256") }

// cerrarCacheOCI deja <root>/cache/oci y su sha256/ de root, con el grupo de
// u y 0750 (o 0700 sin él). Antes de cada subida y de cada construcción: un
// constructor como root la crearía 0755 con su umask.
func cerrarCacheOCI(root string, u *usuarioConstructor) error {
	if err := os.MkdirAll(filepath.Join(root, "cache"), 0o755); err != nil {
		return err
	}
	gid, modo := grupoCaches(u)
	d := dirCacheOCI(root)
	return cerrarDirsCache([]string{filepath.Dir(d), d}, gid, modo)
}

// leerUsados lee la lista de blobs de una construcción: sin seguir enlaces,
// un fichero regular, y solo las líneas que son un digest.
func leerUsados(p string) ([]string, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", ficheroUsados)
	}
	var out []string
	vistos := map[string]bool{}
	sc := bufio.NewScanner(io.LimitReader(f, maxUsados*80))
	for sc.Scan() && len(out) < maxUsados {
		d := strings.TrimSpace(sc.Text())
		if reDigestBlob.MatchString(d) && !vistos[d] {
			vistos[d] = true
			out = append(out, d)
		}
	}
	return out, nil
}

// dirDelConstructor dice si fi es un directorio de verdad (no un enlace) del
// constructor o de root.
func dirDelConstructor(fi os.FileInfo, uid uint32) bool {
	if !fi.IsDir() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && (st.Uid == uid || st.Uid == 0)
}

// abrirPropia abre <cache>/oci/sha256, la caché de blobs del constructor,
// como un os.Root, o nil si no está o no es un directorio suyo (o de root).
// Todo lo que el daemon hace después ahí (listar, abrir, borrar) va relativo
// a ese directorio ya abierto y no sale de él por un enlace: aunque un
// proceso suyo que sobreviviera al barrido cambiara cache/oci por un enlace a
// /etc después de mirarlo, no se borra ni se lee nada fuera. cache
// (<root>/cache/builder) cuelga de un directorio de root: no lo puede
// cambiar él.
func abrirPropia(cache string, uid uint32) *os.Root {
	if fi, err := os.Lstat(cache); err != nil || !dirDelConstructor(fi, uid) {
		return nil
	}
	r, err := os.OpenRoot(cache)
	if err != nil {
		return nil
	}
	defer r.Close()
	sub, err := r.OpenRoot(filepath.Join("oci", "sha256"))
	if err != nil {
		return nil
	}
	if fi, err := sub.Stat("."); err != nil || !dirDelConstructor(fi, uid) {
		sub.Close()
		return nil
	}
	return sub
}

// promoverCache pasa a la caché verificada (verificada/oci) los blobs que
// usó una construcción correcta (la lista de work) y que aún están solo en la
// del constructor (cache/oci) o en la de root (subidos, "" sin ella); a los
// que ya estaban les pone la fecha de hoy (para el barrido). Devuelve los
// digests usados (lo que el barrido no toca) y cuántos pasó. Un blob de la
// del constructor que no cuadra se borra de ella y no entra. maxBytes es el
// tope de las cachés: en una pasada no se copian más bytes que eso (el
// constructor no llena el disco del host a través de root); lo que no cabe se
// queda en la suya, sin verificar. Lo de root se enlaza: no ocupa más.
func promoverCache(work, cache, verificada, subidos string, u *usuarioConstructor, maxBytes int64) (usados []string, n int, err error) {
	usados, err = leerUsados(filepath.Join(work, ficheroUsados))
	if err != nil || len(usados) == 0 {
		return nil, 0, err
	}
	dstDir := filepath.Join(verificada, "oci", "sha256")
	propia := abrirPropia(cache, u.UID)
	if propia != nil {
		defer propia.Close()
	}
	ahora := time.Now()
	var errs []error
	var copiados int64
	for _, d := range usados {
		h := strings.TrimPrefix(d, "sha256:")
		dst := filepath.Join(dstDir, h)
		if fi, lerr := os.Lstat(dst); lerr == nil && fi.Mode().IsRegular() {
			_ = os.Chtimes(dst, ahora, ahora)
			if propia != nil {
				_ = propia.Remove(h) // ya no hace falta allí
			}
			continue
		}
		if propia != nil {
			tam, cerr := copiarVerificado(propia, h, dst, d, u, maxBytes-copiados)
			if cerr != nil {
				errs = append(errs, fmt.Errorf("%s: %w", d, cerr))
			}
			if tam >= 0 && cerr == nil {
				n++
				copiados += tam
			}
			if tam >= 0 && !errors.Is(cerr, errSinSitio) {
				// Verificado ya está en la otra; dañado no sirve: fuera de la suya.
				_ = propia.Remove(h)
			}
			if tam >= 0 {
				continue
			}
		}
		if subidos != "" {
			ok, lerr := enlazarDeRoot(filepath.Join(subidos, h), dst, u.GID, ahora)
			if lerr != nil {
				errs = append(errs, fmt.Errorf("%s: %w", d, lerr))
			}
			if ok {
				n++
			}
		}
	}
	return usados, n, errors.Join(errs...)
}

// enlazarDeRoot pasa src, un blob de la caché de root, a dst en la
// verificada con un enlace duro (mismo sistema de ficheros, sin copiar) y lo
// quita de src. false y nil si no está. Solo un fichero regular del daemon
// sin escritura para grupo ni otros: lo que dejan ahí handlePutOCIBlob y las
// descargas como root, después de comprobar su sha256.
func enlazarDeRoot(src, dst string, gid uint32, ahora time.Time) (bool, error) {
	fi, err := os.Lstat(src)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || st.Uid != uint32(os.Geteuid()) || fi.Mode().Perm()&0o022 != 0 {
		return false, errors.New("not a blob of the daemon's cache")
	}
	if err := os.Link(src, dst); err != nil {
		return false, err
	}
	if os.Geteuid() == 0 {
		_ = os.Lchown(dst, 0, int(gid))
	}
	_ = os.Chmod(dst, 0o640)
	_ = os.Chtimes(dst, ahora, ahora)
	_ = os.Remove(src)
	return true, nil
}

// errSinSitio: el blob no cabe en lo que queda del tope en esta pasada.
var errSinSitio = errors.New("over the builder cache limit (daemon.build_cache_max_gib)")

// copiarVerificado copia el blob nombre de src (la caché del constructor) a
// dst (en la verificada) si sus bytes tienen el sha256 digest, y devuelve
// cuántos bytes copió; -1 y nil si no está. No copia más de max bytes ni un
// fichero disperso (el constructor podría pedir así a root TiB de ceros sin
// gastar disco). El fichero nuevo es de root (de quien corre el daemon), del
// grupo de u y 0640, y llega a dst con un rename después del fsync: dst o
// está entero y verificado, o no está.
func copiarVerificado(src *os.Root, nombre, dst, digest string, u *usuarioConstructor, max int64) (int64, error) {
	// O_NONBLOCK: abrir una FIFO plantada no se queda esperando. Un enlace
	// solo se sigue si no sale de src (os.Root); lo que se abre se mira
	// abajo con fstat igual.
	f, err := src.OpenFile(nombre, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return 0, errors.New("not a regular file")
	case !ok || (st.Uid != u.UID && st.Uid != 0):
		return 0, fmt.Errorf("not owned by the builder (uid %d)", u.UID)
	case st.Uid == 0 && fi.Mode().Perm()&0o004 == 0 && (st.Gid != u.GID || fi.Mode().Perm()&0o040 == 0):
		// Uno de root que el constructor no puede leer (un enlace duro a
		// algo privado, donde no hay protected_hardlinks): copiarlo a la
		// verificada, que él lee, se lo enseñaría, y aceptarlo o no le diría
		// si acertó su sha256. Los de root legítimos (los enlazados de
		// cache/oci) son 0640 de su grupo, o 0644 de antes.
		return 0, errors.New("owned by root and not readable by the builder")
	case st.Blocks*512+4096 < fi.Size():
		// Disperso: lo que ocupa en disco no llega a lo que dice medir (con
		// margen para lo que el sistema de ficheros guarda en línea). Un
		// blob de verdad se escribe entero.
		return 0, errors.New("sparse file")
	case fi.Size() > max:
		return 0, errSinSitio
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name()) // si llega a dst, ya no existe con este nombre
	defer tmp.Close()
	hs := sha256.New()
	// Lo que dice el fstat y ni un byte más: si el fichero creciera (no
	// debería: no queda nadie suyo vivo) no se copia lo que no se miró.
	n, err := io.Copy(io.MultiWriter(tmp, hs), io.LimitReader(f, fi.Size()))
	if err != nil {
		return 0, err
	}
	if n != fi.Size() {
		return 0, fmt.Errorf("short read (%d of %d bytes)", n, fi.Size())
	}
	if got := "sha256:" + hex.EncodeToString(hs.Sum(nil)); got != digest {
		return 0, fmt.Errorf("sha256 mismatch (got %s)", got)
	}
	if os.Geteuid() == 0 {
		if err := tmp.Chown(0, int(u.GID)); err != nil {
			return 0, err
		}
	}
	if err := tmp.Chmod(0o640); err != nil {
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return 0, err
	}
	return n, nil
}

// trasAbrirPropia es para los tests: el constructor cambiando su caché justo
// después de que el daemon la mire.
var trasAbrirPropia = func() {}

// Las cachés de blobs, en el orden en que se barren: primero la del
// constructor (sin verificar), luego la de root y al final la verificada.
const (
	cachePropia = iota
	cacheDeRoot
	cacheVerificada
)

// entradaCache es un fichero de una de las cachés, para el barrido.
type entradaCache struct {
	dir    *os.Root
	nombre string
	tam    int64
	mod    time.Time
	clase  int
}

// barrerCachesConstruccion barre las cachés de blobs (ver arriba): cache es
// la del constructor (<root>/cache/builder), verificada la verificada
// (<root>/cache/verified) y subidos la de root (<root>/cache/oci/sha256); la
// que vaya vacía no se barre. conservar son los digests que acaba de usar
// una construcción; maxBytes y maxEdad, los topes (LimitesCacheConstruccion).
// Devuelve cuántos ficheros borró y cuántos bytes liberó. Corre con los
// procesos del constructor barridos, y aun así lo suyo se recorre y se borra
// a través de abrirPropia: nada fuera de su caché. En subidos no se toca lo
// de menos de graciaCacheOCI (cuenta para el tope, pero no se borra).
func barrerCachesConstruccion(cache, verificada, subidos string, uid uint32, maxBytes int64, maxEdad time.Duration,
	conservar []string, ahora time.Time) (borrados int, liberados int64) {
	guardar := map[string]bool{}
	for _, d := range conservar {
		guardar[strings.TrimPrefix(d, "sha256:")] = true
	}
	var blobs, usados []entradaCache
	var total int64
	recorrer := func(dir *os.Root, clase int) {
		d, err := dir.Open(".")
		if err != nil {
			return
		}
		entradas, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return
		}
		for _, e := range entradas {
			fi, err := dir.Lstat(e.Name())
			if err != nil || fi.IsDir() {
				continue
			}
			if clase == cacheDeRoot && ahora.Sub(fi.ModTime()) < graciaCacheOCI {
				// Una subida o una descarga en curso, o lo que espera a su
				// construcción.
				if fi.Mode().IsRegular() {
					total += fi.Size()
				}
				continue
			}
			if !fi.Mode().IsRegular() || !reHexBlob.MatchString(e.Name()) {
				// .part y .tmp- a medias (nadie escribe ahora) y lo que no es
				// un blob. Un unlink no sigue enlaces.
				if dir.Remove(e.Name()) == nil {
					borrados++
					if fi.Mode().IsRegular() {
						liberados += fi.Size()
					}
				}
				continue
			}
			ent := entradaCache{dir: dir, nombre: e.Name(), tam: fi.Size(), mod: fi.ModTime(), clase: clase}
			if guardar[e.Name()] {
				usados = append(usados, ent)
			} else {
				blobs = append(blobs, ent)
			}
			total += fi.Size()
		}
	}
	if cache != "" {
		if propia := abrirPropia(cache, uid); propia != nil {
			defer propia.Close()
			trasAbrirPropia()
			recorrer(propia, cachePropia)
		}
	}
	if subidos != "" {
		if r, err := os.OpenRoot(subidos); err == nil {
			defer r.Close()
			recorrer(r, cacheDeRoot)
		}
	}
	if verificada != "" {
		if v, err := os.OpenRoot(filepath.Join(verificada, "oci", "sha256")); err == nil {
			defer v.Close()
			recorrer(v, cacheVerificada)
		}
	}

	// Lo usado se guarda mientras quepa en el tope, primero lo verificado
	// (lo que la próxima construcción lee sin rehashear); lo que no cabe se
	// barre como lo demás. La lista la escribe el constructor: sin esto,
	// nombrando blobs verificados mantendría la caché por encima del tope.
	sort.SliceStable(usados, func(i, j int) bool { return usados[i].clase > usados[j].clase })
	var protegidos int64
	for _, u := range usados {
		if protegidos+u.tam <= maxBytes {
			protegidos += u.tam
			continue
		}
		blobs = append(blobs, u)
	}

	// Lo no verificado antes (su fecha la pone el constructor: no se le
	// cree para quedarse delante), luego lo de root, y dentro de cada una lo
	// más viejo.
	sort.Slice(blobs, func(i, j int) bool {
		if blobs[i].clase != blobs[j].clase {
			return blobs[i].clase < blobs[j].clase
		}
		return blobs[i].mod.Before(blobs[j].mod)
	})
	for _, b := range blobs {
		viejo := ahora.Sub(b.mod) > maxEdad
		if !viejo && total <= maxBytes {
			continue
		}
		if b.dir.Remove(b.nombre) == nil {
			borrados++
			liberados += b.tam
			total -= b.tam
		}
	}
	return borrados, liberados
}

// cacheConstruccion es lo que hace el daemon con las cachés al acabar una
// construcción: si fue sin root y bien, pasar a la verificada lo que usó; y
// siempre, barrer. limpio dice si el barrido de sus procesos acabó sin
// ninguno vivo (barrerProcesos): si no, su caché no se toca (ni se promueve
// ni se barre). Con u nil (el constructor corrió como el daemon) solo hay la
// de root. La de root se usa y se barre solo si nadie la está usando
// (muCacheOCI). Nada de esto hace fallar la construcción: se avisa.
func (s *Server) cacheConstruccion(work, cache, verificada string, u *usuarioConstructor, bien, limpio bool) {
	maxBytes, maxEdad := s.limitesCacheConstruccion().efectivos()
	subidos := ""
	if s.root != "" && s.muCacheOCI.TryLock() {
		defer s.muCacheOCI.Unlock()
		subidos = dirCacheOCI(s.root)
	}
	uid := uint32(os.Geteuid())
	if u == nil || cache == "" || verificada == "" {
		cache, verificada, bien = "", "", false
	} else {
		uid = u.UID
		if !limpio {
			log.Printf("WARNING: builder cache: processes of uid %d still alive; not promoting or sweeping its cache", u.UID)
			cache, bien = "", false
		}
	}
	var usados []string
	if bien {
		t0 := time.Now()
		var n int
		var err error
		usados, n, err = promoverCache(work, cache, verificada, subidos, u, maxBytes)
		if err != nil {
			log.Printf("builder cache: %v", err)
		}
		if n > 0 {
			log.Printf("builder cache: %d blob(s) verified into %s in %s", n, verificada, time.Since(t0).Round(time.Millisecond))
		}
	}
	if b, lib := barrerCachesConstruccion(cache, verificada, subidos, uid, maxBytes, maxEdad, usados, time.Now()); b > 0 {
		log.Printf("builder cache: removed %d file(s), %d MiB", b, lib>>20)
	}
}

// ajustesCache son los topes efectivos de las cachés, para GET /info.
func (s *Server) ajustesCache(a map[string]string) {
	b, edad := s.limitesCacheConstruccion().efectivos()
	a["KLING_BUILD_CACHE_MAX_GIB"] = strconv.FormatInt(b>>30, 10)
	a["KLING_BUILD_CACHE_MAX_DAYS"] = strconv.Itoa(int(edad / (24 * time.Hour)))
}
