//go:build linux

package guest

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"

	"github.com/juan52878911/kindling/pkg/share"
)

// fuseDevNum es el número de /dev/fuse (misc, 10:229).
const fuseDevNum = 10<<8 | 229

// mountFUSE abre /dev/fuse y monta el sistema de ficheros en target.
//
// allow_other para que lo vean los usuarios no root del invitado, y
// default_permissions para que el kernel compruebe los permisos con los modos
// que damos (sin él, cualquier usuario escribiría en todo). nosuid y nodev
// siempre: nada de lo que venga del host debe poder elevar privilegios dentro.
func mountFUSE(target string, readOnly bool) (fuseDev, func(), error) {
	if _, err := os.Stat("/dev/fuse"); errors.Is(err, os.ErrNotExist) {
		// Un /dev sin el nodo (devtmpfs a medias): se crea. Si el kernel no
		// tiene FUSE, abrirlo dirá ENODEV justo después.
		_ = syscall.Mknod("/dev/fuse", syscall.S_IFCHR|0o666, fuseDevNum)
	}
	fd, err := syscall.Open("/dev/fuse", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.ENOENT) {
			return nil, nil, fmt.Errorf("%w (/dev/fuse: %v)", errNoFUSE, err)
		}
		return nil, nil, fmt.Errorf("opening /dev/fuse: %w", err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		syscall.Close(fd)
		return nil, nil, err
	}
	opts := fmt.Sprintf("fd=%d,rootmode=40000,user_id=0,group_id=0,allow_other,default_permissions,max_read=%d",
		fd, share.MaxIO)
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV)
	if readOnly {
		flags |= syscall.MS_RDONLY
	}
	if err := syscall.Mount("kling-share", target, "fuse.kling", flags, opts); err != nil {
		syscall.Close(fd)
		if errors.Is(err, syscall.ENODEV) {
			return nil, nil, fmt.Errorf("%w (mount: %v)", errNoFUSE, err)
		}
		return nil, nil, fmt.Errorf("mount: %w", err)
	}
	unmount := func() {
		if err := syscall.Unmount(target, syscall.MNT_DETACH); err != nil {
			log.Printf("share: could not unmount %s: %v", target, err)
		}
	}
	return fdDev(fd), unmount, nil
}

// readaheadKB es la lectura adelantada de una carpeta viva. El kernel le da a
// un montaje FUSE la de cualquier dispositivo (128 KiB) por mucho que INIT
// pida más, y con eso una lectura secuencial va a un viaje al host por cada
// 128 KiB. Con 4 MiB salen hasta cuatro lecturas de 1 MiB (share.MaxIO) a la
// vez.
const readaheadKB = 4096

// setReadahead sube la lectura adelantada del montaje en target por sysfs
// (/sys/class/bdi/<major>:<minor>/read_ahead_kb). El número se saca de
// /proc/self/mountinfo y no con stat, que sería una petición al propio
// montaje. Va DESPUÉS de contestar INIT: al procesar la respuesta el kernel
// recorta la del montaje a la que diga INIT, que no puede pasar de la que
// ofreció (128 KiB). Si algo falla se queda la del kernel, que funciona igual,
// más despacio.
func setReadahead(target string) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	dev := ""
	for sc.Scan() {
		// id padre major:minor raíz punto-de-montaje ...
		fl := strings.Fields(sc.Text())
		if len(fl) >= 5 && fl[4] == target {
			dev = fl[2] // el último gana: es el montaje que acabamos de hacer
		}
	}
	if dev == "" {
		return
	}
	_ = os.WriteFile("/sys/class/bdi/"+dev+"/read_ahead_kb", []byte(fmt.Sprint(readaheadKB)), 0o644)
}

// fdDev es /dev/fuse de verdad. Lecturas bloqueantes a propósito: cada una
// ocupa un hilo mientras espera, y hay un solo lector por montaje.
type fdDev int

func (d fdDev) Read(buf []byte) (int, error) {
	n, err := syscall.Read(int(d), buf)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (d fdDev) Write(msg []byte) (int, error) { return syscall.Write(int(d), msg) }
