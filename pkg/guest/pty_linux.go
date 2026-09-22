//go:build linux

package guest

// Pseudoterminales con la biblioteca estándar.
//
// El agente se compila con CGO_ENABLED=0, así que no hay posix_openpt(),
// grantpt() ni unlockpt() de libc: todo son open() e ioctl() con constantes que
// el paquete syscall ya exporta para linux/amd64 y linux/arm64. Es el único
// sitio del proyecto que usa unsafe, y está aquí por eso.

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

type winsize struct{ Rows, Cols, X, Y uint16 }

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// ensureDevpts monta /dev/pts si falta.
//
// Hace falta de verdad: el init mínimo de las imágenes de kindling monta proc,
// sysfs, devtmpfs, /tmp y /run, y NUNCA devpts (scripts/minimal-init.sh). Sin él
// /dev/ptmx existe —lo crea devtmpfs— pero no entrega esclavo, así que abrir un
// PTY falla con un error que no señala a ninguna parte. Montarlo aquí evita
// tener que reconstruir todas las imágenes ya existentes.
//
// Idempotente: en una imagen con systemd ya está montado y esto no hace nada.
func ensureDevpts() error {
	if _, err := os.Stat("/dev/pts/ptmx"); err == nil {
		return nil
	}
	if err := os.MkdirAll("/dev/pts", 0o755); err != nil {
		return fmt.Errorf("creating /dev/pts: %w", err)
	}
	err := syscall.Mount("devpts", "/dev/pts", "devpts", 0, "ptmxmode=0666,mode=0620")
	if err != nil && err != syscall.EBUSY {
		return fmt.Errorf("mounting devpts: %w", err)
	}
	return nil
}

// openPTY devuelve el maestro y el esclavo de un pseudoterminal nuevo, con el
// tamaño pedido.
func openPTY(rows, cols uint16) (master, slave *os.File, err error) {
	// err ya viene declarado en la firma; el bucle de abajo lo reutiliza.
	if err := ensureDevpts(); err != nil {
		return nil, nil, err
	}
	// /dev/ptmx primero, /dev/pts/ptmx después.
	//
	// El orden importa y costó un test: en una distribución normal el nodo de
	// dentro de devpts está en modo 0000 y solo el de fuera tiene permisos, así
	// que abrir el de dentro falla con "permission denied". En una microVM de
	// kindling, en cambio, /dev lo crea devtmpfs y devpts lo montamos nosotros
	// con ptmxmode=0666, así que valen los dos. Se prueban ambos para funcionar
	// en las dos clases de invitado sin preguntar cuál es.
	var mfd int
	for _, ruta := range []string{"/dev/ptmx", "/dev/pts/ptmx"} {
		mfd, err = syscall.Open(ruta, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("opening the pty multiplexer: %w", err)
	}
	fallo := func(err error) (*os.File, *os.File, error) {
		_ = syscall.Close(mfd)
		return nil, nil, err
	}
	var n uint32
	if err := ioctl(uintptr(mfd), syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		return fallo(fmt.Errorf("asking for the pty number: %w", err))
	}
	var unlock int32 // 0 = desbloquear el esclavo
	if err := ioctl(uintptr(mfd), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		return fallo(fmt.Errorf("unlocking the pty: %w", err))
	}
	ws := winsize{Rows: rows, Cols: cols}
	if err := ioctl(uintptr(mfd), syscall.TIOCSWINSZ, unsafe.Pointer(&ws)); err != nil {
		return fallo(fmt.Errorf("setting the window size: %w", err))
	}
	// No bloqueante ANTES de envolverlo: os.NewFile solo registra en el poller
	// un descriptor no bloqueante, y sin poller un Read pendiente no se
	// desbloquea al cerrar el fichero y la goroutine se queda colgada.
	if err := syscall.SetNonblock(mfd, true); err != nil {
		return fallo(fmt.Errorf("setting O_NONBLOCK: %w", err))
	}
	master = os.NewFile(uintptr(mfd), "/dev/pts/ptmx")

	name := "/dev/pts/" + strconv.Itoa(int(n))
	slave, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("opening %s: %w", name, err)
	}
	return master, slave, nil
}

// resizePTY cambia el tamaño de la terminal y avisa al proceso de dentro
// (el kernel manda SIGWINCH).
//
// Con SyscallConn y no con Fd(): Fd() saca el fichero del poller y lo deja
// bloqueante para siempre, lo que rompería el Read del bucle de salida.
func resizePTY(master *os.File, rows, cols uint16) error {
	rc, err := master.SyscallConn()
	if err != nil {
		return err
	}
	ws := winsize{Rows: rows, Cols: cols}
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		ierr = ioctl(fd, syscall.TIOCSWINSZ, unsafe.Pointer(&ws))
	}); err != nil {
		return err
	}
	return ierr
}

// foregroundPgrp es el grupo de procesos en primer plano de la terminal: a quien
// hay que mandarle una señal pedida por trama. No es el proceso de la shell: si
// la shell lanzó `vim`, la señal es para vim.
func foregroundPgrp(master *os.File) (int, error) {
	rc, err := master.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pgrp int32
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		ierr = ioctl(fd, syscall.TIOCGPGRP, unsafe.Pointer(&pgrp))
	}); err != nil {
		return 0, err
	}
	if ierr != nil {
		return 0, ierr
	}
	return int(pgrp), nil
}

// ptySupported dice si este invitado puede abrir pseudoterminales.
func ptySupported() error { return ensureDevpts() }
