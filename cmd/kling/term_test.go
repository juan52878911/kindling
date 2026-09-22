//go:build linux || darwin

package main

// Estas pruebas necesitan una terminal de verdad pero no la del que las lanza:
// se abre un pseudoterminal en /dev/ptmx. En Linux el lado maestro acepta los
// ioctl de termios; en macOS no, y hay que pedirle al maestro el nombre del
// esclavo y abrir ese. Si el entorno no lo permite (contenedor sin /dev/ptmx,
// sin permisos), se saltan en vez de fallar.

import (
	"os"
	"runtime"
	"syscall"
	"testing"
	"unsafe"
)

func openPTY(t *testing.T) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	if isTerminal(master.Fd()) {
		return master
	}
	if runtime.GOOS != "darwin" {
		t.Skip("/dev/ptmx does not answer termios ioctls here")
	}
	// Literales y no syscall.TIOCPTY*: este fichero compila también en Linux,
	// donde esos nombres no existen. Son TIOCPTYGRANT, TIOCPTYUNLK y
	// TIOCPTYGNAME de zerrors_darwin_*.go, lo que hace posix_openpt en libc.
	fd := master.Fd()
	for _, req := range []uintptr{0x20007454, 0x20007452} {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, 0); e != 0 {
			t.Skipf("pty grant/unlock: %v", e)
		}
	}
	var name [128]byte
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, 0x40807453, uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		t.Skipf("pty slave name: %v", e)
	}
	n := 0
	for n < len(name) && name[n] != 0 {
		n++
	}
	slave, err := os.OpenFile(string(name[:n]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open pty slave %q: %v", name[:n], err)
	}
	t.Cleanup(func() { slave.Close() })
	if !isTerminal(slave.Fd()) {
		t.Skip("the pty slave does not answer termios ioctls here")
	}
	return slave
}

func TestIsTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f.Fd()) {
		t.Error("a regular file is not a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r.Fd()) || isTerminal(w.Fd()) {
		t.Error("a pipe is not a terminal")
	}
	pty := openPTY(t)
	if !isTerminal(pty.Fd()) {
		t.Error("the pty master is a terminal")
	}
}

// makeRaw tiene que apagar lo que dice que apaga —sobre todo ISIG, que es lo
// que hace que Ctrl-C viaje como byte— y restore tiene que dejar la termios
// exactamente como estaba, aunque se llame dos veces.
func TestMakeRawAndRestore(t *testing.T) {
	pty := openPTY(t)
	fd := pty.Fd()

	before, err := termiosGet(fd)
	if err != nil {
		t.Fatal(err)
	}
	// Que el estado inicial tenga algo que apagar; si no, la prueba no prueba.
	cooked := before
	cooked.Lflag |= syscall.ISIG | syscall.ECHO | syscall.ICANON | syscall.IEXTEN
	cooked.Iflag |= syscall.ICRNL | syscall.IXON
	cooked.Oflag |= syscall.OPOST
	if err := termiosSet(fd, &cooked); err != nil {
		t.Skipf("cannot set termios on the pty: %v", err)
	}
	before, err = termiosGet(fd)
	if err != nil {
		t.Fatal(err)
	}

	restore, err := makeRaw(fd)
	if err != nil {
		t.Fatalf("makeRaw: %v", err)
	}
	raw, err := termiosGet(fd)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Lflag&(syscall.ISIG|syscall.ECHO|syscall.ICANON|syscall.IEXTEN) != 0 {
		t.Errorf("Lflag %#x still has ISIG/ECHO/ICANON/IEXTEN", raw.Lflag)
	}
	if raw.Iflag&(syscall.ICRNL|syscall.IXON) != 0 {
		t.Errorf("Iflag %#x still has ICRNL/IXON", raw.Iflag)
	}
	if raw.Oflag&syscall.OPOST != 0 {
		t.Errorf("Oflag %#x still has OPOST", raw.Oflag)
	}
	if raw.Cflag&syscall.CSIZE != syscall.CS8 || raw.Cflag&syscall.PARENB != 0 {
		t.Errorf("Cflag %#x is not CS8 without parity", raw.Cflag)
	}
	if raw.Cc[syscall.VMIN] != 1 || raw.Cc[syscall.VTIME] != 0 {
		t.Errorf("VMIN=%d VTIME=%d, want 1 and 0", raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME])
	}

	restore()
	restore() // idempotente: la segunda no debe romper nada
	after, err := termiosGet(fd)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("restore left the termios different:\n before %+v\n after  %+v", before, after)
	}
}

func TestMakeRawNotATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := makeRaw(r.Fd()); err == nil {
		t.Error("makeRaw on a pipe should fail")
	}
	if _, _, err := winsize(r.Fd()); err == nil {
		t.Error("winsize on a pipe should fail")
	}
}

// El maestro de un pty recién abierto suele decir 0x0; lo que se comprueba es
// que el ioctl funciona y que un 0x0 se reporta como error (el daemon pondrá
// 24x80) en vez de mandarse tal cual.
func TestWinsizePTY(t *testing.T) {
	pty := openPTY(t)
	rows, cols, err := winsize(pty.Fd())
	if err == nil && (rows == 0 || cols == 0) {
		t.Errorf("winsize reported %dx%d without error", rows, cols)
	}
}
