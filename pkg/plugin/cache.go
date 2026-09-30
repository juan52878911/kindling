package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// La caché de manifiestos evita ejecutar cada extensión con --kling-manifest
// en cada `kling <comando>`: un binario Go de 13 MB tarda ~5 ms solo en
// arrancar y decir qué es, y el gancho post-checkout de `kling db branch` lo
// paga en cada git checkout.
//
// Lo que guarda es solo lo que la extensión imprimió, indexado por la ruta y
// la identidad del fichero (dispositivo, inodo, tamaño, modo, dueño, mtime y
// ctime). Nunca decide qué se ejecuta: la ruta sale siempre del descubrimiento,
// y lo que sale de la caché se valida con parseManifest igual que lo recién
// ejecutado (y Discover comprueba nombre y MinKling después, como siempre). Si
// el binario cambia —reinstalado, recompilado, sobrescrito en su sitio— cambia
// su ctime, que nadie sin privilegios puede fijar a mano, y la entrada ya no
// sirve.

// cacheFormat es la versión del fichero de caché: otra se ignora entera.
const cacheFormat = 1

// maxCacheBytes y maxCacheEntries acotan lo que se lee y lo que se guarda.
const (
	maxCacheBytes   = 1 << 20
	maxCacheEntries = 256
)

// racyWindow: un binario cambiado hace menos de esto no se guarda. Dos
// escrituras del mismo tamaño dentro del mismo tic del reloj del sistema de
// ficheros dejan igual mtime y ctime; esperar a que el fichero lleve un rato
// quieto es lo que hace git con su índice por lo mismo.
var racyWindow = 2 * time.Second

// DefaultManifestCache es dónde guarda kling la caché de manifiestos:
// $XDG_STATE_HOME/kling/plugins, o ~/.local/state/kling/plugins, como el estado
// de kling-db. "" si no hay HOME. No va en ~/.cache: un `sudo` que conserva el
// HOME lo deja a veces de root, y entonces no habría caché.
func DefaultManifestCache() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return ""
		}
		base = filepath.Join(h, ".local", "state")
	}
	return filepath.Join(base, "kling", "plugins", "manifests.json")
}

// fileID es la identidad de un binario. Si cualquier campo cambia, la entrada
// de la caché ya no vale.
type fileID struct {
	Dev   uint64 `json:"dev"`
	Ino   uint64 `json:"ino"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	UID   uint32 `json:"uid"`
	Mtime int64  `json:"mtime"`
	Ctime int64  `json:"ctime"`
}

// statID es la identidad de path siguiendo enlaces, como exec. ok=false si no
// se puede saber (y entonces no se usa la caché para ese binario).
func statID(path string) (fileID, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return fileID{}, false
	}
	return fileIDOf(fi)
}

// recent dice si el fichero cambió dentro de racyWindow antes de now.
func (id fileID) recent(now time.Time) bool {
	edge := now.Add(-racyWindow).UnixNano()
	return id.Mtime >= edge || id.Ctime >= edge
}

type cacheEntry struct {
	ID       fileID          `json:"id"`
	Kling    string          `json:"kling"` // versión del núcleo que lo pidió
	API      string          `json:"api"`   // KLING_PLUGIN_API de entonces
	Manifest json.RawMessage `json:"manifest"`
}

type cacheFile struct {
	Format  int                    `json:"format"`
	Entries map[string]*cacheEntry `json:"entries"`
}

// manifestCache es la caché de un Discover. No es para varias goroutines: la
// usa un único descubrimiento de principio a fin.
type manifestCache struct {
	path    string
	version string
	entries map[string]*cacheEntry
	dirty   bool
}

// openManifestCache lee la caché de path. Un fichero ausente, ilegible, ajeno
// o con permisos para otros es una caché vacía: nunca un error. path vacío es
// no usar caché (nil).
func openManifestCache(path, version string) *manifestCache {
	if path == "" {
		return nil
	}
	c := &manifestCache{path: path, version: version, entries: map[string]*cacheEntry{}}
	if raw, err := readPrivate(path); err == nil {
		var f cacheFile
		if json.Unmarshal(raw, &f) == nil && f.Format == cacheFormat {
			for k, e := range f.Entries {
				if e != nil && filepath.IsAbs(k) {
					c.entries[k] = e
				}
			}
		}
	}
	return c
}

// manifest es loadManifest con caché. c nil es loadManifest a secas.
func (c *manifestCache) manifest(ctx context.Context, path string) (*Manifest, error) {
	if c == nil {
		return loadManifest(ctx, path)
	}
	before, ok := statID(path)
	if e := c.entries[path]; e != nil {
		if ok && e.ID == before && e.Kling == c.version && e.API == APIVersion {
			if m, err := parseManifest(e.Manifest); err == nil {
				return m, nil
			}
		}
		// Vieja o inválida: fuera, y se vuelve a preguntar.
		delete(c.entries, path)
		c.dirty = true
	}
	raw, err := runManifest(ctx, path)
	if err != nil {
		return nil, err
	}
	m, err := parseManifest(raw)
	if err != nil {
		return nil, err // un manifiesto roto no se guarda: se vuelve a preguntar
	}
	// Solo se guarda si el binario no cambió mientras contestaba y lleva un
	// rato quieto: si no, lo guardado podría ser de otro fichero.
	after, ok2 := statID(path)
	if ok && ok2 && before == after && !before.recent(time.Now()) {
		c.entries[path] = &cacheEntry{ID: before, Kling: c.version, API: APIVersion, Manifest: bytes.Clone(raw)}
		c.dirty = true
	}
	return m, nil
}

// save escribe la caché si cambió. Tira las entradas de binarios que ya no
// están o ya no son los mismos. Los errores se ignoran: la caché es un atajo.
func (c *manifestCache) save() {
	if c == nil || !c.dirty {
		return
	}
	f := cacheFile{Format: cacheFormat, Entries: map[string]*cacheEntry{}}
	for k, e := range c.entries {
		if len(f.Entries) >= maxCacheEntries {
			break
		}
		if id, ok := statID(k); ok && id == e.ID {
			f.Entries[k] = e
		}
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return
	}
	_ = writePrivate(c.path, raw)
}

// errNotPrivate es un fichero o directorio de caché que no es solo nuestro.
var errNotPrivate = errors.New("not private to this user")

// privateDir comprueba que dir es un directorio (no un enlace) de este usuario
// sin permisos para nadie más.
func privateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 || !ownedByMe(fi) {
		return fmt.Errorf("%s: %w", dir, errNotPrivate)
	}
	return nil
}

// mkdirMine crea dir (0700) solo si el antepasado más cercano que ya existe es
// de este usuario: un kling con sudo que conserva el HOME no deja directorios
// de root en el HOME de otro, que luego ese otro no podría usar.
func mkdirMine(dir string) error {
	for p := dir; ; {
		fi, err := os.Stat(p)
		if err == nil {
			if !ownedByMe(fi) {
				return fmt.Errorf("%s: %w", p, errNotPrivate)
			}
			break
		}
		up := filepath.Dir(p)
		if up == p {
			break
		}
		p = up
	}
	return os.MkdirAll(dir, 0o700)
}

// readPrivate lee path solo si él y su directorio son de este usuario y de
// nadie más, sin seguir un enlace en el último componente.
func readPrivate(path string) ([]byte, error) {
	if err := privateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 || !ownedByMe(fi) {
		return nil, fmt.Errorf("%s: %w", path, errNotPrivate)
	}
	if fi.Size() > maxCacheBytes {
		return nil, fmt.Errorf("%s: too big", path)
	}
	return io.ReadAll(io.LimitReader(f, maxCacheBytes))
}

// writePrivate escribe path de una vez (temporal y rename) con 0600, en un
// directorio 0700. Dos kling a la vez: gana el último, y nadie lee a medias.
func writePrivate(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := mkdirMine(dir); err != nil {
		return err
	}
	if err := privateDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".manifests-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
