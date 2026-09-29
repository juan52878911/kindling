package main

// Estado de dm-verity de la capa de Android, por ioctl de device-mapper.
//
// Quien monta la capa detrás de dm-verity es el init de la base (el
// overlay-init que parchea image/verity.sh), ANTES de que exista PID 1: la
// capa es el rootfs de la VM, así que no hay forma de ponerla detrás de verity
// más tarde. Lo que hace kling-phoned es comprobar que la imagen que declara
// verity (la tabla en /etc/kindling-android/layer.verity) la tiene de verdad
// y que el kernel no ha visto bloques corruptos: el estado del objetivo
// verity es "V" (verificado) o "C" (corrupto). /v1/health lo enseña y no
// da el teléfono por sano si es "C" o falta.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	dmName       = "android-layer"
	dmIoctlSize  = 312 // sizeof(struct dm_ioctl)
	dmBufSize    = 16 << 10
	dmTargetSpec = 40 // sizeof(struct dm_target_spec)
)

// _IOWR(0xfd, 12, struct dm_ioctl): DM_TABLE_STATUS.
const dmTableStatus = 3<<30 | dmIoctlSize<<16 | 0xfd<<8 | 12

// verityStatus: "verified", "corrupted", "none" (la imagen no declara
// verity) o "missing"/"error: ..." (la declara y no está).
func verityStatus() string {
	if _, err := os.Stat(verityTable); err != nil {
		return "none"
	}
	st, err := dmTargetStatus(dmName)
	if err != nil {
		if errors.Is(err, syscall.ENXIO) {
			return "missing"
		}
		return "error: " + err.Error()
	}
	switch {
	case bytes.HasPrefix(st, []byte("V")):
		return "verified"
	case bytes.HasPrefix(st, []byte("C")):
		return "corrupted"
	}
	return "unknown: " + string(st)
}

// dmTargetStatus devuelve la cadena de estado del primer objetivo del
// dispositivo name (DM_TABLE_STATUS sin DM_STATUS_TABLE_FLAG).
func dmTargetStatus(name string) ([]byte, error) {
	ctl, err := os.OpenFile("/dev/mapper/control", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer ctl.Close()
	buf := make([]byte, dmBufSize)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], 4) // versión 4.0.0
	le.PutUint32(buf[12:], dmBufSize)
	le.PutUint32(buf[16:], dmIoctlSize) // data_start
	copy(buf[48:48+127], name)          // name[128] tras dev (u64 en 40)
	if err := ioctl(ctl.Fd(), dmTableStatus, uintptr(unsafe.Pointer(&buf[0]))); err != nil {
		return nil, err
	}
	targets := le.Uint32(buf[20:])
	start := le.Uint32(buf[16:])
	if targets == 0 {
		return nil, fmt.Errorf("%s has no targets", name)
	}
	if int(start)+dmTargetSpec > len(buf) {
		return nil, errors.New("short DM_TABLE_STATUS reply")
	}
	spec := buf[start:]
	typ := bytes.TrimRight(spec[24:40], "\x00")
	if string(typ) != "verity" {
		return nil, fmt.Errorf("%s is %q, not verity", name, typ)
	}
	s := spec[dmTargetSpec:]
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return s, nil
}
