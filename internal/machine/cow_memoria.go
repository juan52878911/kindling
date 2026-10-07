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
	"time"

	"github.com/juan52878911/kindling/pkg/durable"
)

// sufijoBaseMemoria distingue en bases/<snap> el espejo de la memoria del del
// overlay (.ext4): base() y barrer solo tocan los de su sufijo.
const sufijoBaseMemoria = ".mem"

// tiemposMemoria es lo que costó preparar la memoria de un despertar en el
// almacén, para el desglose del evento de thaw (notaFases): montar el almacén
// y, si aún no estaba, copiar el espejo del dorado (o esperar al que se copia
// tras el commit, espejarMemoria).
type tiemposMemoria struct {
	almacen, espejo time.Duration
}

// baseMemoria devuelve el espejo del mem.file src del dorado snap dentro del
// almacén, creándolo si aún no está. Con a.mu tomado, que SUELTA mientras
// copia: son segundos con un dorado de GiB (10 s medidos con Postgres), y con
// el candado tomado todo lo demás del almacén —un run -from, otro thaw— se
// quedaba esperando. Si otro ya lo está copiando (el espejo de después del
// commit, o un thaw a la vez), se espera a ese en vez de copiar dos veces.
// Mismo contrato que base(): a un temporal y renombrado, para que un espejo a
// medias no pueda quedar con su nombre definitivo. t, si no es nil, suma lo
// que se tardó.
func (a *almacenCoW) baseMemoria(ctx context.Context, snap, src string, t *tiemposMemoria) (string, error) {
	if t != nil {
		t0 := time.Now()
		defer func() { t.espejo += time.Since(t0) }()
	}
	if err := nombreSeguro(snap); err != nil {
		return "", err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	dir := a.dirBases(snap)
	ruta := filepath.Join(dir, claveBase(fi)+sufijoBaseMemoria)
	for {
		if _, err := os.Lstat(ruta); err == nil {
			return ruta, nil
		}
		hecho := a.espejando[ruta]
		if hecho == nil {
			break
		}
		a.mu.Unlock()
		select {
		case <-hecho:
			a.mu.Lock()
		case <-ctx.Done():
			a.mu.Lock()
			return "", ctx.Err()
		}
		// Si aquel falló, la vuelta siguiente no ve ni espejo ni copia en
		// curso, y copia este.
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
	hecho := make(chan struct{})
	if a.espejando == nil {
		a.espejando = map[string]chan struct{}{}
	}
	a.espejando[ruta] = hecho
	a.mu.Unlock()
	err = a.copiar(ctx, src, tmp)
	a.mu.Lock()
	delete(a.espejando, ruta)
	close(hecho)
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("mirroring the memory of %s into the store: %w", snap, err)
	}
	_ = os.Chmod(tmp, 0o400)
	if err := durable.Renombrar(tmp, ruta); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	// Los espejos de versiones anteriores de este dorado sobran; los .ext4
	// son del overlay y no se tocan, ni el temporal de otro que aún copia.
	if entradas, err := os.ReadDir(dir); err == nil {
		for _, e := range entradas {
			if e.Name() != filepath.Base(ruta) && strings.HasSuffix(e.Name(), sufijoBaseMemoria) &&
				!a.espejoEnCurso(dir, e.Name()) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return ruta, nil
}

// espejoEnCurso dice si nombre, dentro de dir (bases/<snap>), es el temporal
// de un espejo que alguien está copiando ahora mismo sin a.mu: ni barrer ni
// la limpieza de versiones viejas deben borrárselo. Con a.mu tomado.
func (a *almacenCoW) espejoEnCurso(dir, nombre string) bool {
	if !strings.HasPrefix(nombre, ".tmp-") {
		return false
	}
	return a.espejando[filepath.Join(dir, strings.TrimPrefix(nombre, ".tmp-"))] != nil
}

// espejarMemoria deja hecho el espejo del mem.file src del dorado snap, sin
// que nadie lo pida todavía: lo llama el commit en segundo plano
// (Manager.espejarMemoriaDorado), para que el primer thaw de una copia
// congelada en diferencial no pague la copia entera (hecho dice si espejó).
// Quien llama ya sabe que el almacén está en uso (el modo store): si aún no
// existe, como en un daemon recién instalado cuyo primer dorado se acaba de
// guardar, se crea aquí con el tamaño de daemon.cow_store_gib (gib), igual
// que lo haría el primer run -from de ese dorado. Antes se esperaba a ese run
// -from, y el primer dorado de cada instalación se quedaba sin espejo: su
// primer thaw en diferencial pagaba la copia entera.
func (a *almacenCoW) espejarMemoria(ctx context.Context, snap, src string, gib int) (hecho bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.preparar(ctx, gib); err != nil {
		return false, err
	}
	// Por adelantado solo si sobra sitio: un dorado del que nunca se congela
	// ninguna copia en diferencial no necesita espejo, y con varios dorados
	// los espejos le quitarían el almacén a las instancias. Si después del
	// espejo quedaría menos de la mitad libre, se deja al primer thaw que lo
	// necesite (el camino perezoso de memoriaInstancia).
	if total, libre, err := a.libreDentro(); err == nil && total > 0 && libre-allocatedBytes(src) < total/2 {
		return false, nil
	}
	if _, err := a.baseMemoria(ctx, snap, src, nil); err != nil {
		return false, err
	}
	return true, nil
}

// memoriaInstancia deja en el almacén la memoria completa de la copia id:
// el espejo del mem.file src del dorado snap clonado y con el diferencial
// diff escrito encima. Devuelve su ruta, ya legible por el VMM.
func (a *almacenCoW) memoriaInstancia(ctx context.Context, snap, src, diff, id string, gib int, t *tiemposMemoria) (string, error) {
	if err := nombreSeguro(id); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	t0 := time.Now()
	err := a.preparar(ctx, gib)
	if t != nil {
		t.almacen += time.Since(t0)
	}
	if err != nil {
		return "", err
	}
	// El diff se escribe entero sobre el clon: son los únicos bloques que el
	// almacén asigna para esta copia, más lo que debe quedar para las demás.
	if _, libre, err := a.libreDentro(); err == nil && libre < allocatedBytes(diff)+libreMinimaAlmacen {
		return "", &errAlmacenLleno{libre: libre}
	}
	base, err := a.baseMemoria(ctx, snap, src, t)
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
// camino de siempre. t, si no es nil, suma lo que costaron el almacén y el
// espejo.
func (m *Manager) memoriaEnAlmacen(ctx context.Context, id, dir, base, diff string, t *tiemposMemoria) string {
	if m.alm == nil || m.cow.actual() != cowModoStore {
		return ""
	}
	snap := filepath.Base(filepath.Dir(base))
	ruta, err := m.alm.memoriaInstancia(ctx, snap, base, diff, id, m.cow.gibs(), t)
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

// espejarMemoriaDorado copia en segundo plano, tras el commit del dorado name,
// el espejo de su memoria en el almacén (espejarMemoria). Sin esto lo copiaba
// el PRIMER thaw de una copia congelada en diferencial, con quien la despierta
// esperando: 10 s medidos con un dorado de Postgres. Solo si las copias se
// congelan en diferencial y el almacén está en uso; si algo falla, el thaw lo
// sigue haciendo como antes. Si el almacén aún no existe, lo crea (ver
// espejarMemoria); si no puede, degrada a copy como lo haría el primer run
// -from. Se cancela al cerrar el Manager: lo que quede a
// medias es un temporal que barrer recoge al arrancar.
func (m *Manager) espejarMemoriaDorado(name string) {
	if m.alm == nil || m.cow.actual() != cowModoStore || !congelarEnDiff() || isClosed(m.quit) {
		return
	}
	src := filepath.Join(m.snapDir(name), "mem.file")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-m.quit:
		case <-ctx.Done():
		}
		cancel()
	}()
	go func() {
		defer cancel()
		t0 := time.Now()
		hecho, err := m.alm.espejarMemoria(ctx, name, src, m.cow.gibs())
		if err != nil {
			switch {
			case ctx.Err() != nil:
			case errors.Is(err, errAlmacenNoDisponible):
				// El almacén no se pudo crear o montar, y no se reintenta
				// hasta reiniciar: se dice ya (kling info), no en el
				// primer run -from.
				m.cow.degradarSinAlmacen(err)
			default:
				log.Printf("warning: copy-on-write store: mirroring the memory of %s: %v; the first thaw of a copy will do it", name, err)
			}
			return
		}
		if !hecho {
			return
		}
		log.Printf("copy-on-write store: memory of %s mirrored in %d ms", name, time.Since(t0).Milliseconds())
	}()
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
// quepa su memoria además del overlay. Solo en Btrfs, donde la cuota es del
// subvolumen y cuenta lo referenciado; en XFS es un proyecto por fichero y se
// fija sobre cada uno. Si el almacén impone cuota y no se puede aplicar, es
// un error: sin ella un VMM comprometido podría llenarlo.
func (a *almacenCoW) ampliarCuotaMemoria(d string, memBytes int64) error {
	if a.cuota != "qgroup" || a.limitar == nil {
		return nil
	}
	// Dos veces la memoria: al congelar conviven el mem.full (el clon del
	// espejo con el diff anterior encima, que Btrfs cuenta entero), el diff
	// acumulado y el mem.diff nuevo. Con una sola, una copia que escribe mucha
	// memoria daba EDQUOT al congelar y se comía la cuota de su overlay.
	total := 2 * cuotaInstancia(memBytes)
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

// borrarAcumuladoDiff retira el diff acumulado de la copia id: el fichero del
// almacén (si vive ahí) y el enlace o fichero de machines/<id>/mem.file. Para
// cuando ese diff ya no vale: la copia va a volcar entero (un commit, un diff
// que falló, el diff apagado, la base desaparecida) y lo que quedara sería un
// huérfano bajo su cuota que barrer no recoge mientras la máquina viva. Y sin
// quitar el enlace antes de un volcado completo, Firecracker escribiría la
// RAM entera DENTRO del almacén a través de él.
func (m *Manager) borrarAcumuladoDiff(id, dir string) {
	mem := filepath.Join(dir, "mem.file")
	if fi, err := os.Lstat(mem); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(mem)
	}
	if m.alm != nil && nombreSeguro(id) == nil {
		m.alm.mu.Lock()
		if m.alm.montado {
			if err := os.Remove(m.alm.acumuladoDiff(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("warning: copy-on-write store: removing the diff of %s: %v", shortID(id), err)
			}
		}
		m.alm.mu.Unlock()
	}
}
