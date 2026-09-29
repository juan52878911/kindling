//go:build darwin

package footprint

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// El freno del tope de CPU: parar (SIGSTOP) y reanudar (SIGCONT) el auxiliar
// de Apple entero, con sus vCPU.
//
// Por qué señales y no pausar la VM por el framework: con una pausa del
// framework el reloj del invitado se para con ella, y un invitado regulado
// se queda atrás (medido en un M4: 2 vCPU a tope con techo 50, 23 s de
// retraso en 30 s). Con SIGSTOP el invitado ve pasar el tiempo parado, como
// bajo cpu.max en Linux, y la parada cuesta microsegundos.
//
// Por qué un proceso aparte: el perfil de sandbox de kling-vz no deja mandar
// señales a otros procesos, y el filtro de la regla signal solo distingue
// "self" de "others" (probado: signing-identifier y process-path no
// restringen nada). Abrirle signal a others dejaría a un kling-vz tomado por
// un invitado hostil parar o matar cualquier proceso del usuario. Así que las
// manda este proceso pequeño, que kling-vz lanza antes de encerrarse y que se
// encierra a sí mismo (ServeFreno): no abre ficheros ni red, y solo señaliza
// al auxiliar que tiene abierta la tubería de consola que kling-vz le pasa
// por SCM_RIGHTS. kling-vz solo puede pasarle tuberías que tiene, así que no
// puede apuntarlo a otra VM. Si kling-vz muere, el freno ve cerrarse el
// socket, reanuda el auxiliar y termina: nunca queda una VM parada sin dueño.

// Órdenes del protocolo: un byte, y la respuesta es otro (0 = hecho).
const (
	frenoVM     = 'T' // con la tubería de consola adjunta
	frenoParar  = 'S'
	frenoSoltar = 'C'
)

// Freno es el lado de kling-vz.
type Freno struct {
	mu sync.Mutex
	c  *net.UnixConn
}

// StartFreno lanza el proceso freno: este mismo ejecutable con args (el
// modo freno de main). Hay que llamarlo antes de encerrar kling-vz.
func StartFreno(args ...string) (*Freno, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	mio, suyo := os.NewFile(uintptr(fds[0]), "freno"), os.NewFile(uintptr(fds[1]), "freno")
	defer suyo.Close()
	syscall.CloseOnExec(fds[0])
	cmd := exec.Command(exe, args...)
	cmd.ExtraFiles = []*os.File{suyo} // fd 3 en el hijo
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		mio.Close()
		return nil, err
	}
	go func() { _ = cmd.Wait() }()
	c, err := net.FileConn(mio)
	mio.Close()
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	return &Freno{c: c.(*net.UnixConn)}, nil
}

func (f *Freno) orden(op byte, rights []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, _, err := f.c.WriteMsgUnix([]byte{op}, rights, nil); err != nil {
		return fmt.Errorf("CPU brake process: %w", err)
	}
	var r [1]byte
	if _, err := f.c.Read(r[:]); err != nil {
		return fmt.Errorf("CPU brake process: %w", err)
	}
	if r[0] != 0 {
		return errors.New("the CPU brake process could not signal the VM's helper")
	}
	return nil
}

// Track le pasa al freno la tubería de consola de la VM recién creada.
func (f *Freno) Track(fd uintptr) error {
	return f.orden(frenoVM, syscall.UnixRights(int(fd)))
}

// Freeze para (true) o reanuda (false) el auxiliar de la VM.
func (f *Freno) Freeze(stop bool) error {
	if stop {
		return f.orden(frenoParar, nil)
	}
	return f.orden(frenoSoltar, nil)
}

// ServeFreno es el proceso freno: atiende las órdenes que llegan por el
// socket fd hasta que se cierra. Devuelve el código de salida.
func ServeFreno(fd int) int {
	c, err := net.FileConn(os.NewFile(uintptr(fd), "freno"))
	if err != nil {
		return 2
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 2
	}
	var (
		anchor           uint64
		auxiliar, parado int
		b                = make([]byte, 1)
		oob              = make([]byte, syscall.CmsgSpace(4*4))
	)
	soltar := func() bool {
		if parado == 0 {
			return true
		}
		err := syscall.Kill(parado, syscall.SIGCONT)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			parado = 0
			return true
		}
		return false
	}
	for {
		n, oobn, _, _, err := uc.ReadMsgUnix(b, oob)
		if err != nil || n == 0 {
			soltar()
			return 0
		}
		ok := false
		switch b[0] {
		case frenoVM:
			// Una VM nueva: la anterior, si seguía parada, se suelta.
			soltar()
			anchor, auxiliar = 0, 0
			if msgs, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil {
				for _, m := range msgs {
					fds, err := syscall.ParseUnixRights(&m)
					if err != nil {
						continue
					}
					for _, f := range fds {
						if h, bien := pipeHandle(uintptr(f)); bien && anchor == 0 {
							anchor, ok = h, true
						}
						// No se guarda: tener abierta la tubería cambiaría
						// cuándo ve su final quien lee la consola.
						syscall.Close(f)
					}
				}
			}
		case frenoParar:
			if auxiliar = buscarAuxiliar(anchor, auxiliar); auxiliar > 0 {
				if soltar() && syscall.Kill(auxiliar, syscall.SIGSTOP) == nil {
					parado, ok = auxiliar, true
				}
			}
		case frenoSoltar:
			ok = soltar()
		}
		r := byte(1)
		if ok {
			r = 0
		}
		if _, err := uc.Write([]byte{r}); err != nil {
			soltar()
			return 0
		}
	}
}
