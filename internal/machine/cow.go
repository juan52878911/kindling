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
//     fichero propio con reflink ($root/cow.xfs, o $root/cow.btrfs donde el
//     núcleo no tiene XFS, en $root/cow) y guarda ahí, una vez por dorado, una
//     copia "base" de su overlay; cada instancia es un FICLONE de esa base. El overlay de la instancia vive en $root/cow/m/<id>/ y
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
	"slices"
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
			return cowModoStore, "daemon.cow=reflink-store: overlays are reflinked inside kindling's copy-on-write store"
		}
		return cowModoCopy, fmt.Sprintf("daemon.cow=reflink-store, but the store is unavailable (%v): copying overlays", errAlmacen)
	case "", CoWAuto:
		if nativo {
			return cowModoReflink, "the data root supports reflink (FICLONE): overlays share blocks with their golden"
		}
		if errAlmacen == nil {
			return cowModoStore, "no reflink on the data root: overlays are reflinked inside kindling's copy-on-write store"
		}
		return cowModoCopy, fmt.Sprintf("no reflink on the data root and no copy-on-write store (%v): copying overlays", errAlmacen)
	}
	return cowModoCopy, fmt.Sprintf("unknown daemon.cow %q: copying overlays", pedido)
}

// notaAlmacenPendiente completa el motivo del modo store cuando el almacén aún
// no existe: de qué tipo será y, si el núcleo todavía no lista ese sistema de
// ficheros, que tendrá que cargar su módulo al montarlo (en un contenedor LXC
// no puede, y el primer run -from lo descubrirá).
func notaAlmacenPendiente(fs string, conoce bool) string {
	n := fmt.Sprintf(" (%s store, created on the first save or run -from", fs)
	if !conoce {
		n += fmt.Sprintf("; the kernel does not list %s yet: it has to load its module to mount it", fs)
	}
	return n + ")"
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

// degradarSinAlmacen degrada (degradar) porque el almacén no se pudo
// preparar (errAlmacenNoDisponible): lo mismo da quién lo descubrió, un run
// -from o el espejo tras un save.
func (e *estadoCoW) degradarSinAlmacen(err error) {
	e.degradar(fmt.Sprintf("copy-on-write store unavailable (%v): copying overlays until the daemon restarts", err))
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
		// En modo store, hasta el primer save o run -from el almacén no
		// existe (o no se ha montado): se crea entonces. Decir "store" sin más daba por
		// hecho algo que aún no se ha probado.
		info.Pending = info.Mode == cowModoStore && !m.alm.estaListo()
	}
	return info
}

// GrowCoWStore amplía el almacén de copias de disco (POST /cow/store/grow):
// hasta sizeMiB, o en addMiB. Uno de los dos. Devuelve cómo queda.
func (m *Manager) GrowCoWStore(ctx context.Context, sizeMiB, addMiB int64) (*api.CoWStore, error) {
	if m.alm == nil {
		return nil, errors.New("there is no copy-on-write store on this platform")
	}
	const maxMiB = 1 << 30 // 1 PiB: más es un error de unidades, no un almacén
	switch {
	case sizeMiB < 0 || addMiB < 0 || sizeMiB > maxMiB || addMiB > maxMiB:
		return nil, errors.New("invalid size")
	case (sizeMiB > 0) == (addMiB > 0):
		return nil, errors.New("give either the new size or how much to add")
	}
	if err := m.alm.crecer(ctx, sizeMiB<<20, addMiB<<20); err != nil {
		return nil, err
	}
	return m.alm.info(), nil
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
		var lleno *errAlmacenLleno
		if errors.As(err, &lleno) {
			// Sin sitio en el almacén, la copia completa en la raíz solo si
			// cabe ENTERA con el margen de la raíz: una copia dispersa que
			// luego no puede crecer da el mismo EIO en el invitado, solo que
			// más tarde.
			if cabe := m.alm.cabeCopiaEnRaiz(src); cabe != nil {
				return "", fmt.Errorf("%v; %v", err, cabe)
			}
			log.Printf("warning: %v; copying the overlay of %s to the data root instead", err, shortID(id))
		} else if errors.Is(err, errAlmacenNoDisponible) {
			m.cow.degradarSinAlmacen(err)
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
// descriptor ya abierto y no por ruta, que es un fichero regular. Devuelve el
// fichero ABIERTO: la copia se hace desde ese descriptor (copiarOverlayDesde),
// nunca volviendo a abrir la ruta. El VMM es dueño del overlay y puede cambiarlo
// por un enlace simbólico a un fichero de root entre la comprobación y la
// copia, y dejarlo como estaba después; una copia por ruta se llevaría al
// dorado el otro fichero y ninguna comprobación posterior lo vería. El
// descriptor sigue apuntando al inodo que se comprobó, pase lo que pase con la
// ruta.
//
// La función que devuelve, a llamar tras la copia, mira además que la ruta
// siga siendo el mismo fichero: ya no es lo que protege la copia, pero un
// overlay cambiado a mitad de un commit es un dorado que no corresponde a la
// memoria volcada, y se descarta.
func fijarOverlayParaLeer(ruta string) (*os.File, func() error, error) {
	f, err := os.OpenFile(ruta, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the overlay %s: %w", ruta, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("the overlay %s is not a regular file", ruta)
	}
	return f, func() error {
		ahora, err := os.Lstat(ruta)
		if err != nil || !ahora.Mode().IsRegular() || !os.SameFile(fi, ahora) {
			return fmt.Errorf("the overlay %s changed while it was being copied", ruta)
		}
		return nil
	}, nil
}

// copiarOverlayDesde copia en dst el overlay abierto en in (ver
// fijarOverlayParaLeer): con FICLONE si el modo es reflink, y si no (o si
// falla) con una copia dispersa en Go. Todo va por descriptores: se lee del
// que se comprobó y se escribe en un dst creado con O_EXCL|O_NOFOLLOW, porque
// dst puede estar en un directorio del VMM (el jail de la plantilla) y un
// enlace plantado ahí haría que root escribiera, o cediera, otro fichero. Si
// own no es nil se aplica al fichero creado, también por su descriptor, y no
// con un chown por ruta que seguiría un enlace. Devuelve la identidad del
// fichero creado, para comprobar después que el que se recupera es este.
//
// En macOS se clona con fclonefileat desde el mismo descriptor (clon_darwin.go),
// y si el disco no es APFS (ENOTSUP) o dst está en otro volumen (EXDEV), copia
// dispersa.
func (m *Manager) copiarOverlayDesde(ctx context.Context, in *os.File, dst string, own func(*os.File) error) (os.FileInfo, error) {
	out, clonado, err := crearDestinoOverlay(in, dst, m.cow.actual() == cowModoReflink)
	if err != nil {
		return nil, err
	}
	fallo := func(err error) (os.FileInfo, error) {
		out.Close()
		_ = os.Remove(dst)
		return nil, err
	}
	if !clonado {
		if err := copiarDisperso(ctx, in, out); err != nil {
			return fallo(err)
		}
	}
	if own != nil {
		if err := own(out); err != nil {
			return fallo(err)
		}
	}
	fi, err := out.Stat()
	if err != nil {
		return fallo(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return nil, err
	}
	return fi, nil
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
	memoriaDorado := func(snap string) string {
		return filepath.Join(m.snapDir(snap), "mem.file")
	}
	m.alm.barrer(viva, overlayDorado, memoriaDorado)
}

// ── el almacén ────────────────────────────────────────────────────────────────

// errAlmacenNoDisponible envuelve los fallos de preparar el almacén (crear,
// formatear, montar, probar): con ellos no tiene sentido reintentar en cada
// instancia.
var errAlmacenNoDisponible = errors.New("store unavailable")

// libreMinimaAlmacen es lo que tiene que quedar ASIGNABLE en el almacén para
// clonar en él o despertar a una instancia que vive en él: un reflink no
// ocupa nada, pero la instancia va a escribir. Por debajo, una instancia nueva
// va a una copia completa en la raíz si allí cabe entera (clonarOverlayInstancia),
// y una que ya vive en el almacén no se despierta (comprobarAlmacenPara).
const libreMinimaAlmacen = 256 << 20

// marcaPausaAlmacen es por debajo de cuánto espacio asignable el vigilante del
// almacén pausa las instancias que escriben en él (ver vigilarAlmacen). Menor
// que libreMinimaAlmacen: una instancia recién admitida no se pausa al nacer, y
// entre las dos marcas está la histéresis de la reanudación.
const marcaPausaAlmacen = 128 << 20

// errAlmacenLleno es el almacén sin espacio asignable para una instancia más.
// El mensaje dice cuánto queda y qué hacer, en vez de dejar que el invitado lo
// descubra con un EIO.
type errAlmacenLleno struct{ libre int64 }

func (e *errAlmacenLleno) Error() string {
	return fmt.Sprintf("copy-on-write store full (%d MiB free): kling cow grow +%dG", max(e.libre, 0)>>20, crecimientoSugeridoGiB)
}

// crecimientoSugeridoGiB es lo que proponen los mensajes de almacén lleno.
const crecimientoSugeridoGiB = 4

// cabeCopiaEnRaiz dice por qué una copia completa del overlay src no cabe en
// la raíz (su tamaño lógico más margenRaizAlmacen), o nil si cabe.
func (a *almacenCoW) cabeCopiaEnRaiz(src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	_, libre, err := a.libreEn(a.root)
	if err != nil {
		return err
	}
	if libre < fi.Size()+margenRaizAlmacen {
		return fmt.Errorf("and a full copy (%d MiB) doesn't fit on the data root (%d MiB free)", fi.Size()>>20, libre>>20)
	}
	return nil
}

// libreDentro es el espacio del almacén montado, asignable de verdad.
func (a *almacenCoW) libreDentro() (total, libre int64, err error) {
	if a.libreAlmacen != nil {
		return a.libreAlmacen(a.dir)
	}
	return a.libreEn(a.dir)
}

// almacenCoW es el almacén propio: un XFS con reflink (o un Btrfs, ver
// elegirFSAlmacen) en un fichero, montado por loop dentro de la raíz de
// kindling. Todo lo que toca el sistema (crear
// el fichero, formatear, montar, clonar) va por funciones que pone la
// plataforma, para poder probar la lógica sin root.
type almacenCoW struct {
	mu   sync.Mutex
	root string // $root
	fs   string // "xfs" o "btrfs"
	img  string // $root/cow.xfs o $root/cow.btrfs
	dir  string // $root/cow (punto de montaje)
	priv *Privileges
	// viva dice si la máquina id existe (o se está creando): un directorio de
	// instancia de una máquina que no está viva es un residuo y se reemplaza.
	viva func(id string) bool

	montado  bool
	errFatal error // la preparación falló: no se reintenta hasta reiniciar
	errFS    error // no hay sistema de ficheros para crear el almacén (fsDelAlmacen)

	// Operaciones del sistema, de la plataforma (cow_fc.go) o de un test.
	estaMontado func(dir string) (bool, error)
	crear       func(ctx context.Context, img string, bytes int64) error
	montar      func(ctx context.Context, img, dir string) error
	clonar      func(src, dst string) error
	copiar      func(ctx context.Context, src, dst string) error
	libreEn     func(dir string) (total, libre int64, err error)
	// libreAlmacen es libreEn para el punto de montaje del almacén: lo de
	// verdad asignable (cow_asignable.go). nil = libreEn.
	libreAlmacen func(dir string) (total, libre int64, err error)

	// Para no dejar atrás un almacén que no monta y para hacerlo crecer
	// (cow_fc.go). nil en un test que no los usa:
	//   candidatos son los tipos posibles para un almacén nuevo, por preferencia.
	//   reconfigurar cambia el tipo del almacén (fichero, operaciones, cuota).
	//   desmontar desmonta el almacén.
	//   agrandar amplía el fichero, el loop y el sistema de ficheros a bytes.
	candidatos   func() []string
	reconfigurar func(fs string)
	desmontar    func(dir string) error
	agrandar     func(ctx context.Context, img, dir string, bytes int64) error

	// Cuota por instancia (cow_cuota.go). Todas nil en un test o sin las
	// herramientas: el almacén funciona igual, sin cuota.
	//   detectarCuota dice qué cuota impone este montaje ("prjquota", "qgroup" o "").
	//   crearDir crea el directorio de la instancia (un subvolumen en Btrfs).
	//   limitar aplica la cuota de bytes al directorio de la instancia y a su overlay.
	//   quitarDir borra el directorio de la instancia entero.
	detectarCuota func(ctx context.Context) string
	crearDir      func(d string) error
	limitar       func(d, overlay string, bytes int64) error
	quitarDir     func(d string) error
	cuota         string // lo que detectó detectarCuota al montar

	// espejando son los espejos de memoria que alguien copia ahora mismo sin
	// a.mu (baseMemoria): la ruta definitiva y un canal que se cierra al
	// acabar, bien o mal.
	espejando map[string]chan struct{}
}

// holguraCuota es lo que se deja por encima del tamaño lógico del overlay: el
// sistema de ficheros puede contar más que el tamaño del fichero (asignación
// especulativa, metadatos del propio fichero). La cuota no es un límite
// ajustado a propósito: el objetivo es que un VMM comprometido no pueda hacer
// crecer su overlay, no medir el uso al byte.
func cuotaInstancia(tam int64) int64 {
	return tam + tam/32 + 16<<20
}

// mkdirInstancia crea el directorio de la instancia (con su hook si lo hay).
func (a *almacenCoW) mkdirInstancia(d string) error {
	if a.crearDir != nil && a.cuota != "" {
		return a.crearDir(d)
	}
	return os.Mkdir(d, 0o700)
}

// rmdirInstancia borra el directorio de una instancia, sea un directorio o un
// subvolumen. Si el hook falla se reintenta una vez (un `btrfs subvolume
// delete` puede fallar por algo pasajero) y, si vuelve a fallar, se dice
// claro en el log: RemoveAll vacía un subvolumen pero no lo quita (EPERM), y
// el que quede ocupa su qgroup y bloquea el id hasta que alguien lo borre.
func (a *almacenCoW) rmdirInstancia(d string) {
	if a.quitarDir != nil && a.cuota != "" {
		err := a.quitarDir(d)
		if err == nil {
			return
		}
		if _, serr := os.Lstat(d); os.IsNotExist(serr) {
			return
		}
		if err = a.quitarDir(d); err == nil {
			return
		}
		if _, serr := os.Lstat(d); os.IsNotExist(serr) {
			return
		}
		log.Printf("warning: copy-on-write store: could not remove %s (%v); if it is a subvolume, "+
			"remove it by hand with: btrfs subvolume delete %s", d, err, d)
	}
	if err := os.RemoveAll(d); err != nil {
		log.Printf("warning: copy-on-write store: removing %s: %v", d, err)
	}
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

// existe dice si el almacén está creado (el fichero de imagen existe). Con
// a.mu, salvo al arrancar (antes de servir nada): a.img puede cambiar.
func (a *almacenCoW) existe() bool {
	_, err := os.Lstat(a.img)
	return err == nil
}

// estaListo dice si el almacén está montado y probado: hasta entonces el modo
// store está pendiente (api.CoWInfo.Pending).
func (a *almacenCoW) estaListo() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.montado
}

// comprobarEspacio dice si cabe un almacén nuevo en la raíz (tamAlmacen): si
// no, se sabe ya al arrancar y no hace falta esperar al primer run -from para
// decir por qué se copia.
func (a *almacenCoW) comprobarEspacio(gib int) error {
	_, libre, err := a.libreEn(a.root)
	if err != nil {
		return err
	}
	_, err = tamAlmacen(libre, gib)
	return err
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
	err := a.asegurarMontado(ctx)
	if err == nil {
		return
	}
	// Un almacén que no monta y que no usa ninguna instancia (lo dejó un
	// daemon anterior que no llegó a usarlo, o el núcleo ya no tiene su
	// sistema de ficheros) no guarda nada que no se pueda rehacer: las bases
	// se vuelven a copiar. Se quita, en vez de dejar un fichero de gigas
	// reservados y un aviso en cada arranque; el primer run -from lo crea otra
	// vez, con el tipo que este núcleo pueda montar.
	if a.enUso() {
		log.Printf("WARNING: copy-on-write store %s: %v; instances whose overlay lives there won't start until it is mounted", a.img, err)
		return
	}
	// Solo se borra cuando el fallo es definitivo (el núcleo no tiene ese
	// sistema de ficheros, o aquí no se deja montar). Cualquier otro (un loop
	// ocupado, un fsck a medias, un tiempo agotado) puede ser pasajero: se
	// avisa y la imagen se queda para el siguiente intento.
	if !falloAlmacenDefinitivo(err) {
		log.Printf("WARNING: copy-on-write store %s could not be mounted: %v; kept (the error may be transient); it is mounted again on the next run -from", a.img, err)
		return
	}
	if derr := a.desechar(); derr != nil {
		log.Printf("WARNING: copy-on-write store %s: %v; no instance uses it, but it could not be removed: %v", a.img, err, derr)
		return
	}
	log.Printf("warning: copy-on-write store %s could not be mounted (%v%s) and no instance uses it: removed; it is created again on the next run -from", a.img, err, pistaFalloAlmacen(a.fs, err))
	if a.candidatos != nil {
		if c := a.candidatos(); len(c) > 0 && c[0] != a.fs {
			a.usarFS(c[0])
		}
	}
}

// enUso dice si alguna máquina tiene su overlay en el almacén: su
// machines/<id>/overlay.ext4 es un enlace que apunta dentro de a.dir. Ante la
// duda (no se puede leer machines/), sí.
func (a *almacenCoW) enUso() bool {
	entradas, err := os.ReadDir(filepath.Join(a.root, "machines"))
	if err != nil {
		return !os.IsNotExist(err)
	}
	for _, e := range entradas {
		dest, err := os.Readlink(filepath.Join(a.root, "machines", e.Name(), "overlay.ext4"))
		if err == nil && strings.HasPrefix(filepath.Clean(dest), a.dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// desechar desmonta (si está montado) y borra la imagen del almacén. Solo
// para uno recién creado que no funciona o uno que ninguna instancia usa. Si
// sobre el punto de montaje hay otra cosa, o no se puede desmontar, no borra
// nada: con la imagen montada el borrado no liberaría el sitio. Con a.mu.
func (a *almacenCoW) desechar() error {
	a.montado = false
	ya, err := a.estaMontado(a.dir)
	if err != nil {
		return err
	}
	if ya {
		if a.desmontar == nil {
			return fmt.Errorf("%s is mounted", a.dir)
		}
		if err := a.desmontar(a.dir); err != nil {
			return fmt.Errorf("unmounting %s: %w", a.dir, err)
		}
	}
	if err := os.Remove(a.img); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// usarFS cambia el tipo del almacén (aún sin crear). Con a.mu.
func (a *almacenCoW) usarFS(fs string) {
	if a.reconfigurar != nil {
		a.reconfigurar(fs)
		return
	}
	a.fs, a.img = fs, filepath.Join(a.root, imgAlmacen(fs))
}

// tiposAProbar son los tipos con los que intentar crear el almacén: el suyo y
// luego los demás candidatos.
func (a *almacenCoW) tiposAProbar() []string {
	out := []string{a.fs}
	if a.candidatos != nil {
		for _, fs := range a.candidatos() {
			if !slices.Contains(out, fs) {
				out = append(out, fs)
			}
		}
	}
	return out
}

// crearYMontar crea el almacén de bytes y lo monta. Si el tipo elegido no
// monta o no clona (el núcleo no tiene el módulo, el contenedor no deja
// montar ese tipo), su imagen se desmonta y se borra, y se prueba con el
// siguiente tipo: nunca queda atrás un fichero reservado que no sirve. Con
// a.mu.
func (a *almacenCoW) crearYMontar(ctx context.Context, bytes int64) error {
	var fallos []string
	for i, fs := range a.tiposAProbar() {
		if i > 0 {
			a.usarFS(fs)
			log.Printf("trying the copy-on-write store as %s instead", fs)
		}
		log.Printf("creating the copy-on-write store %s (%d MiB, reserved up front)", a.img, bytes>>20)
		err := a.crear(ctx, a.img, bytes)
		if err == nil {
			if err = a.asegurarMontado(ctx); err == nil {
				return nil
			}
			if derr := a.desechar(); derr != nil {
				// No se sigue: la imagen sigue ahí y otro tipo no cambiaría eso.
				return fmt.Errorf("%s: %v%s; and the image could not be removed: %v", fs, err, pistaFalloAlmacen(fs, err), derr)
			}
			log.Printf("warning: the copy-on-write store as %s does not work (%v): image removed", fs, err)
		}
		fallos = append(fallos, fmt.Sprintf("%s: %v%s", fs, err, pistaFalloAlmacen(fs, err)))
	}
	return errors.New(strings.Join(fallos, "; "))
}

// pistaFalloAlmacen explica los fallos de montar el almacén que tienen una
// causa conocida.
func pistaFalloAlmacen(fs string, err error) string {
	switch causaFalloAlmacen(err) {
	case falloSinFS:
		return fmt.Sprintf(" (the kernel has no %s: load its module on the host, or install the tools of the other filesystem)", fs)
	case falloSinPermiso:
		return " (mounting is not allowed here: in an LXC container, check that it is privileged and its AppArmor profile)"
	}
	return ""
}

// Causas conocidas de que el almacén no monte.
const (
	falloOtro = iota
	falloSinFS
	falloSinPermiso
)

// causaFalloAlmacen clasifica un fallo al montar el almacén: el núcleo no
// tiene su sistema de ficheros, montar no está permitido aquí (EPERM/EACCES),
// u otra cosa.
func causaFalloAlmacen(err error) int {
	if err == nil {
		return falloOtro
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "unknown filesystem type"):
		return falloSinFS
	case strings.Contains(s, "ermission denied") || strings.Contains(s, "not permitted"):
		return falloSinPermiso
	}
	return falloOtro
}

// falloAlmacenDefinitivo dice si un fallo al montar no se arregla solo: los
// que pistaFalloAlmacen reconoce. Solo esos justifican borrar una imagen.
func falloAlmacenDefinitivo(err error) bool {
	return causaFalloAlmacen(err) != falloOtro
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
	a.cuota = ""
	if a.detectarCuota != nil {
		a.cuota = a.detectarCuota(ctx)
	}
	if a.cuota == "" && a.detectarCuota != nil {
		log.Printf("WARNING: copy-on-write store %s: no per-instance disk quota (see docs/cow.md, \"Disk quota\")", a.dir)
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
// reflink (xfsprogs muy viejo) se montaría igual y aquí se ve; y es lo que
// demuestra que el Btrfs clona dentro de este núcleo y este contenedor.
func (a *almacenCoW) probar() error {
	dir := filepath.Join(a.dir, "bases")
	src := filepath.Join(dir, fmt.Sprintf(".probe-%d", time.Now().UnixNano()))
	dst := src + "-clone"
	defer os.Remove(src)
	defer os.Remove(dst)
	err := os.WriteFile(src, make([]byte, 4096), 0o600)
	if err == nil {
		err = a.clonar(src, dst)
		if err != nil && !errors.Is(err, syscall.ENOSPC) {
			return fmt.Errorf("the store does not reflink: %w", err)
		}
	}
	// Lleno no es roto: el almacén ya clonó cuando se creó, y sus instancias
	// lo necesitan montado para despertar (o para que se vea que no caben).
	// Si se diera por no disponible, el daemon pasaría a copiar hasta
	// reiniciarse y nadie miraría ya cuánto sitio le queda. Visto en el
	// laboratorio: un Btrfs lleno al arrancar el daemon, liberado segundos
	// después por su limpiador de subvolúmenes borrados.
	if errors.Is(err, syscall.ENOSPC) {
		log.Printf("warning: copy-on-write store %s is full (%v): mounted anyway; new instances copy until there is room (kling cow grow)", a.dir, err)
		return nil
	}
	return err
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
			return a.crearYMontar(ctx, bytes)
		}
		return a.asegurarMontado(ctx)
	}()
	if err != nil {
		a.errFatal = fmt.Errorf("%w: %v", errAlmacenNoDisponible, err)
		return a.errFatal
	}
	return nil
}

// margenRaizAlmacen es lo que el almacén deja libre en la raíz al crearse o
// crecer, como minFreeDiskMiB.
const margenRaizAlmacen = 2 << 30

// tamAlmacen es cuánto reservar para el almacén: lo pedido, o una cuarta
// parte del disco libre con un máximo de 16 GiB. Se reserva entero
// (fallocate): un XFS o un Btrfs sobre un fichero disperso que se queda sin
// sitio debajo recibe errores de E/S y se apaga (o pasa a solo lectura), con
// todas sus instancias dentro. Mejor
// una cuota fija que un almacén que puede romperse por algo que hace otro.
func tamAlmacen(libre int64, gib int) (int64, error) {
	if gib > 0 {
		b := int64(gib) << 30
		if b > libre-margenRaizAlmacen {
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
	// siguen vivos en las instancias que se clonaron de ellas: XFS y Btrfs
	// cuentan las referencias. Los espejos de memoria (.mem) son de
	// baseMemoria y no se tocan aquí.
	if entradas, err := os.ReadDir(dir); err == nil {
		for _, e := range entradas {
			if e.Name() != filepath.Base(ruta) && strings.HasSuffix(e.Name(), ".ext4") {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return ruta, nil
}

// clonarInstancia prepara el almacén si hace falta, asegura la base del
// dorado y la clona para la instancia id. Devuelve la ruta del overlay de la
// instancia, ya abierto al VMM por grupo (ver cederPorGrupo).
func (a *almacenCoW) clonarInstancia(ctx context.Context, snap, src, id string, gib int) (string, error) {
	if err := nombreSeguro(id); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.preparar(ctx, gib); err != nil {
		return "", err
	}
	if _, libre, err := a.libreDentro(); err == nil && libre < libreMinimaAlmacen {
		return "", &errAlmacenLleno{libre: libre}
	}
	base, err := a.base(ctx, snap, src)
	if err != nil {
		return "", err
	}
	d := a.dirInstancia(id)
	if err := a.mkdirInstancia(d); err != nil {
		if !errors.Is(err, os.ErrExist) || a.viva == nil || a.viva(id) {
			return "", err
		}
		// Un directorio residual (un runFrom que murió, un barrido que no llegó):
		// la máquina no está viva, así que sobra y no debe mandar a la instancia
		// a copia completa.
		a.rmdirInstancia(d)
		if err := a.mkdirInstancia(d); err != nil {
			return "", err
		}
	}
	ruta := filepath.Join(d, "overlay.ext4")
	if err := a.clonar(base, ruta); err != nil {
		a.rmdirInstancia(d)
		return "", fmt.Errorf("reflinking the overlay: %w", err)
	}
	// La base es 0400 y el clon hereda el modo: se abre para el VMM.
	_ = os.Chmod(ruta, 0o600)
	// La cuota, antes de ceder el fichero al VMM. Si el almacén la impone y no
	// se puede aplicar, la instancia NO va al almacén: sin cuota, un VMM
	// comprometido podría llenarlo.
	if a.cuota != "" && a.limitar != nil {
		fi, err := os.Stat(ruta)
		if err == nil {
			err = a.limitar(d, ruta, cuotaInstancia(fi.Size()))
		}
		if err != nil {
			a.rmdirInstancia(d)
			return "", fmt.Errorf("applying the disk quota (%s): %w", a.cuota, err)
		}
	}
	if a.priv != nil && a.priv.Enabled {
		// El VMM NO es dueño de nada aquí: el overlay queda del daemon (root) y
		// el VMM lo lee y escribe por grupo (0660); el directorio, root:grupo
		// 0750, solo lo atraviesa. Dueño del FICHERO podría cambiarle el id de
		// proyecto de XFS con FS_IOC_FSSETXATTR (el núcleo lo permite al dueño,
		// inode_owner_or_capable) y salirse de su cuota o comerse la de otra
		// instancia. Dueño del directorio podría crear ficheros en él (llenar el
		// almacén compartido) y cambiar el overlay por un enlace entre la
		// comprobación del daemon y su lectura.
		if err := cederPorGrupo(ruta, os.Geteuid(), a.priv.GID); err != nil {
			a.rmdirInstancia(d)
			return "", err
		}
		if err := os.Lchown(d, os.Geteuid(), a.priv.GID); err != nil {
			a.rmdirInstancia(d)
			return "", fmt.Errorf("securing %s: %w", d, err)
		}
		if err := os.Chmod(d, 0o750); err != nil {
			a.rmdirInstancia(d)
			return "", err
		}
	}
	return ruta, nil
}

// cederPorGrupo deja el fichero ruta con dueño y grupo dados y modo 0660: el
// grupo (el del VMM) lo lee y escribe, pero no es suyo, así que no puede
// cambiarle los atributos que solo toca el dueño (el id de proyecto de XFS, el
// modo, los permisos). Por descriptor y sin seguir enlaces: el directorio es
// del daemon, pero no cuesta nada no fiarse de la ruta.
func cederPorGrupo(ruta string, dueño, grupo int) error {
	fd, err := syscall.Open(ruta, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("granting %s: %w", ruta, err)
	}
	f := os.NewFile(uintptr(fd), ruta)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("granting %s: %w", ruta, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("granting %s: not a regular file", ruta)
	}
	if err := f.Chown(dueño, grupo); err != nil {
		return fmt.Errorf("granting %s: %w", ruta, err)
	}
	if err := f.Chmod(0o660); err != nil {
		return fmt.Errorf("granting %s: %w", ruta, err)
	}
	return nil
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
	a.rmdirInstancia(a.dirInstancia(id))
}

// barrer quita las instancias sin máquina y las bases sin dorado (o de una
// versión anterior del dorado): el overlay (.ext4) y el espejo de la memoria
// (.mem, cow_memoria.go), cada uno con la clave de su fichero del dorado.
func (a *almacenCoW) barrer(viva func(id string) bool, overlayDorado, memoriaDorado func(snap string) string) {
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
			a.rmdirInstancia(filepath.Join(a.dir, "m", e.Name()))
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
		vigenteMem := ""
		if fm, err := os.Stat(memoriaDorado(e.Name())); err == nil {
			vigenteMem = claveBase(fm) + sufijoBaseMemoria
		}
		hijos, _ := os.ReadDir(dir)
		for _, h := range hijos {
			if h.Name() != vigente && h.Name() != vigenteMem && !a.espejoEnCurso(dir, h.Name()) {
				_ = os.Remove(filepath.Join(dir, h.Name()))
			}
		}
	}
}

// info es el estado del almacén para GET /info; nil si no existe.
func (a *almacenCoW) info() *api.CoWStore {
	// Todo bajo a.mu: el tipo y la imagen cambian si el primero no monta
	// (crearYMontar), a la vez que alguien pregunta por GET /info.
	a.mu.Lock()
	if !a.existe() {
		a.mu.Unlock()
		return nil
	}
	s := &api.CoWStore{Path: a.dir, FS: a.fs, Mounted: a.montado}
	if s.Mounted {
		s.Quota, s.NoQuota = a.cuota, a.cuota == ""
	}
	a.mu.Unlock()
	if s.Mounted {
		if total, libre, err := a.libreDentro(); err == nil {
			s.SizeMiB, s.FreeMiB = total>>20, libre>>20
		}
	}
	return s
}

// crecer amplía el almacén montado hasta nuevo bytes, o en añadir bytes: el
// fichero (reservado, como al crearlo), su loop y el sistema de ficheros
// (agrandar). No encoge. Pedir el tamaño que ya tiene el fichero repite solo
// el loop y el sistema de ficheros: así se completa un crecimiento que se
// quedó a medias. Deja en la raíz el mismo margen que al crearlo.
func (a *almacenCoW) crecer(ctx context.Context, nuevo, añadir int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.montado {
		if !a.existe() {
			return errors.New("there is no copy-on-write store yet: it is created on the first save or run -from")
		}
		return fmt.Errorf("the copy-on-write store %s is not mounted", a.dir)
	}
	if a.agrandar == nil {
		return errors.New("growing the copy-on-write store is not supported here")
	}
	fi, err := os.Lstat(a.img)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", a.img)
	}
	actual := fi.Size()
	if añadir > 0 {
		nuevo = actual + añadir
	}
	nuevo &^= (1 << 20) - 1 // MiB enteros
	if nuevo < actual {
		return fmt.Errorf("the copy-on-write store already has %d MiB: it can't shrink", actual>>20)
	}
	if extra := nuevo - actual; extra > 0 {
		_, libre, err := a.libreEn(a.root)
		if err != nil {
			return err
		}
		if extra > libre-margenRaizAlmacen {
			return fmt.Errorf("growing the store by %d MiB doesn't fit: %d MiB free under the data root, and %d MiB must stay free",
				extra>>20, libre>>20, int64(margenRaizAlmacen)>>20)
		}
	}
	log.Printf("growing the copy-on-write store %s from %d to %d MiB", a.img, actual>>20, nuevo>>20)
	return a.agrandar(ctx, a.img, a.dir, nuevo)
}

// Los sistemas de ficheros que sirven de almacén, en orden de preferencia, con
// su programa de formateo y el nombre de su fichero de imagen. XFS primero: es
// el que se probó a fondo y el que ya tienen los almacenes existentes. Btrfs
// es para núcleos sin XFS, como el de Proxmox visto desde un contenedor LXC
// (que no puede cargar módulos).
var fsAlmacen = []struct{ fs, mkfs, paquete, img string }{
	{"xfs", "mkfs.xfs", "xfsprogs", "cow.xfs"},
	{"btrfs", "mkfs.btrfs", "btrfs-progs", "cow.btrfs"},
}

// imgAlmacen es el nombre del fichero de imagen del almacén de tipo fs.
func imgAlmacen(fs string) string {
	for _, c := range fsAlmacen {
		if c.fs == fs {
			return c.img
		}
	}
	return "cow." + fs
}

// soportados lee /proc/filesystems: una línea por tipo, "nodev" delante de los
// que no necesitan dispositivo.
func soportados(filesystems string) map[string]bool {
	out := make(map[string]bool)
	for _, l := range strings.Split(filesystems, "\n") {
		if campos := strings.Fields(l); len(campos) > 0 {
			out[campos[len(campos)-1]] = true
		}
	}
	return out
}

// elegirFSAlmacen elige el sistema de ficheros de un almacén nuevo: el primero
// que el núcleo ya conoce (/proc/filesystems) y cuyo mkfs está instalado. Si
// el núcleo no lista ninguno de los dos (el módulo aún no está cargado; se
// cargaría al montar), el primero con mkfs. Si no, error diciendo qué
// instalar. Es pura para probar la tabla sin root.
func elegirFSAlmacen(filesystems string, hayMkfs func(string) bool) (string, error) {
	conoce := soportados(filesystems)
	ninguno := true
	for _, c := range fsAlmacen {
		if conoce[c.fs] {
			ninguno = false
			if hayMkfs(c.mkfs) {
				return c.fs, nil
			}
		}
	}
	if ninguno {
		for _, c := range fsAlmacen {
			if hayMkfs(c.mkfs) {
				return c.fs, nil
			}
		}
		return "", errors.New("no mkfs.xfs or mkfs.btrfs: install xfsprogs (or btrfs-progs where the kernel has no XFS)")
	}
	var falta []string
	for _, c := range fsAlmacen {
		if conoce[c.fs] {
			falta = append(falta, fmt.Sprintf("%s not found (install %s)", c.mkfs, c.paquete))
		}
	}
	return "", errors.New(strings.Join(falta, "; "))
}

// ordenCandidatos son los tipos con los que se puede crear un almacén nuevo en
// este host, por preferencia: los que el núcleo ya lista y tienen mkfs, y
// luego los que tienen mkfs y el núcleo aún no lista (se cargarían al
// montar). Si el primero no monta, se prueba el siguiente (crearYMontar).
func ordenCandidatos(filesystems string, hayMkfs func(string) bool) []string {
	conoce := soportados(filesystems)
	var primero, luego []string
	for _, c := range fsAlmacen {
		switch {
		case !hayMkfs(c.mkfs):
		case conoce[c.fs]:
			primero = append(primero, c.fs)
		default:
			luego = append(luego, c.fs)
		}
	}
	return append(primero, luego...)
}

// argsCrecerFS es el programa y los argumentos que agrandan en caliente el
// sistema de ficheros montado en dir hasta llenar su dispositivo.
func argsCrecerFS(fs, dir string) (string, []string) {
	if fs == "btrfs" {
		return "btrfs", []string{"filesystem", "resize", "max", dir}
	}
	return "xfs_growfs", []string{dir}
}

// nombreLoop dice si fuente es un /dev/loopN y devuelve "loopN".
func nombreLoop(fuente string) (string, bool) {
	n, ok := strings.CutPrefix(fuente, "/dev/")
	num, ok2 := strings.CutPrefix(n, "loop")
	if !ok || !ok2 || num == "" {
		return "", false
	}
	for _, c := range num {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return n, true
}

// ── montajes ──────────────────────────────────────────────────────────────────

// montaje es una línea de /proc/self/mountinfo.
type montaje struct {
	punto  string
	fstype string
	opts   string // opciones del montaje y del superbloque, juntas
	fuente string // el dispositivo (/dev/loop3), tras el tipo
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
		opts := campos[5%len(campos)]
		if sep+3 < len(campos) {
			opts += "," + campos[sep+3]
		}
		mt := montaje{punto: desescaparMountinfo(campos[4]), fstype: campos[sep+1], opts: opts}
		if sep+2 < len(campos) {
			mt.fuente = desescaparMountinfo(campos[sep+2])
		}
		out = append(out, mt)
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

// montadoConTipo dice si dir (canónica) es un punto de montaje en ms, y exige
// que el montaje visible (el último sobre la ruta) sea de tipo fs.
func montadoConTipo(ms []montaje, dir, fs string) (bool, error) {
	tipo := ""
	for _, mt := range ms {
		if mt.punto == dir {
			tipo = mt.fstype // el último montaje sobre la ruta es el visible
		}
	}
	switch tipo {
	case "":
		return false, nil
	case fs:
		return true, nil
	}
	return false, fmt.Errorf("%s is mounted, but it is %s and not the %s store", dir, tipo, fs)
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
