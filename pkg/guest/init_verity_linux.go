//go:build linux

package guest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// dmControl es el nodo de control de device-mapper. Con device-mapper en el
// kernel, devtmpfs lo crea (mapper/control); si no está, se crea con el menor
// que dice /proc/misc, como hace dmsetup.
const dmControl = "/dev/mapper/control"

// DMCreate es "dmsetup create name --readonly --table ..." sin udev: crea el
// dispositivo, carga la tabla de solo lectura, lo reanuda (que es cuando la
// tabla entra en vigor) y deja su nodo en /dev/mapper/name.
func (linuxInit) DMCreate(name string, targets []dmTarget) (string, error) {
	fd, err := abrirDMControl()
	if err != nil {
		return "", err
	}
	defer syscall.Close(fd)

	b, err := dmHeader(name, 0, 0, 0)
	if err != nil {
		return "", err
	}
	if err := dmIoctl(fd, dmDevCreate, b); err != nil {
		return "", fmt.Errorf("DM_DEV_CREATE %s: %w", name, err)
	}
	dev := binary.NativeEndian.Uint64(b[dmDevOffset:])
	// Lo creado a medias se quita: sin la tabla buena el dispositivo no
	// sirve, y el init se para de todos modos.
	quitar := func() {
		if r, err := dmHeader(name, 0, 0, 0); err == nil {
			_ = dmIoctl(fd, dmDevRemove, r)
		}
	}
	load, err := dmTableLoadBuf(name, targets)
	if err != nil {
		quitar()
		return "", err
	}
	if err := dmIoctl(fd, dmTableLoad, load); err != nil {
		quitar()
		return "", fmt.Errorf("DM_TABLE_LOAD %s: %w", name, err)
	}
	// DM_DEV_SUSPEND sin DM_SUSPEND_FLAG reanuda: la tabla cargada pasa a ser
	// la activa.
	r, _ := dmHeader(name, 0, 0, 0)
	if err := dmIoctl(fd, dmDevSuspend, r); err != nil {
		quitar()
		return "", fmt.Errorf("DM_DEV_SUSPEND (resume) %s: %w", name, err)
	}
	major, minor := dmDecodeDev(dev)
	p := "/dev/mapper/" + name
	if err := os.MkdirAll("/dev/mapper", 0o755); err != nil {
		return "", err
	}
	err = syscall.Mknod(p, syscall.S_IFBLK|0o600, int(major<<8|minor&0xff|(minor&^0xff)<<12))
	if err != nil && !errors.Is(err, syscall.EEXIST) {
		return "", fmt.Errorf("mknod %s: %w", p, err)
	}
	return p, nil
}

func dmIoctl(fd int, req uintptr, b []byte) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(&b[0])))
	if e != 0 {
		return e
	}
	return nil
}

func abrirDMControl() (int, error) {
	fd, err := syscall.Open(dmControl, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err == nil || !errors.Is(err, syscall.ENOENT) {
		if err != nil {
			return -1, fmt.Errorf("opening %s: %w", dmControl, err)
		}
		return fd, nil
	}
	minor, merr := dmMiscMinor()
	if merr != nil {
		return -1, fmt.Errorf("opening %s: %w (and %v)", dmControl, err, merr)
	}
	_ = os.MkdirAll("/dev/mapper", 0o755)
	if err := syscall.Mknod(dmControl, syscall.S_IFCHR|0o600, 10<<8|minor); err != nil && !errors.Is(err, syscall.EEXIST) {
		return -1, fmt.Errorf("mknod %s: %w", dmControl, err)
	}
	fd, err = syscall.Open(dmControl, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("opening %s: %w", dmControl, err)
	}
	return fd, nil
}

// dmMiscMinor es el menor de device-mapper en /proc/misc ("236 device-mapper").
func dmMiscMinor() (int, error) {
	b, err := os.ReadFile("/proc/misc")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == "device-mapper" {
			if n, err := strconv.Atoi(f[0]); err == nil && n >= 0 && n < 256 {
				return n, nil
			}
		}
	}
	return 0, errors.New("the kernel has no device-mapper (not in /proc/misc)")
}
