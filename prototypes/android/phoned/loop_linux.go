package main

// /data en RAM (ANDROID_DATA_MODE=tmpfs): un ext4 disperso copiado a un tmpfs
// y montado por un dispositivo loop. No un tmpfs a secas: PackageManager guarda
// el serial del usuario en un xattr "user.*" de /data/user/0 y tmpfs no los
// admite hasta Linux 6.6 (README, fallo 4). Loop por ioctl, sin losetup.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"
)

const (
	loopCtlGetFree = 0x4C82
	loopSetFd      = 0x4C00
	loopSetStatus  = 0x4C04 // LOOP_SET_STATUS64
	loopClrFd      = 0x4C01
	loFlagsAutoCl  = 4

	seekData = 3
	seekHole = 4
)

// loopInfo64 es struct loop_info64 (linux/loop.h), 232 bytes.
type loopInfo64 struct {
	Device         uint64
	Inode          uint64
	Rdevice        uint64
	Offset         uint64
	SizeLimit      uint64
	Number         uint32
	EncryptType    uint32
	EncryptKeySize uint32
	Flags          uint32
	FileName       [64]byte
	CryptName      [64]byte
	EncryptKey     [32]byte
	Init           [2]uint64
}

func ioctl(fd uintptr, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

// attachLoop engancha file a un loop libre con autoborrado (se suelta solo al
// desmontar) y devuelve /dev/loopN.
func attachLoop(file string) (string, error) {
	ctl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("loop-control: %w", err)
	}
	defer ctl.Close()
	f, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for try := 0; try < 8; try++ {
		n, _, e := syscall.Syscall(syscall.SYS_IOCTL, ctl.Fd(), loopCtlGetFree, 0)
		if e != 0 {
			return "", fmt.Errorf("LOOP_CTL_GET_FREE: %w", e)
		}
		dev := fmt.Sprintf("/dev/loop%d", n)
		if _, err := os.Stat(dev); errors.Is(err, os.ErrNotExist) {
			_ = syscall.Mknod(dev, syscall.S_IFBLK|0o660, int(7<<8|n&0xff|(n&^0xff)<<12))
		}
		l, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err != nil {
			return "", err
		}
		if err := ioctl(l.Fd(), loopSetFd, f.Fd()); err != nil {
			l.Close()
			if errors.Is(err, syscall.EBUSY) {
				continue // otro lo cogió entre medias
			}
			return "", fmt.Errorf("LOOP_SET_FD: %w", err)
		}
		var info loopInfo64
		info.Flags = loFlagsAutoCl
		copy(info.FileName[:], file)
		if err := ioctl(l.Fd(), loopSetStatus, uintptr(unsafe.Pointer(&info))); err != nil {
			_ = ioctl(l.Fd(), loopClrFd, 0)
			l.Close()
			return "", fmt.Errorf("LOOP_SET_STATUS64: %w", err)
		}
		l.Close()
		return dev, nil
	}
	return "", errors.New("no free loop device")
}

// copySparse copia src en dst sin rellenar los huecos (SEEK_DATA/SEEK_HOLE):
// un ext4 de 2 GiB casi vacío ocupa en el tmpfs lo que sus metadatos.
func copySparse(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	size := st.Size()
	var off int64
	for off < size {
		d, err := syscall.Seek(int(in.Fd()), off, seekData)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) {
				break // solo hueco hasta el final
			}
			return fmt.Errorf("SEEK_DATA: %w", err)
		}
		h, err := syscall.Seek(int(in.Fd()), d, seekHole)
		if err != nil {
			return fmt.Errorf("SEEK_HOLE: %w", err)
		}
		if _, err := in.Seek(d, io.SeekStart); err != nil {
			return err
		}
		if _, err := out.Seek(d, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(out, in, h-d); err != nil {
			return err
		}
		off = h
	}
	return out.Truncate(size)
}
