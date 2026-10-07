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
//	<root>/cache/verified/oci/sha256/<hex>   root, directorios 0755, ficheros 0644
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
//
// El constructor la lee (KLING_VERIFIED_CACHE_DIR) y no puede escribir en ella
// ni renombrar nada: los directorios no son suyos ni tienen escritura para
// otros. El cliente OCI usa un blob de ahí sin rehashear solo si lo comprueba
// así (oci.verificado); si no, es como si no estuviera.
//
// Las dos cachés crecían sin límite. Después de cada construcción sin root se
// barren (barrerCachesConstruccion): fuera los .part y lo que no es un blob,
// lo que lleva más de daemon.build_cache_max_days sin usarse (en la
// verificada, cada uso le pone la fecha) y, si entre las dos pasan de
// daemon.build_cache_max_gib, lo más viejo, primero lo no verificado. Lo que
// acaba de usar la construcción no se toca.

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

func (l LimitesCacheConstruccion) efectivos() (bytes int64, edad time.Duration) {
	gib, dias := l.MaxGiB, l.MaxDays
	if gib <= 0 {
		gib = cacheMaxGiBPorDefecto
	}
	if dias <= 0 {
		dias = cacheMaxDiasPorDefecto
	}
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

// prepararVerificada deja <root>/cache/verified/oci/sha256, de root y 0755
// en cada nivel, y devuelve <root>/cache/verified (lo que recibe el
// constructor). Cualquier nivel que no sea un directorio de verdad (un
// enlace) es un error: ahí no escribe nadie más que root.
func prepararVerificada(root string) (string, error) {
	v := filepath.Join(root, "cache", "verified")
	for _, d := range []string{v, filepath.Join(v, "oci"), filepath.Join(v, "oci", "sha256")} {
		if err := os.Mkdir(d, 0o755); err != nil && !os.IsExist(err) {
			return "", err
		}
		fi, err := os.Lstat(d)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%s is not a directory", d)
		}
		if os.Geteuid() == 0 {
			if err := os.Lchown(d, 0, 0); err != nil {
				return "", err
			}
		}
		if err := os.Chmod(d, 0o755); err != nil {
			return "", err
		}
	}
	return v, nil
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

// dirDelConstructor dice si d es un directorio de verdad (no un enlace) del
// constructor o de root. Con sus procesos barridos no cambia mientras se
// mira y se usa.
func dirDelConstructor(d string, uid uint32) bool {
	fi, err := os.Lstat(d)
	if err != nil || !fi.IsDir() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && (st.Uid == uid || st.Uid == 0)
}

// promoverCache pasa a la caché verificada (verificada/oci) los blobs que
// usó una construcción correcta (la lista de work) y que aún están solo en la
// del constructor (cache/oci); a los que ya estaban les pone la fecha de hoy
// (para el barrido). Devuelve los digests usados (lo que el barrido no toca)
// y cuántos pasó. Un blob que no cuadra se borra de la caché del constructor
// y no entra.
func promoverCache(work, cache, verificada string, uid uint32) (usados []string, n int, err error) {
	usados, err = leerUsados(filepath.Join(work, ficheroUsados))
	if err != nil || len(usados) == 0 {
		return nil, 0, err
	}
	srcDir := filepath.Join(cache, "oci", "sha256")
	dstDir := filepath.Join(verificada, "oci", "sha256")
	propia := dirDelConstructor(cache, uid) && dirDelConstructor(filepath.Join(cache, "oci"), uid) && dirDelConstructor(srcDir, uid)
	ahora := time.Now()
	var errs []error
	for _, d := range usados {
		h := strings.TrimPrefix(d, "sha256:")
		dst := filepath.Join(dstDir, h)
		if fi, lerr := os.Lstat(dst); lerr == nil && fi.Mode().IsRegular() {
			_ = os.Chtimes(dst, ahora, ahora)
			if propia {
				_ = os.Remove(filepath.Join(srcDir, h)) // ya no hace falta allí
			}
			continue
		}
		if !propia {
			continue
		}
		src := filepath.Join(srcDir, h)
		ok, cerr := copiarVerificado(src, dst, d, uid)
		if cerr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d, cerr))
		}
		if ok {
			n++
		}
		if ok || cerr != nil {
			// Verificado ya está en la otra; dañado no sirve: fuera de la suya.
			_ = os.Remove(src)
		}
	}
	return usados, n, errors.Join(errs...)
}

// copiarVerificado copia src (de la caché del constructor) a dst (en la
// verificada) si sus bytes tienen el sha256 digest. false y nil si src no
// está. El fichero nuevo es de root (de quien corre el daemon) y 0644, y
// llega a dst con un rename después del fsync: dst o está entero y
// verificado, o no está.
func copiarVerificado(src, dst, digest string, uid uint32) (bool, error) {
	// O_NONBLOCK: abrir una FIFO plantada no se queda esperando.
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return false, errors.New("not a regular file")
	case !ok || (st.Uid != uid && st.Uid != 0):
		return false, fmt.Errorf("not owned by the builder (uid %d)", uid)
	case st.Uid == 0 && fi.Mode().Perm()&0o004 == 0:
		// Uno de root que el constructor no puede leer (un enlace duro a
		// algo privado, donde no hay protected_hardlinks): copiarlo a la
		// verificada, 0644, se lo enseñaría, y aceptarlo o no le diría si
		// acertó su sha256. Los de root legítimos (los enlazados de
		// cache/oci) son 0644.
		return false, errors.New("owned by root and not world-readable")
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name()) // si llega a dst, ya no existe con este nombre
	defer tmp.Close()
	hs := sha256.New()
	// Lo que dice el fstat y ni un byte más: si el fichero creciera (no
	// debería: no queda nadie suyo vivo) no se copia lo que no se miró.
	n, err := io.Copy(io.MultiWriter(tmp, hs), io.LimitReader(f, fi.Size()))
	if err != nil {
		return false, err
	}
	if n != fi.Size() {
		return false, fmt.Errorf("short read (%d of %d bytes)", n, fi.Size())
	}
	if got := "sha256:" + hex.EncodeToString(hs.Sum(nil)); got != digest {
		return false, fmt.Errorf("sha256 mismatch (got %s)", got)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return false, err
	}
	return true, nil
}

// entradaCache es un fichero de una de las dos cachés, para el barrido.
type entradaCache struct {
	ruta       string
	tam        int64
	mod        time.Time
	verificada bool
}

// barrerCachesConstruccion barre las dos cachés de blobs del constructor (ver
// arriba): cache es la suya (<root>/cache/builder), verificada la de root
// (<root>/cache/verified). conservar son los digests que acaba de usar una
// construcción; maxBytes y maxEdad, los topes (LimitesCacheConstruccion).
// Devuelve cuántos ficheros borró y cuántos bytes liberó. Corre con los
// procesos del constructor barridos: su directorio no cambia mientras se
// recorre.
func barrerCachesConstruccion(cache, verificada string, uid uint32, maxBytes int64, maxEdad time.Duration,
	conservar []string, ahora time.Time) (borrados int, liberados int64) {
	guardar := map[string]bool{}
	for _, d := range conservar {
		guardar[strings.TrimPrefix(d, "sha256:")] = true
	}
	var blobs []entradaCache
	var total int64
	recorrer := func(dir string, esVerificada bool) {
		entradas, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entradas {
			p := filepath.Join(dir, e.Name())
			fi, err := os.Lstat(p)
			if err != nil || fi.IsDir() {
				continue
			}
			if !fi.Mode().IsRegular() || !reHexBlob.MatchString(e.Name()) {
				// .part y .tmp- a medias (nadie escribe ahora) y lo que no es
				// un blob. Un unlink no sigue enlaces.
				if os.Remove(p) == nil {
					borrados++
					if fi.Mode().IsRegular() {
						liberados += fi.Size()
					}
				}
				continue
			}
			if guardar[e.Name()] {
				total += fi.Size()
				continue
			}
			blobs = append(blobs, entradaCache{ruta: p, tam: fi.Size(), mod: fi.ModTime(), verificada: esVerificada})
			total += fi.Size()
		}
	}
	if propia := filepath.Join(cache, "oci", "sha256"); dirDelConstructor(cache, uid) &&
		dirDelConstructor(filepath.Join(cache, "oci"), uid) && dirDelConstructor(propia, uid) {
		recorrer(propia, false)
	}
	recorrer(filepath.Join(verificada, "oci", "sha256"), true)

	// Lo no verificado antes (su fecha la pone el constructor: no se le
	// cree para quedarse delante), y dentro de cada una lo más viejo.
	sort.Slice(blobs, func(i, j int) bool {
		if blobs[i].verificada != blobs[j].verificada {
			return !blobs[i].verificada
		}
		return blobs[i].mod.Before(blobs[j].mod)
	})
	for _, b := range blobs {
		viejo := ahora.Sub(b.mod) > maxEdad
		if !viejo && total <= maxBytes {
			continue
		}
		if os.Remove(b.ruta) == nil {
			borrados++
			liberados += b.tam
			total -= b.tam
		}
	}
	return borrados, liberados
}

// cacheConstruccion es lo que hace el daemon con las cachés al acabar una
// construcción sin root: si fue bien, pasar a la verificada lo que usó; y
// siempre, barrer. Nada de esto hace fallar la construcción: se avisa.
func (s *Server) cacheConstruccion(work, cache, verificada string, u *usuarioConstructor, bien bool) {
	if cache == "" || verificada == "" {
		return
	}
	var usados []string
	if bien {
		t0 := time.Now()
		var n int
		var err error
		usados, n, err = promoverCache(work, cache, verificada, u.UID)
		if err != nil {
			log.Printf("builder cache: %v", err)
		}
		if n > 0 {
			log.Printf("builder cache: %d blob(s) verified into %s in %s", n, verificada, time.Since(t0).Round(time.Millisecond))
		}
	}
	maxBytes, maxEdad := s.limitesCacheConstruccion().efectivos()
	if b, lib := barrerCachesConstruccion(cache, verificada, u.UID, maxBytes, maxEdad, usados, time.Now()); b > 0 {
		log.Printf("builder cache: removed %d file(s), %d MiB", b, lib>>20)
	}
}

// ajustesCache son los topes efectivos de las cachés, para GET /info.
func (s *Server) ajustesCache(a map[string]string) {
	b, edad := s.limitesCacheConstruccion().efectivos()
	a["KLING_BUILD_CACHE_MAX_GIB"] = strconv.FormatInt(b>>30, 10)
	a["KLING_BUILD_CACHE_MAX_DAYS"] = strconv.Itoa(int(edad / (24 * time.Hour)))
}
