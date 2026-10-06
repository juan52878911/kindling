package machine

// El espejo de la memoria de un dorado en el almacén de copia al escribir.
//
// Descongelar una copia congelada en diferencial (diff_volcado.go) necesita
// base + diff en un solo fichero. Con la raíz en ext4 eso era copiar la base
// entera a mem.full en cada despertar: medido con Hindsight, 1,3 GiB y de 4 a
// 5 s, en vez de los milisegundos de un thaw normal. El almacén (cow.go) ya
// resuelve lo mismo para los discos: una copia "base" por dorado dentro de un
// Btrfs o XFS propio, y un FICLONE por instancia. Aquí se hace igual con el
// mem.file del dorado:
//
//   - bases/<snap>/<clave>.mem es la copia dispersa del mem.file del dorado,
//     una por versión del dorado (la clave es su inodo, tamaño y fecha, como
//     la del overlay), de solo lectura. Es lo único que se paga por dorado.
//   - m/<id>/mem.full es un clon de esa base para la copia id, sobre el que se
//     escriben los tramos del diff: el almacén solo asigna esas páginas. El
//     VMM lo mapea MAP_PRIVATE, así que después no escribe en él nadie: lo que
//     el invitado cambie va a memoria anónima y, al congelar, al diff.
//     machines/<id>/mem.full es un enlace absoluto a él, como el overlay: en
//     el jail resuelve al bind del directorio de la instancia.
//
// Seguridad, igual que con los discos: la base es del daemon y 0400; el clon
// es del daemon, legible por el grupo del VMM y NADA más (0640: el VMM no lo
// escribe, y así un Firecracker comprometido no puede ni crecerlo ni
// cambiarle el proyecto); el directorio de la instancia lleva su cuota, que
// se amplía con el tamaño de la memoria antes de clonar (Btrfs cuenta lo
// referenciado, clon incluido). Y como con los discos, cualquier fallo del
// almacén acaba en el camino de siempre (la copia en la raíz): una
// optimización no deja a nadie sin despertar.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/pkg/durable"
)

// sufijoBaseMemoria distingue en bases/<snap> el espejo de la memoria del del
// overlay (.ext4): base() y barrer solo tocan los de su sufijo.
const sufijoBaseMemoria = ".mem"

// baseMemoria devuelve el espejo del mem.file src del dorado snap dentro del
// almacén, creándolo la primera vez. Con a.mu tomado. Mismo contrato que
// base(): a un temporal y renombrado, para que un espejo a medias no pueda
// quedar con su nombre definitivo.
func (a *almacenCoW) baseMemoria(ctx context.Context, snap, src string) (string, error) {
	if err := nombreSeguro(snap); err != nil {
		return "", err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	dir := a.dirBases(snap)
	ruta := filepath.Join(dir, claveBase(fi)+sufijoBaseMemoria)
	if _, err := os.Lstat(ruta); err == nil {
		return ruta, nil
	}
	// La base entera, más lo que debe quedar para las instancias.
	if _, libre, err := a.libreDentro(); err == nil && libre < allocatedBytes(src)+libreMinimaAlmacen {
		return "", &errAlmacenLleno{libre: libre}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".tmp-"+filepath.Base(ruta))
	_ = os.Remove(tmp)
	if err := a.copiar(ctx, src, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("mirroring the memory of %s into the store: %w", snap, err)
	}
	_ = os.Chmod(tmp, 0o400)
	if err := durable.Renombrar(tmp, ruta); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	// Los espejos de versiones anteriores de este dorado sobran; los .ext4
	// son del overlay y no se tocan.
	if entradas, err := os.ReadDir(dir); err == nil {
		for _, e := range entradas {
			if e.Name() != filepath.Base(ruta) && strings.HasSuffix(e.Name(), sufijoBaseMemoria) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return ruta, nil
}

// memoriaInstancia deja en el almacén la memoria completa de la copia id:
// el espejo del mem.file src del dorado snap clonado y con el diferencial
// diff escrito encima. Devuelve su ruta, ya legible por el VMM.
func (a *almacenCoW) memoriaInstancia(ctx context.Context, snap, src, diff, id string, gib int) (string, error) {
	if err := nombreSeguro(id); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.preparar(ctx, gib); err != nil {
		return "", err
	}
	// El diff se escribe entero sobre el clon: son los únicos bloques que el
	// almacén asigna para esta copia, más lo que debe quedar para las demás.
	if _, libre, err := a.libreDentro(); err == nil && libre < allocatedBytes(diff)+libreMinimaAlmacen {
		return "", &errAlmacenLleno{libre: libre}
	}
	base, err := a.baseMemoria(ctx, snap, src)
	if err != nil {
		return "", err
	}
	d := a.dirInstancia(id)
	nuevo := false
	if _, err := os.Lstat(d); err != nil {
		// Su overlay no vive en el almacén (una copia de antes del almacén, o
		// que no cupo): el directorio es solo para la memoria.
		if err := a.mkdirInstancia(d); err != nil {
			return "", err
		}
		nuevo = true
		if a.priv != nil && a.priv.Enabled {
			if err := os.Lchown(d, os.Geteuid(), a.priv.GID); err != nil {
				a.rmdirInstancia(d)
				return "", fmt.Errorf("securing %s: %w", d, err)
			}
			if err := os.Chmod(d, 0o750); err != nil {
				a.rmdirInstancia(d)
				return "", err
			}
		}
	}
	ruta := filepath.Join(d, memFull)
	_ = os.Remove(ruta)
	deshacer := func() {
		_ = os.Remove(ruta)
		if nuevo {
			a.rmdirInstancia(d)
		}
	}
	fiBase, err := os.Stat(base)
	if err != nil {
		deshacer()
		return "", err
	}
	// La cuota. En Btrfs es del subvolumen y cuenta lo referenciado (el clon
	// entero), así que se amplía ANTES de clonar: la del overlay, si está, más
	// la de la memoria. En XFS es un proyecto por fichero y se fija DESPUÉS,
	// sobre el clon. Si el almacén la impone y no se puede aplicar, la memoria
	// no va al almacén.
	cuotaMem := cuotaInstancia(fiBase.Size())
	if a.cuota == "qgroup" && a.limitar != nil {
		total := cuotaMem
		if fi, err := os.Stat(filepath.Join(d, "overlay.ext4")); err == nil {
			total += cuotaInstancia(fi.Size())
		}
		if err := a.limitar(d, ruta, total); err != nil {
			deshacer()
			return "", fmt.Errorf("applying the disk quota (%s): %w", a.cuota, err)
		}
	}
	if err := a.clonar(base, ruta); err != nil {
		deshacer()
		return "", fmt.Errorf("reflinking the memory mirror: %w", err)
	}
	if a.cuota == "prjquota" && a.limitar != nil {
		if err := a.limitar(d, ruta, cuotaMem); err != nil {
			deshacer()
			return "", fmt.Errorf("applying the disk quota (%s): %w", a.cuota, err)
		}
	}
	// El diff, tal cual, ceros incluidos (aplicarDiff). Lo escribe el daemon:
	// el fichero aún es suyo y 0400 (heredado de la base).
	if err := os.Chmod(ruta, 0o600); err != nil {
		deshacer()
		return "", err
	}
	if err := aplicarDiff(ctx, diff, ruta); err != nil {
		deshacer()
		return "", fmt.Errorf("applying the diff onto the memory mirror: %w", err)
	}
	// Legible por el grupo del VMM, de nadie más, y no suyo.
	if err := os.Chmod(ruta, 0o640); err != nil {
		deshacer()
		return "", err
	}
	if a.priv != nil && a.priv.Enabled {
		if err := os.Lchown(ruta, os.Geteuid(), a.priv.GID); err != nil {
			deshacer()
			return "", fmt.Errorf("granting %s: %w", ruta, err)
		}
	}
	return ruta, nil
}

// borrarMemoriaInstancia quita la memoria de la copia id del almacén, y su
// directorio si solo estaba para ella.
func (a *almacenCoW) borrarMemoriaInstancia(id string) {
	if nombreSeguro(id) != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.montado {
		return
	}
	d := a.dirInstancia(id)
	if err := os.Remove(filepath.Join(d, memFull)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: copy-on-write store: removing the memory of %s: %v", shortID(id), err)
	}
	if _, err := os.Lstat(filepath.Join(d, "overlay.ext4")); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(d); err == nil {
			a.rmdirInstancia(d)
		}
	}
}

// memoriaEnAlmacen deja en machines/<id>/mem.full un enlace absoluto a la
// memoria completa (base + diff) de la copia construida en el almacén, y
// devuelve esa ruta; nil error y "" si el almacén no está en uso. Cualquier
// fallo del almacén se dice y devuelve "", para que quien llama siga por el
// camino de siempre.
func (m *Manager) memoriaEnAlmacen(ctx context.Context, id, dir, base string) string {
	if m.alm == nil || m.cow.actual() != cowModoStore {
		return ""
	}
	snap := filepath.Base(filepath.Dir(base))
	ruta, err := m.alm.memoriaInstancia(ctx, snap, base, filepath.Join(dir, "mem.file"), id, m.cow.gibs())
	if err == nil {
		enlace := filepath.Join(dir, memFull)
		_ = os.Remove(enlace)
		if err = os.Symlink(ruta, enlace); err == nil {
			return enlace
		}
		m.alm.borrarMemoriaInstancia(id)
	}
	log.Printf("warning: copy-on-write store: %v; building the memory of %s in the data root instead", err, shortID(id))
	return ""
}

// borrarMemoriaAlmacen es borrarMemoriaInstancia desde el Manager (nil sin
// almacén).
func (m *Manager) borrarMemoriaAlmacen(id string) {
	if m.alm != nil {
		m.alm.borrarMemoriaInstancia(id)
	}
}
