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
	d, nuevo, err := a.dirParaInstancia(id)
	if err != nil {
		return "", err
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
	if err := a.ampliarCuotaMemoria(d, fiBase.Size()); err != nil {
		deshacer()
		return "", err
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

// dirParaInstancia devuelve el directorio de la instancia id en el almacén,
// creándolo (y asegurándolo como clonarInstancia) si su overlay no vive aquí:
// una copia de antes del almacén, o que no cupo. Con a.mu tomado.
func (a *almacenCoW) dirParaInstancia(id string) (string, bool, error) {
	d := a.dirInstancia(id)
	if _, err := os.Lstat(d); err == nil {
		return d, false, nil
	}
	if err := a.mkdirInstancia(d); err != nil {
		return "", false, err
	}
	if a.priv != nil && a.priv.Enabled {
		if err := os.Lchown(d, os.Geteuid(), a.priv.GID); err != nil {
			a.rmdirInstancia(d)
			return "", false, fmt.Errorf("securing %s: %w", d, err)
		}
		if err := os.Chmod(d, 0o750); err != nil {
			a.rmdirInstancia(d)
			return "", false, err
		}
	}
	return d, true, nil
}

// ampliarCuotaMemoria sube la cuota del directorio de la instancia para que
// quepa su memoria (el clon del espejo y el diff, que como mucho suman la RAM
// lógica) además del overlay. Solo en Btrfs, donde la cuota es del
// subvolumen y cuenta lo referenciado; en XFS es un proyecto por fichero y se
// fija sobre cada uno. Si el almacén impone cuota y no se puede aplicar, es
// un error: sin ella un VMM comprometido podría llenarlo.
func (a *almacenCoW) ampliarCuotaMemoria(d string, memBytes int64) error {
	if a.cuota != "qgroup" || a.limitar == nil {
		return nil
	}
	total := cuotaInstancia(memBytes)
	if fi, err := os.Stat(filepath.Join(d, "overlay.ext4")); err == nil {
		total += cuotaInstancia(fi.Size())
	}
	if err := a.limitar(d, "", total); err != nil {
		return fmt.Errorf("applying the disk quota (%s): %w", a.cuota, err)
	}
	return nil
}

// acumuladoDiff es dónde vive en el almacén el diff acumulado de la copia id
// (lo escrito desde el dorado, en un solo nivel): machines/<id>/mem.file es
// un enlace a él. Así aplicarlo sobre el clon del espejo al despertar es
// clonar extents dentro del mismo Btrfs o XFS, sin copiar nada.
func (a *almacenCoW) acumuladoDiff(id string) string {
	return filepath.Join(a.dirInstancia(id), "mem.file")
}

// prepararDiff deja en el almacén un mem.diff vacío, escribible por el VMM,
// para que SnapshotDiff vuelque ahí lo sucio desde el dorado; devuelve su
// ruta. La cuota se amplía antes con la memoria de la copia (memMiB), que es
// lo más que puede ocupar un diff.
func (a *almacenCoW) prepararDiff(ctx context.Context, id string, memMiB, gib int) (string, error) {
	if err := nombreSeguro(id); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.preparar(ctx, gib); err != nil {
		return "", err
	}
	// Lo que ocupa un diff no se sabe hasta volcarlo: un cuarto de la RAM es
	// más de lo medido y deja sitio para las demás.
	if _, libre, err := a.libreDentro(); err == nil && libre < int64(memMiB)<<20/4+libreMinimaAlmacen {
		return "", &errAlmacenLleno{libre: libre}
	}
	d, nuevo, err := a.dirParaInstancia(id)
	if err != nil {
		return "", err
	}
	deshacer := func() {
		if nuevo {
			a.rmdirInstancia(d)
		}
	}
	if err := a.ampliarCuotaMemoria(d, int64(memMiB)<<20); err != nil {
		deshacer()
		return "", err
	}
	ruta := filepath.Join(d, memDiff)
	_ = os.Remove(ruta)
	f, err := os.OpenFile(ruta, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o660)
	if err != nil {
		deshacer()
		return "", err
	}
	f.Close()
	if a.cuota == "prjquota" && a.limitar != nil {
		if err := a.limitar(d, ruta, cuotaInstancia(int64(memMiB)<<20)); err != nil {
			_ = os.Remove(ruta)
			deshacer()
			return "", fmt.Errorf("applying the disk quota (%s): %w", a.cuota, err)
		}
	}
	if a.priv != nil && a.priv.Enabled {
		// Como el overlay: del daemon, y el VMM lo escribe por grupo.
		if err := cederPorGrupo(ruta, os.Geteuid(), a.priv.GID); err != nil {
			_ = os.Remove(ruta)
			deshacer()
			return "", err
		}
	}
	return ruta, nil
}

// borrarDiffParcial quita el mem.diff de la copia id del almacén (un volcado
// que falló a medias).
func (a *almacenCoW) borrarDiffParcial(id string) {
	if nombreSeguro(id) != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.montado {
		_ = os.Remove(filepath.Join(a.dirInstancia(id), memDiff))
	}
}

// diffEnAlmacen dice dónde debe volcar el VMM el diferencial de la copia id:
// en el almacén, si está en uso y el overlay de la copia vive en él (en el
// jail, solo ese directorio está montado), o "" para el camino de siempre
// (el directorio de la máquina). Un diff que YA se acumula fuera del almacén
// (machines/<id>/mem.file regular, de antes de esto) sigue fuera: no se
// mezclan los dos sitios.
func (m *Manager) diffEnAlmacen(ctx context.Context, id string, memMiB int) string {
	if m.alm == nil || m.cow.actual() != cowModoStore || !m.alm.contiene(id) {
		return ""
	}
	if fi, err := os.Lstat(filepath.Join(m.dir(id), "mem.file")); err == nil && fi.Mode().IsRegular() {
		return ""
	}
	ruta, err := m.alm.prepararDiff(ctx, id, memMiB, m.cow.gibs())
	if err != nil {
		log.Printf("warning: copy-on-write store: %v; %s dumps its diff to its own directory instead", err, shortID(id))
		return ""
	}
	return ruta
}

// borrarDiffParcialAlmacen es borrarDiffParcial desde el Manager.
func (m *Manager) borrarDiffParcialAlmacen(id string) {
	if m.alm != nil {
		m.alm.borrarDiffParcial(id)
	}
}
