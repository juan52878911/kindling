package durable

import (
	"errors"
	"os"
	"syscall"
)

// fBarrierFsync es F_BARRIERFSYNC de <sys/fcntl.h>: el paquete syscall de Go
// 1.24 no la trae.
const fBarrierFsync = 0x55

// Ordenar lleva al disco lo escrito en f y pone una barrera: nada de lo que se
// escriba después (un rename, el sello) llega al SSD antes que esto. No vacía
// la caché del SSD: lo que la vacía, y con ella todo lo anterior a la
// barrera, es el F_FULLFSYNC (File.Sync) con el que acaba cada escritura
// durable. Es lo que hace falta entre el contenido y el rename que lo publica,
// a ~0,7 ms en vez de los ~5 ms de un F_FULLFSYNC (medido en el M4).
//
// Donde no se admite (un volumen de red, ENOTSUP, o cualquier otro fallo de
// la barrera), un Sync entero: mejor pagar de más que no ordenar.
func Ordenar(f *os.File) error {
	_, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), fBarrierFsync, 0)
	if e == 0 {
		return nil
	}
	if errors.Is(e, syscall.EINTR) {
		if _, _, e = syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), fBarrierFsync, 0); e == 0 {
			return nil
		}
	}
	return f.Sync()
}
