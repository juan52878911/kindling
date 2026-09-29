package machine

// Copias de disco baratas al crear una instancia desde un dorado (daemon.cow).
//
// runFrom daba a cada instancia su overlay copiando entero el del dorado con
// `cp --sparse=always`. Medido en el laboratorio (ext4): 76 ms por 200 MiB, y
// ~6,7 GiB escritos para 32 copias, con la latencia creciendo ronda a ronda
// porque el disco se va llenando de copias idénticas. La memoria ya no cuesta:
// el mem.file se mapea y se comparte. El disco era lo único que escalaba con
// el tamaño del dorado.
//
// Tres modos (docs/cow.md tiene el análisis entero y las opciones descartadas):
//
//   - reflink: la raíz de kindling está en un sistema de ficheros con reflink
//     (XFS con reflink=1, Btrfs). FICLONE da a la instancia un fichero propio
//     que comparte los bloques del dorado hasta que alguno escribe. Coste
//     constante, sin nada que montar.
//   - store: la raíz no tiene reflink (ext4). kindling monta por loop un
//     fichero XFS propio ($root/cow.xfs en $root/cow) y guarda ahí, una vez por
//     dorado, una copia "base" de su overlay; cada instancia es un FICLONE de
//     esa base. El overlay de la instancia vive en $root/cow/m/<id>/ y
//     machines/<id>/overlay.ext4 es un enlace simbólico a él: el resto del
//     daemon sigue abriendo la ruta de siempre.
//   - copy: lo de antes.
//
// Lo que ya existe no se migra: las instancias viejas siguen con su copia en
// machines/<id>, y las del almacén siguen en él aunque luego se ponga
// daemon.cow=off (el almacén se monta al arrancar si existe, sea cual sea el
// modo).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/durable"
)

// Lo que se configura (daemon.cow). Mismos valores que pkg/config.
const (
	CoWAuto  = "auto"
	CoWStore = "reflink-store"
	CoWOff   = "off"
)

// Lo que se hace de verdad (api.CoWInfo.Mode).
const (
	cowModoReflink = "reflink"
	cowModoStore   = "store"
	cowModoClone   = "clonefile"
	cowModoCopy    = "copy"
)

// CoWConfig es la configuración de copias de disco que el daemon lee al
// arrancar (daemon.cow y daemon.cow_store_gib).
type CoWConfig struct {
	Mode     string
	StoreGiB int
}

// decidirCoW elige el modo a partir de lo pedido, de si la raíz clona con
// FICLONE (probado de verdad, no deducido del tipo de sistema de ficheros) y
// de si el almacén propio se puede usar (errAlmacen nil). Es pura para poder
// probar la tabla entera sin root ni XFS.
func decidirCoW(pedido string, nativo bool, errAlmacen error) (modo, motivo string) {
	switch pedido {
	case CoWOff:
		return cowModoCopy, "daemon.cow is off: every instance copies its golden overlay"
	case CoWStore:
		if errAlmacen == nil {
			return cowModoStore, "daemon.cow=reflink-store: overlays are reflinked inside kindling's XFS store"
		}
		return cowModoCopy, fmt.Sprintf("daemon.cow=reflink-store, but the store is unavailable (%v): copying overlays", errAlmacen)
	case "", CoWAuto:
		if nativo {
			return cowModoReflink, "the data root supports reflink (FICLONE): overlays share blocks with their golden"
		}
		if errAlmacen == nil {
			return cowModoStore, "no reflink on the data root: overlays are reflinked inside kindling's XFS store"
		}
		return cowModoCopy, fmt.Sprintf("no reflink on the data root and no XFS store (%v): copying overlays", errAlmacen)
	}
	return cowModoCopy, fmt.Sprintf("unknown daemon.cow %q: copying overlays", pedido)
}

// estadoCoW es el modo en uso y sus contadores. Sin configurar (tests, o un
// daemon que no llama a SetCoW) es la copia de siempre.
type estadoCoW struct {
	mu     sync.Mutex
	pedido string
	modo   string
	motivo string
	gib    int
	clones map[string]int64
}

func (e *estadoCoW) actual() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.modo == "" {
		return cowModoCopy
	}
	return e.modo
}

func (e *estadoCoW) contar(modo string) {
	e.mu.Lock()
	if e.clones == nil {
		e.clones = make(map[string]int64)
	}
	e.clones[modo]++
	e.mu.Unlock()
}

// degradar pasa a copia completa hasta reiniciar el daemon: el almacén no se
// pudo preparar y reintentar mkfs en cada run -from solo añadiría latencia.
func (e *estadoCoW) degradar(motivo string) {
	e.mu.Lock()
	cambia := e.modo != cowModoCopy
	e.modo, e.motivo = cowModoCopy, motivo
	e.mu.Unlock()
	if cambia {
		log.Printf("warning: %s", motivo)
	}
}

// SetCoW fija el modo de copias de disco. Se llama una vez, al arrancar el
// daemon y antes de servir; la detección prueba FICLONE de verdad.
func (m *Manager) SetCoW(cfg CoWConfig) {
	modo, motivo := m.detectarCoW(cfg)
	m.cow.mu.Lock()
	m.cow.pedido, m.cow.modo, m.cow.motivo, m.cow.gib = cfg.Mode, modo, motivo, cfg.StoreGiB
	if m.cow.pedido == "" {
		m.cow.pedido = CoWAuto
	}
	m.cow.mu.Unlock()
	if modo == cowModoCopy && cfg.Mode != CoWOff {
		log.Printf("warning: copy-on-write disks: %s", motivo)
	} else {
		log.Printf("copy-on-write disks: %s (%s)", modo, motivo)
	}
}

// CoWInfo es lo que GET /info cuenta del modo de copias.
func (m *Manager) CoWInfo() *api.CoWInfo {
	m.cow.mu.Lock()
	info := &api.CoWInfo{Setting: m.cow.pedido, Mode: m.cow.modo, Reason: m.cow.motivo}
	if info.Setting == "" {
		info.Setting = CoWOff // sin SetCoW: la copia de siempre
	}
	if info.Mode == "" {
		info.Mode, info.Reason = cowModoCopy, "not configured"
	}
	if len(m.cow.clones) > 0 {
		info.Clones = make(map[string]int64, len(m.cow.clones))
		for k, v := range m.cow.clones {
			info.Clones[k] = v
		}
	}
	m.cow.mu.Unlock()
	if m.alm != nil {
		info.Store = m.alm.info()
	}
	return info
}

// copiarOverlay copia un disco entero (plantilla, commit): con reflink nativo
// primero se intenta FICLONE, que no cuesta nada, y si falla la copia de
// siempre. Fuera del modo reflink es copiarDisco sin más.
func (m *Manager) copiarOverlay(ctx context.Context, src, dst string) ([]byte, error) {
	if m.cow.actual() == cowModoReflink {
		if err := clonarFichero(src, dst); err == nil {
			return nil, nil
		}
		_ = os.Remove(dst)
	}
	return copiarDisco(ctx, src, dst)
}

// clonarOverlayInstancia deja en dst el overlay de la instancia id, creada
// desde el snapshot snap cuyo overlay es src, y dice cómo lo hizo. Cualquier
// fallo del camino rápido acaba en la copia de siempre: una optimización no
// deja a nadie sin arrancar.
func (m *Manager) clonarOverlayInstancia(ctx context.Context, snap, src, id, dst string) (string, error) {
	modo := m.cow.actual()
	switch modo {
	case cowModoReflink:
		if err := clonarFichero(src, dst); err == nil {
			m.cow.contar(cowModoReflink)
			return cowModoReflink, nil
		} else {
			log.Printf("warning: reflink of %s failed (%v): copying it", filepath.Base(src), err)
		}
		_ = os.Remove(dst)
	case cowModoStore:
		ruta, err := m.alm.clonarInstancia(ctx, snap, src, id, m.cow.gibs())
		if err == nil {
			// Enlace ABSOLUTO: dentro del jail la misma ruta resuelve al bind
			// del directorio de la instancia (ver prepararBindsJail).
			if err = os.Symlink(ruta, dst); err == nil {
				m.cow.contar(cowModoStore)
				return cowModoStore, nil
			}
			m.alm.borrarInstancia(id)
		}
		if errors.Is(err, errAlmacenNoDisponible) {
			m.cow.degradar(fmt.Sprintf("copy-on-write store unavailable (%v): copying overlays until the daemon restarts", err))
		} else {
			log.Printf("warning: copy-on-write store: %v; copying the overlay of %s instead", err, shortID(id))
		}
	}
	if out, err := copiarDisco(ctx, src, dst); err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	hecho := cowModoCopy
	if modo == cowModoClone {
		hecho = cowModoClone // macOS: cp -c es clonefile en APFS
	}
	m.cow.contar(hecho)
	return hecho, nil
}

func (e *estadoCoW) gibs() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gib
}

// overlayParaLeer es de dónde lee el DAEMON el overlay de id (commit): el
// fichero del almacén, por su ruta derivada del id, si la instancia lo tiene
// ahí; si no, machines/<id>/overlay.ext4. Así el daemon no sigue el enlace de
// machines/<id>, que está en un directorio del VMM.
func (m *Manager) overlayParaLeer(id string) string {
	if m.alm != nil && nombreSeguro(id) == nil {
		ruta := filepath.Join(m.alm.dirInstancia(id), "overlay.ext4")
		if fi, err := os.Lstat(ruta); err == nil && fi.Mode().IsRegular() {
			return ruta
		}
	}
	return filepath.Join(m.dir(id), "overlay.ext4")
}

// rutaCanonica devuelve p absoluta y sin enlaces simbólicos, como las rutas de
// /proc/self/mountinfo. Si p no existe todavía resuelve su ancestro más
// cercano; si nada se puede resolver devuelve p tal cual.
func rutaCanonica(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	rest, cur := "", abs
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		up := filepath.Dir(cur)
		if up == cur {
			return abs
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = up
	}
}

// fijarOverlayParaLeer abre ruta con O_NOFOLLOW y comprueba con Fstat, sobre el
// descriptor ya abierto y no por ruta, que es un fichero regular. El VMM puede
// haber cambiado el overlay por un enlace simbólico a un fichero de root entre
// que se eligió la ruta y que se copia: un Lstat previo no lo evita. Devuelve
// una función que hay que llamar tras la copia: comprueba que la ruta sigue
// siendo el mismo fichero, y si no, la copia se descarta.
func fijarOverlayParaLeer(ruta string) (func() error, error) {
	f, err := os.OpenFile(ruta, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("opening the overlay %s: %w", ruta, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("the overlay %s is not a regular file", ruta)
	}
	return func() error {
		ahora, err := os.Lstat(ruta)
		if err != nil || !ahora.Mode().IsRegular() || !os.SameFile(fi, ahora) {
			return fmt.Errorf("the overlay %s changed while it was being copied", ruta)
		}
		return nil
	}, nil
}

// borrarOverlayAlmacen quita del almacén el overlay de una máquina que se
// elimina. Lo que no se borre aquí lo recoge barrerAlmacen.
func (m *Manager) borrarOverlayAlmacen(id string) {
	if m.alm != nil {
		m.alm.borrarInstancia(id)
	}
}

// barrerAlmacen quita del almacén lo que ya no es de nadie: overlays de
// máquinas que ya no tienen directorio (un runFrom que falló, una máquina
// barrida como huérfana) y bases de dorados que ya no existen o que se
// reemplazaron. Sin m.mu: lo toma solo para decidir.
func (m *Manager) barrerAlmacen() {
	if m.alm == nil {
		return
	}
	viva := func(id string) bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		if _, ok := m.byID[id]; ok || m.reserved[id] > 0 {
			return true
		}
		_, err := os.Lstat(m.dir(id))
		return err == nil || !os.IsNotExist(err)
	}
	overlayDorado := func(snap string) string {
		return filepath.Join(m.snapDir(snap), "overlay.ext4")
	}
	m.alm.barrer(viva, overlayDorado)
}

// ── el almacén ────────────────────────────────────────────────────────────────

// errAlmacenNoDisponible envuelve los fallos de preparar el almacén (crear,
// formatear, montar, probar): con ellos no tiene sentido reintentar en cada
// instancia.
var errAlmacenNoDisponible = errors.New("store unavailable")

// libreMinimaAlmacen es lo que tiene que quedar libre en el almacén para
// clonar en él: un reflink no ocupa nada, pero la instancia va a escribir.
// Por debajo, la instancia va a una copia completa en la raíz, como antes.
const libreMinimaAlmacen = 256 << 20

// almacenCoW es el almacén propio: un XFS con reflink en un fichero, montado
// por loop dentro de la raíz de kindling. Todo lo que toca el sistema (crear
// el fichero, formatear, montar, clonar) va por funciones que pone la
// plataforma, para poder probar la lógica sin root.
type almacenCoW struct {
	mu   sync.Mutex
	root string // $root
	img  string // $root/cow.xfs
	dir  string // $root/cow (punto de montaje)
	priv *Privileges
	// viva dice si la máquina id existe (o se está creando): un directorio de
	// instancia de una máquina que no está viva es un residuo y se reemplaza.
	viva func(id string) bool

	montado  bool
	errFatal error // la preparación falló: no se reintenta hasta reiniciar

	// Operaciones del sistema, de la plataforma (cow_fc.go) o de un test.
	estaMontado func(dir string) (bool, error)
	crear       func(ctx context.Context, img string, bytes int64) error
	montar      func(ctx context.Context, img, dir string) error
	clonar      func(src, dst string) error
	copiar      func(ctx context.Context, src, dst string) error
	libreEn     func(dir string) (total, libre int64, err error)
}

func (a *almacenCoW) dirInstancia(id string) string { return filepath.Join(a.dir, "m", id) }
func (a *almacenCoW) dirBases(snap string) string   { return filepath.Join(a.dir, "bases", snap) }

// nombreSeguro rechaza lo que no es un solo componente de ruta: los ids y los
// nombres de snapshot se validan antes, pero aquí se construyen rutas bajo
// las que luego se borra.
func nombreSeguro(n string) error {
	if n == "" || n == "." || n == ".." || strings.HasPrefix(n, ".") || strings.ContainsAny(n, "/\\\x00") {
		return fmt.Errorf("invalid name %q for the copy-on-write store", n)
	}
	return nil
}

// existe dice si el almacén está creado (el fichero de imagen existe).
func (a *almacenCoW) existe() bool {
	_, err := os.Lstat(a.img)
	return err == nil
}

// montarSiExiste monta al arrancar un almacén ya creado, sea cual sea el modo:
// sus instancias lo necesitan para arrancar o descongelarse. Si no se puede,
// se dice; esas instancias fallarán al abrir su overlay, con la ruta en el
// error.
func (a *almacenCoW) montarSiExiste(ctx context.Context) {
	if !a.existe() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.asegurarMontado(ctx); err != nil {
		log.Printf("WARNING: copy-on-write store %s: %v; instances whose overlay lives there won't start until it is mounted", a.img, err)
	}
}

// asegurarMontado monta el almacén si existe y no lo está. Con a.mu tomado.
func (a *almacenCoW) asegurarMontado(ctx context.Context) error {
	if a.montado {
		return nil
	}
	ya, err := a.estaMontado(a.dir)
	if err != nil {
		return err
	}
	if !ya {
		if err := os.MkdirAll(a.dir, 0o750); err != nil {
			return err
		}
		if err := a.montar(ctx, a.img, a.dir); err != nil {
			return err
		}
	}
	if err := a.disponer(); err != nil {
		return err
	}
	a.montado = true
	return nil
}

// disponer deja los directorios del almacén montado con sus permisos: la
// raíz y m/ atravesables por el grupo del VMM (como machines/), bases/ solo
// del daemon. Y prueba que clona de verdad.
func (a *almacenCoW) disponer() error {
	modo, gid := os.FileMode(0o700), 0
	if a.priv != nil && a.priv.Enabled {
		modo, gid = 0o750, a.priv.GID
	}
	for _, d := range []struct {
		ruta string
		modo os.FileMode
		gid  int
	}{{a.dir, modo, gid}, {filepath.Join(a.dir, "m"), modo, gid}, {filepath.Join(a.dir, "bases"), 0o700, 0}} {
		if err := os.MkdirAll(d.ruta, d.modo); err != nil {
			return err
		}
		fi, err := os.Lstat(d.ruta)
		if err != nil || !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", d.ruta)
		}
		if os.Geteuid() == 0 {
			_ = os.Lchown(d.ruta, 0, d.gid)
		}
		_ = os.Chmod(d.ruta, d.modo)
	}
	return a.probar()
}

// probar clona un fichero pequeño dentro del almacén. Un XFS formateado sin
// reflink (xfsprogs muy viejo) se montaría igual y aquí se ve.
func (a *almacenCoW) probar() error {
	dir := filepath.Join(a.dir, "bases")
	src := filepath.Join(dir, fmt.Sprintf(".probe-%d", time.Now().UnixNano()))
	dst := src + "-clone"
	defer os.Remove(src)
	defer os.Remove(dst)
	if err := os.WriteFile(src, make([]byte, 4096), 0o600); err != nil {
		return err
	}
	if err := a.clonar(src, dst); err != nil {
		return fmt.Errorf("the store does not reflink: %w", err)
	}
	return nil
}

// preparar crea (si hace falta) y monta el almacén. Con a.mu tomado.
func (a *almacenCoW) preparar(ctx context.Context, gib int) error {
	if a.montado {
		return nil
	}
	if a.errFatal != nil {
		return a.errFatal
	}
	err := func() error {
		if !a.existe() {
			_, libre, err := a.libreEn(a.root)
			if err != nil {
				return err
			}
			bytes, err := tamAlmacen(libre, gib)
			if err != nil {
				return err
			}
			log.Printf("creating the copy-on-write store %s (%d MiB, reserved up front)", a.img, bytes>>20)
			if err := a.crear(ctx, a.img, bytes); err != nil {
				return err
			}
		}
		return a.asegurarMontado(ctx)
	}()
	if err != nil {
		a.errFatal = fmt.Errorf("%w: %v", errAlmacenNoDisponible, err)
		return a.errFatal
	}
	return nil
}

// tamAlmacen es cuánto reservar para el almacén: lo pedido, o una cuarta
// parte del disco libre con un máximo de 16 GiB. Se reserva entero
// (fallocate): un XFS sobre un fichero disperso que se queda sin sitio debajo
// recibe errores de E/S y se apaga, con todas sus instancias dentro. Mejor
// una cuota fija que un almacén que puede romperse por algo que hace otro.
func tamAlmacen(libre int64, gib int) (int64, error) {
	const margen = 2 << 30 // lo que se deja libre en la raíz, como minFreeDiskMiB
	if gib > 0 {
		b := int64(gib) << 30
		if b > libre-margen {
			return 0, fmt.Errorf("daemon.cow_store_gib=%d doesn't fit: %d MiB free under the data root", gib, libre>>20)
		}
		return b, nil
	}
	b := min(libre/4, 16<<30)
	b &^= (1 << 20) - 1 // MiB enteros
	if b < 1<<30 {
		return 0, fmt.Errorf("only %d MiB free under the data root: not enough for a store (needs 4 GiB free)", libre>>20)
	}
	return b, nil
}

// claveBase identifica una versión concreta del overlay de un dorado: si el
// dorado se reemplaza (commit -replace) es otro fichero, otro inodo, y la base
// vieja deja de valer.
func claveBase(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Sprintf("%x-%x", fi.Size(), fi.ModTime().UnixNano())
	}
	return fmt.Sprintf("%x-%x-%x-%x", uint64(st.Dev), uint64(st.Ino), fi.Size(), fi.ModTime().UnixNano())
}

// base devuelve la copia del overlay del dorado dentro del almacén, creándola
// la primera vez: es la única copia completa que se paga por dorado. Se
// escribe a un temporal y se renombra: una base a medias no
// puede quedar con su nombre definitivo, porque de ella se clonarían
// instancias corruptas para siempre (durable.Renombrar sincroniza antes de
// renombrar). Con a.mu tomado.
func (a *almacenCoW) base(ctx context.Context, snap, src string) (string, error) {
	if err := nombreSeguro(snap); err != nil {
		return "", err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	dir := a.dirBases(snap)
	ruta := filepath.Join(dir, claveBase(fi)+".ext4")
	if _, err := os.Lstat(ruta); err == nil {
		return ruta, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".tmp-"+filepath.Base(ruta))
	_ = os.Remove(tmp)
	if err := a.copiar(ctx, src, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("copying %s into the store: %w", snap, err)
	}
	// Solo lectura: nadie escribe en una base, se clona.
	_ = os.Chmod(tmp, 0o400)
	if err := durable.Renombrar(tmp, ruta); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	// Las bases de versiones anteriores de este dorado sobran. Sus bloques
	// siguen vivos en las instancias que se clonaron de ellas: XFS cuenta las
	// referencias.
	if entradas, err := os.ReadDir(dir); err == nil {
		for _, e := range entradas {
			if e.Name() != filepath.Base(ruta) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return ruta, nil
}

// clonarInstancia prepara el almacén si hace falta, asegura la base del
// dorado y la clona para la instancia id. Devuelve la ruta del overlay de la
// instancia, ya cedido al usuario del VMM.
func (a *almacenCoW) clonarInstancia(ctx context.Context, snap, src, id string, gib int) (string, error) {
	if err := nombreSeguro(id); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.preparar(ctx, gib); err != nil {
		return "", err
	}
	if _, libre, err := a.libreEn(a.dir); err == nil && libre < libreMinimaAlmacen {
		return "", fmt.Errorf("the store is almost full (%d MiB free)", libre>>20)
	}
	base, err := a.base(ctx, snap, src)
	if err != nil {
		return "", err
	}
	d := a.dirInstancia(id)
	if err := os.Mkdir(d, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) || a.viva == nil || a.viva(id) {
			return "", err
		}
		// Un directorio residual (un runFrom que murió, un barrido que no llegó):
		// la máquina no está viva, así que sobra y no debe mandar a la instancia
		// a copia completa.
		if err := os.RemoveAll(d); err != nil {
			return "", err
		}
		if err := os.Mkdir(d, 0o700); err != nil {
			return "", err
		}
	}
	ruta := filepath.Join(d, "overlay.ext4")
	if err := a.clonar(base, ruta); err != nil {
		_ = os.RemoveAll(d)
		return "", fmt.Errorf("reflinking the overlay: %w", err)
	}
	// La base es 0400 y el clon hereda el modo: se abre para el VMM.
	_ = os.Chmod(ruta, 0o600)
	if a.priv != nil && a.priv.Enabled {
		// El VMM es dueño del FICHERO, no del directorio: con el directorio en
		// root:grupo 0750 solo lo atraviesa. Dueño del directorio podría crear
		// ficheros en él (llenar el almacén compartido) y cambiar el overlay por
		// un enlace entre la comprobación del daemon y su lectura.
		if err := a.priv.Own(ruta); err != nil {
			_ = os.RemoveAll(d)
			return "", err
		}
		if err := os.Lchown(d, 0, a.priv.GID); err != nil {
			_ = os.RemoveAll(d)
			return "", fmt.Errorf("securing %s: %w", d, err)
		}
		if err := os.Chmod(d, 0o750); err != nil {
			_ = os.RemoveAll(d)
			return "", err
		}
	}
	return ruta, nil
}

// borrarInstancia quita el directorio de la instancia id del almacén. La ruta
// sale del id, nunca del enlace de machines/<id>, que es del VMM y podría
// apuntar a cualquier sitio.
func (a *almacenCoW) borrarInstancia(id string) {
	if nombreSeguro(id) != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.montado {
		return // nunca se borra en el directorio de montaje vacío de la raíz
	}
	_ = os.RemoveAll(a.dirInstancia(id))
}

// barrer quita las instancias sin máquina y las bases sin dorado (o de una
// versión anterior del dorado).
func (a *almacenCoW) barrer(viva func(id string) bool, overlayDorado func(snap string) string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.montado {
		return
	}
	if entradas, err := os.ReadDir(filepath.Join(a.dir, "m")); err == nil {
		for _, e := range entradas {
			if nombreSeguro(e.Name()) != nil || viva(e.Name()) {
				continue
			}
			log.Printf("copy-on-write store: removing the overlay of %s (its machine is gone)", shortID(e.Name()))
			_ = os.RemoveAll(filepath.Join(a.dir, "m", e.Name()))
		}
	}
	entradas, err := os.ReadDir(filepath.Join(a.dir, "bases"))
	if err != nil {
		return
	}
	for _, e := range entradas {
		dir := filepath.Join(a.dir, "bases", e.Name())
		if nombreSeguro(e.Name()) != nil {
			if strings.HasPrefix(e.Name(), ".probe-") {
				_ = os.Remove(dir)
			}
			continue
		}
		fi, err := os.Stat(overlayDorado(e.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				_ = os.RemoveAll(dir)
			}
			continue
		}
		vigente := claveBase(fi) + ".ext4"
		hijos, _ := os.ReadDir(dir)
		for _, h := range hijos {
			if h.Name() != vigente {
				_ = os.Remove(filepath.Join(dir, h.Name()))
			}
		}
	}
}

// info es el estado del almacén para GET /info; nil si no existe.
func (a *almacenCoW) info() *api.CoWStore {
	if !a.existe() {
		return nil
	}
	a.mu.Lock()
	montado := a.montado
	a.mu.Unlock()
	s := &api.CoWStore{Path: a.dir, Mounted: montado}
	if montado {
		if total, libre, err := a.libreEn(a.dir); err == nil {
			s.SizeMiB, s.FreeMiB = total>>20, libre>>20
		}
	}
	return s
}

// ── montajes ──────────────────────────────────────────────────────────────────

// montaje es una línea de /proc/self/mountinfo.
type montaje struct {
	punto  string
	fstype string
}

// parsearMountinfo lee /proc/self/mountinfo. El punto de montaje es el campo 5
// y el tipo el primero tras el separador " - "; los espacios de las rutas van
// escapados en octal (\040).
func parsearMountinfo(r io.Reader) ([]montaje, error) {
	var out []montaje
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		campos := strings.Fields(sc.Text())
		sep := -1
		for i, c := range campos {
			if c == "-" {
				sep = i
				break
			}
		}
		if len(campos) < 5 || sep < 0 || sep+1 >= len(campos) {
			continue
		}
		out = append(out, montaje{punto: desescaparMountinfo(campos[4]), fstype: campos[sep+1]})
	}
	return out, sc.Err()
}

func desescaparMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// montadoEncima dice si ruta es la raíz de un montaje distinto del de su
// directorio padre (distinto dispositivo). Es la comprobación que va antes de
// cualquier RemoveAll que pase por encima de un bind: RemoveAll no se para en
// los puntos de montaje y borraría lo de dentro.
func montadoEncima(ruta string) (bool, error) {
	var st, padre syscall.Stat_t
	if err := syscall.Lstat(ruta, &st); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := syscall.Lstat(filepath.Dir(ruta), &padre); err != nil {
		return false, err
	}
	return st.Dev != padre.Dev, nil
}
