//go:build !darwin

package machine

// Cuota por instancia en el almacén de copia al escribir (ver cow.go).
//
// Qué se limita y por qué así. El VMM escribe el FICHERO overlay.ext4 de su
// instancia por grupo (dueño root, 0660; ver cederPorGrupo), sin ser dueño de
// nada: dueño del fichero podría cambiarle el id de proyecto con
// FS_IOC_FSSETXATTR y salirse de la cuota. Un invitado legítimo no puede pasar
// del tamaño lógico de su disco, pero un Firecracker comprometido puede hacer
// crecer el fichero (ftruncate, escribir más allá) y llenar el almacén
// compartido. La
// cuota es, por tanto, el tamaño lógico del overlay más una holgura
// (cuotaInstancia), aplicada por el núcleo:
//
//   - XFS: cuota de proyecto. Cada overlay recibe su propio id de proyecto (por
//     ioctl, sobre el fichero: sin PROJINHERIT en el directorio, que haría fallar
//     el FICLONE desde la base con EXDEV) y un límite duro con xfs_quota. XFS
//     cuenta los bloques compartidos enteros, así que el límite no puede ser
//     menor que el tamaño lógico: es una cota de crecimiento, no una reserva.
//   - Btrfs: un subvolumen por instancia con un qgroup de límite referenciado.
//     FICLONE entre subvolúmenes del mismo Btrfs funciona.
//
// Sin las herramientas (xfs_quota, btrfs) o con un montaje sin cuota, el
// almacén funciona como antes y `kling doctor` lo avisa.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Los ioctl FS_IOC_FSGETXATTR / FS_IOC_FSSETXATTR (_IOR/_IOW 'X' 31/32 de
// struct fsxattr, 28 bytes). Valen en las arquitecturas donde corre kindling
// (x86-64 y arm64), como ioctlFICLONE.
const (
	ioctlFSGETXATTR = 0x801c581f
	ioctlFSSETXATTR = 0x401c5820
)

// primerProyecto es el primer id de proyecto que se reparte: el 0 es el
// proyecto por defecto de todo el almacén y se deja lo bajo a quien ya use
// proyectos para otra cosa.
const primerProyecto = 1000

type fsxattr struct {
	Xflags     uint32
	Extsize    uint32
	Nextents   uint32
	Projid     uint32
	Cowextsize uint32
	_          [8]byte
}

func proyectoDe(f *os.File) (uint32, error) {
	var x fsxattr
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlFSGETXATTR, uintptr(unsafe.Pointer(&x))); e != 0 {
		return 0, e
	}
	return x.Projid, nil
}

func fijarProyecto(f *os.File, id uint32) error {
	var x fsxattr
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlFSGETXATTR, uintptr(unsafe.Pointer(&x))); e != 0 {
		return e
	}
	x.Projid = id
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlFSSETXATTR, uintptr(unsafe.Pointer(&x))); e != 0 {
		return e
	}
	return nil
}

// abrirSinSeguir abre un fichero regular del almacén sin seguir enlaces: el
// directorio es de root pero el fichero lo escribe el VMM, y un enlace lo
// cambiaría por otro fichero.
func abrirSinSeguir(ruta string) (*os.File, error) {
	fd, err := syscall.Open(ruta, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), ruta)
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", ruta)
	}
	return f, nil
}

// asignadorProyectos reparte los ids de proyecto de XFS desde memoria: un
// conjunto de usados y el último repartido, bajo un mutex pequeño. Se siembra
// una vez con el escaneo de proyectosPorDir (sobrevive a un reinicio del
// daemon) y cada id se libera al borrar el directorio de su instancia. Es la
// única fuente de ids: dos instancias con el mismo proyecto compartirían cuota.
type asignadorProyectos struct {
	mu      sync.Mutex
	sembrar func() (map[string][]uint32, error) // el escaneo del disco (dir → ids); nil = vacío
	listo   bool
	usados  map[uint32]string            // id → directorio de la instancia
	porDir  map[string][]uint32          // directorio → ids
	nombres map[string]map[string]uint32 // directorio → fichero → id
	sig     uint32                       // desde dónde buscar el próximo
}

// sembrarSiHaceFalta hace el escaneo la primera vez. Con a.mu tomado. Si el
// escaneo falló no se da por hecho: se queda con lo que leyó y lo repite en la
// siguiente asignación (fundiendo, sin pisar lo ya repartido), porque dar por
// libres los id que no pudo leer repartiría un proyecto ya en uso.
func (p *asignadorProyectos) sembrarSiHaceFalta() {
	if p.listo {
		return
	}
	if p.usados == nil {
		p.usados = map[uint32]string{}
		p.porDir = map[string][]uint32{}
		p.nombres = map[string]map[string]uint32{}
		p.sig = primerProyecto
	}
	p.listo = true
	if p.sembrar != nil {
		vistos, err := p.sembrar()
		if err != nil {
			p.listo = false
			log.Printf("warning: copy-on-write store: scanning project ids failed (%v); retrying on the next allocation", err)
		}
		for dir, ids := range vistos {
			for _, id := range ids {
				if _, en := p.usados[id]; en {
					continue
				}
				p.usados[id] = dir
				p.porDir[dir] = append(p.porDir[dir], id)
			}
		}
	}
}

// asignar reserva un id libre para un fichero de la instancia d.
func (p *asignadorProyectos) asignar(d string) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.asignarLocked(d, "")
}

// asignarFichero reserva el id del fichero nombre de la instancia d y lo
// recuerda: recrear mem.full o mem.diff sin borrar la instancia reutiliza el
// id en vez de gastar uno nuevo en cada llamada.
func (p *asignadorProyectos) asignarFichero(d, nombre string) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.asignarLocked(d, nombre)
}

func (p *asignadorProyectos) asignarLocked(d, nombre string) uint32 {
	p.sembrarSiHaceFalta()
	if nombre != "" {
		if id, ok := p.nombres[d][nombre]; ok && p.usados[id] == d {
			return id
		}
	}
	id := p.sig
	if id < primerProyecto {
		id = primerProyecto
	}
	for {
		if _, en := p.usados[id]; !en {
			break
		}
		id++
	}
	p.usados[id] = d
	p.porDir[d] = append(p.porDir[d], id)
	if nombre != "" {
		if p.nombres[d] == nil {
			p.nombres[d] = map[string]uint32{}
		}
		p.nombres[d][nombre] = id
	}
	p.sig = id + 1
	return id
}

// liberar devuelve los ids de la instancia d. El siguiente reparto vuelve a
// mirar desde el más bajo liberado, para reutilizarlos.
func (p *asignadorProyectos) liberar(d string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.listo {
		return
	}
	for _, id := range p.porDir[d] {
		delete(p.usados, id)
		if id < p.sig {
			p.sig = id
		}
	}
	delete(p.porDir, d)
	delete(p.nombres, d)
}

// soltar libera un solo id (el que no llegó a usarse).
func (p *asignadorProyectos) soltar(d string, id uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.usados[id] == d {
		delete(p.usados, id)
		if id < p.sig {
			p.sig = id
		}
		for n, v := range p.nombres[d] {
			if v == id {
				delete(p.nombres[d], n)
			}
		}
		ids := p.porDir[d]
		for i, v := range ids {
			if v == id {
				p.porDir[d] = append(ids[:i], ids[i+1:]...)
				break
			}
		}
	}
}

// proyectoLibre es el primer id de proyecto desde primerProyecto que no está
// en usados.
func proyectoLibre(usados map[uint32]bool) uint32 {
	id := uint32(primerProyecto)
	for usados[id] {
		id++
	}
	return id
}

// proyectosPorDir recorre los ficheros de las instancias del almacén (el
// overlay y los de memoria: mem.full, mem.diff, el diff acumulado, cada uno
// con su proyecto) y devuelve, por directorio de instancia, los ids que ya
// tienen. Siembra al asignador: sobrevive a un reinicio del daemon. Mirar solo
// los overlays repartía a un overlay nuevo el id del mem.full de otra copia, y
// el límite del overlay se encontraba ya gastado: EDQUOT en el disco del
// invitado.
func proyectosPorDir(dirM string) (map[string][]uint32, error) {
	usados := map[string][]uint32{}
	instancias, err := os.ReadDir(dirM)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return usados, nil
		}
		return usados, err
	}
	var primero error
	fallo := func(err error) {
		if err != nil && !errors.Is(err, fs.ErrNotExist) && primero == nil {
			primero = err // una instancia borrada durante el escaneo no cuenta
		}
	}
	for _, inst := range instancias {
		if !inst.IsDir() {
			continue
		}
		dir := filepath.Join(dirM, inst.Name())
		ficheros, err := os.ReadDir(dir)
		if err != nil {
			fallo(err)
			continue
		}
		for _, e := range ficheros {
			if !e.Type().IsRegular() {
				continue
			}
			f, err := abrirSinSeguir(filepath.Join(dir, e.Name()))
			if err != nil {
				fallo(err)
				continue
			}
			id, err := proyectoDe(f)
			if err != nil {
				fallo(err)
			} else if id != 0 {
				usados[dir] = append(usados[dir], id)
			}
			f.Close()
		}
	}
	return usados, primero
}

// quotactl de XFS: Q_XSETQLIM sobre cuotas de proyecto (XQM_CMD(4) con tipo 2).
// Es lo que hace `xfs_quota -x -c "limit -p bhard=..."`, sin lanzar un proceso.
const (
	qXSetQLim      = (('X'<<8 + 4) << 8) | 2
	fsDQBHard      = 1 << 3 // FS_DQ_BHARD: d_blk_hardlimit vale
	fsDquotVersion = 1
	xfsProjQuota   = 2
)

// fsDiskQuota es struct fs_disk_quota de <linux/dqblk_xfs.h> (112 bytes).
type fsDiskQuota struct {
	Version   int8
	Flags     int8
	Fieldmask uint16
	ID        uint32
	BlkHard   uint64 // en bloques de 512 bytes
	BlkSoft   uint64
	InoHard   uint64
	InoSoft   uint64
	BCount    uint64
	ICount    uint64
	ITimer    int32
	BTimer    int32
	IWarns    uint16
	BWarns    uint16
	_         int32
	RtbHard   uint64
	RtbSoft   uint64
	RtbCount  uint64
	RtbTimer  int32
	RtbWarns  uint16
	_         int16
	_         [8]byte
}

// limiteQuotactlXFS arma la petición de límite duro de bloques de un proyecto.
func limiteQuotactlXFS(id uint32, bytes int64) fsDiskQuota {
	return fsDiskQuota{
		Version: fsDquotVersion, Flags: xfsProjQuota, Fieldmask: fsDQBHard,
		ID: id, BlkHard: uint64((bytes + 511) / 512),
	}
}

// limiteXFSQuotactl fija el límite duro de bytes del proyecto id con la
// llamada quotactl sobre el dispositivo dev del montaje.
func limiteXFSQuotactl(dev string, id uint32, bytes int64) error {
	p, err := syscall.BytePtrFromString(dev)
	if err != nil {
		return err
	}
	q := limiteQuotactlXFS(id, bytes)
	_, _, e := syscall.Syscall6(syscall.SYS_QUOTACTL, uintptr(qXSetQLim), uintptr(unsafe.Pointer(p)),
		uintptr(id), uintptr(unsafe.Pointer(&q)), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// quotactlXFS fija límites por quotactl y recuerda si el núcleo no lo admite.
type quotactlXFS struct {
	dev    string
	roto   atomic.Bool
	llamar func(dev string, id uint32, bytes int64) error
}

// fijar intenta el límite por quotactl y dice si quedó hecho.
func (q *quotactlXFS) fijar(id uint32, bytes int64) bool {
	if q.roto.Load() || q.dev == "" {
		return false
	}
	if err := q.llamar(q.dev, id, bytes); err != nil {
		// Solo es permanente si el núcleo o el dispositivo no lo admiten; un
		// EINTR o un fallo pasajero se reintenta en la siguiente llamada.
		if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EPERM) ||
			errors.Is(err, syscall.ENOTBLK) || errors.Is(err, syscall.EINVAL) {
			q.roto.Store(true)
		}
		log.Printf("warning: copy-on-write store: quotactl on %s failed (%v); using xfs_quota", q.dev, err)
		return false
	}
	return true
}

// ejecutar corre una herramienta del sistema con un tope de tiempo.
func ejecutar(bin string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %s", filepath.Base(bin), strings.Join(args, " "), conSalida(err, out))
	}
	return nil
}

// argsLimiteXFS son los argumentos de xfs_quota para el límite duro de un
// proyecto: kibibytes, redondeando hacia arriba.
func argsLimiteXFS(id uint32, bytes int64, dir string) []string {
	kib := (bytes + 1023) / 1024
	return []string{"-x", "-c", fmt.Sprintf("limit -p bhard=%dk %d", kib, id), dir}
}

// opcionesConCuotaXFS dice si las opciones de un montaje XFS imponen la cuota
// de proyecto (prjquota / pquota; pqnoenforce la contabiliza pero no la impone).
func opcionesConCuotaXFS(opts string) bool {
	impone := false
	for _, o := range strings.Split(opts, ",") {
		switch o {
		case "prjquota", "pquota":
			impone = true
		case "pqnoenforce", "prjqnoenforce":
			return false
		}
	}
	return impone
}

func montajeDe(dir string) (montaje, bool) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return montaje{}, false
	}
	defer f.Close()
	ms, err := parsearMountinfo(f)
	if err != nil {
		return montaje{}, false
	}
	dir = rutaCanonica(dir)
	var out montaje
	ok := false
	for _, mt := range ms {
		if mt.punto == dir {
			out, ok = mt, true // el último es el visible
		}
	}
	return out, ok
}

// activarCuota engancha en el almacén la cuota que su sistema de ficheros
// permita, si están las herramientas; si no, deja los hooks a nil.
func (a *almacenCoW) activarCuota() {
	a.liberarProyectos = nil
	switch a.fs {
	case "xfs":
		a.detectarCuota = func(context.Context) string {
			if buscarE2fs("xfs_quota") == "" {
				return ""
			}
			mt, ok := montajeDe(a.dir)
			if !ok || !opcionesConCuotaXFS(mt.opts) {
				return ""
			}
			return "prjquota"
		}
		// Los ids se reparten desde memoria, sembrados una vez con el escaneo
		// del almacén; se liberan al borrar la instancia (rmdirInstancia).
		ids := &asignadorProyectos{sembrar: func() (map[string][]uint32, error) {
			return proyectosPorDir(filepath.Join(a.dir, "m"))
		}}
		a.liberarProyectos = ids.liberar
		var devOnce sync.Once
		qc := &quotactlXFS{llamar: limiteXFSQuotactl}
		a.limitar = func(d, overlay string, bytes int64) error {
			id := ids.asignarFichero(d, filepath.Base(overlay))
			ok := false
			defer func() {
				if !ok {
					ids.soltar(d, id)
				}
			}()
			// Primero el límite, luego el proyecto: si el fichero no cupiera, el
			// cambio de proyecto falla y la instancia no entra al almacén. Por
			// quotactl; si falla (o no hay dispositivo), el proceso xfs_quota.
			devOnce.Do(func() {
				if mt, found := montajeDe(a.dir); found && strings.HasPrefix(mt.fuente, "/dev/") {
					qc.dev = mt.fuente
				}
			})
			hecho := qc.fijar(id, bytes)
			if !hecho {
				bin := buscarE2fs("xfs_quota")
				if bin == "" {
					return errors.New("xfs_quota not found")
				}
				if err := ejecutar(bin, argsLimiteXFS(id, bytes, a.dir)...); err != nil {
					return err
				}
			}
			f, err := abrirSinSeguir(overlay)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := fijarProyecto(f, id); err != nil {
				return fmt.Errorf("setting the project of %s: %w", overlay, err)
			}
			ok = true
			return nil
		}
	case "btrfs":
		a.detectarCuota = func(context.Context) string {
			bin := buscarE2fs("btrfs")
			if bin == "" {
				return ""
			}
			if ejecutar(bin, "qgroup", "show", a.dir) != nil {
				// Sin cuotas activadas: se activan (el almacén es nuestro).
				if ejecutar(bin, "quota", "enable", a.dir) != nil || ejecutar(bin, "qgroup", "show", a.dir) != nil {
					return ""
				}
			}
			return "qgroup"
		}
		a.crearDir = func(d string) error {
			if _, err := os.Lstat(d); err == nil {
				return fs.ErrExist
			}
			bin := buscarE2fs("btrfs")
			if bin == "" {
				return errors.New("btrfs not found")
			}
			if err := ejecutar(bin, "subvolume", "create", d); err != nil {
				return err
			}
			return os.Chmod(d, 0o700)
		}
		a.limitar = func(d, _ string, bytes int64) error {
			bin := buscarE2fs("btrfs")
			if bin == "" {
				return errors.New("btrfs not found")
			}
			return ejecutar(bin, "qgroup", "limit", strconv.FormatInt(bytes, 10), d)
		}
		a.quitarDir = func(d string) error {
			bin := buscarE2fs("btrfs")
			if bin == "" {
				return errors.New("btrfs not found")
			}
			return ejecutar(bin, "subvolume", "delete", d)
		}
	}
}
