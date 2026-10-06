//go:build linux

package main

import (
	"fmt"
	"syscall"
)

// Topes de un constructor sin root, que se pone él mismo antes de leer la
// petición: lo que viene después (registros, tars) es de fuera. Bajar un
// límite no pide privilegios y no se puede deshacer.
const (
	prSetNoNewPrivs = 38
	rlimitNproc     = 6 // RLIMIT_NPROC en amd64 y arm64; syscall no lo exporta

	// constructorMemoria acota el montón de Go (RLIMIT_DATA cuenta las
	// páginas anónimas escribibles): el árbol de una imagen grande ocupa
	// cientos de MiB, no esto.
	constructorMemoria  = 8 << 30
	constructorFicheros = 4096
	// Por usuario, no por proceso: cuenta los hilos de Go y los de cualquier
	// otro proceso del usuario de construcción, que no debería haber.
	constructorProcesos = 512
)

func limitarConstructor() error {
	// no_new_privs en todos los hilos: ningún setuid del host le devuelve
	// privilegios. Con cgo AllThreadsSyscall no vale; entonces, en éste.
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); e == syscall.ENOTSUP {
		if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); e != 0 {
			return fmt.Errorf("no_new_privs: %v", e)
		}
	} else if e != 0 {
		return fmt.Errorf("no_new_privs: %v", e)
	}
	for _, l := range []struct {
		r int
		v uint64
	}{
		{syscall.RLIMIT_CORE, 0}, // sin volcados con lo que haya en memoria
		{syscall.RLIMIT_NOFILE, constructorFicheros},
		{syscall.RLIMIT_DATA, constructorMemoria},
		{rlimitNproc, constructorProcesos},
	} {
		var cur syscall.Rlimit
		if err := syscall.Getrlimit(l.r, &cur); err != nil {
			return fmt.Errorf("rlimit %d: %w", l.r, err)
		}
		v := min(l.v, cur.Max) // sin root no se sube un tope
		if err := syscall.Setrlimit(l.r, &syscall.Rlimit{Cur: v, Max: v}); err != nil {
			return fmt.Errorf("rlimit %d: %w", l.r, err)
		}
	}
	return nil
}
