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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// proyectoLibre es el primer id de proyecto desde primerProyecto que no está
// en usados.
func proyectoLibre(usados map[uint32]bool) uint32 {
	id := uint32(primerProyecto)
	for usados[id] {
		id++
	}
	return id
}

// proyectosEnUso recorre los overlays de las instancias del almacén y devuelve
// los ids de proyecto que ya tienen. Sin estado en memoria: sobrevive a un
// reinicio del daemon y libera el id de una instancia borrada sin llevar
// cuentas.
func proyectosEnUso(dirM string) map[uint32]bool {
	usados := map[uint32]bool{}
	entradas, err := os.ReadDir(dirM)
	if err != nil {
		return usados
	}
	for _, e := range entradas {
		f, err := abrirSinSeguir(filepath.Join(dirM, e.Name(), "overlay.ext4"))
		if err != nil {
			continue
		}
		if id, err := proyectoDe(f); err == nil && id != 0 {
			usados[id] = true
		}
		f.Close()
	}
	return usados
}

// ejecutar corre una herramienta del sistema con un tope de tiempo.
func ejecutar(bin string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", filepath.Base(bin), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
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
		a.limitar = func(d, overlay string, bytes int64) error {
			bin := buscarE2fs("xfs_quota")
			if bin == "" {
				return errors.New("xfs_quota not found")
			}
			id := proyectoLibre(proyectosEnUso(filepath.Dir(d)))
			// Primero el límite, luego el proyecto: si el fichero no cupiera, el
			// cambio de proyecto falla y la instancia no entra al almacén.
			if err := ejecutar(bin, argsLimiteXFS(id, bytes, a.dir)...); err != nil {
				return err
			}
			f, err := abrirSinSeguir(overlay)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := fijarProyecto(f, id); err != nil {
				return fmt.Errorf("setting the project of %s: %w", overlay, err)
			}
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
