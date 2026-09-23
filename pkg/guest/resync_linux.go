//go:build linux

package guest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// Números de ioctl de <linux/random.h>, con la codificación genérica que usan
// amd64 y arm64: RNDADDENTROPY = _IOW('R', 0x03, int[2]); RNDRESEEDCRNG =
// _IO('R', 0x07).
const (
	rndAddEntropy = 0x40085203
	rndReseedCRNG = 0x5207
)

// setClockOS pone el reloj de pared (CLOCK_REALTIME). El monotónico no se toca:
// es el que usan los temporizadores, y saltar el de pared no los afecta.
func setClockOS(t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Settimeofday(&tv)
}

// mixEntropyOS mete b en el pool del kernel ACREDITÁNDOLO y fuerza a que el
// CRNG se resiembre ya.
//
// Escribir en /dev/urandom sin más mezcla pero no acredita ni resiembra: el
// CRNG seguiría sacando la secuencia copiada del snapshot hasta su próxima
// resiembra periódica, minutos después. RNDADDENTROPY acredita y RNDRESEEDCRNG
// (Linux 4.17+) fuerza la resiembra; en kernels 5.18+ además sube la
// generación, y cada CRNG por CPU —y el getrandom del vDSO— se rehace.
func mixEntropyOS(b []byte) error {
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	// entropy_count en bits, buf_size en bytes. El búfer se reserva en uint32
	// para que quede alineado como el kernel espera.
	words := make([]uint32, 2+(len(b)+3)/4)
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*4)
	binary.NativeEndian.PutUint32(raw[0:], uint32(len(b)*8))
	binary.NativeEndian.PutUint32(raw[4:], uint32(len(b)))
	copy(raw[8:], b)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), rndAddEntropy,
		uintptr(unsafe.Pointer(&words[0]))); e != 0 {
		return fmt.Errorf("RNDADDENTROPY: %w", e)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), rndReseedCRNG, 0); e != 0 {
		// Kernel anterior a 4.17: la entropía ya está mezclada y acreditada, y
		// el CRNG la tomará en su próxima resiembra. Peor que ahora, pero no es
		// motivo para fallar la restauración.
		if errors.Is(e, syscall.ENOTTY) || errors.Is(e, syscall.EINVAL) {
			log.Printf("resync: this kernel has no RNDRESEEDCRNG (%v); the RNG reseeds on its own schedule", e)
			return nil
		}
		return fmt.Errorf("RNDRESEEDCRNG: %w", e)
	}
	return nil
}
