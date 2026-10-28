//go:build darwin

package footprint

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
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
// Al revés, si muere el freno (un SIGKILL; a las demás señales suelta antes de
// salir), kling-vz lo ve en el socket (ErrFrenoCaido): si el auxiliar estaba
// parado, nadie puede soltarlo, y kling-vz termina para que el sistema se
// lleve la VM; si no, sigue con el tope por pausas del framework.

// Órdenes del protocolo: un byte, y la respuesta es otro (0 = hecho).
const (
	frenoVM     = 'T' // con la tubería de consola adjunta
	frenoParar  = 'S'
	frenoSoltar = 'C'
)

// ErrFrenoCaido: el proceso freno ya no contesta (murió o cerró el socket).
// Si tenía el auxiliar parado, se queda parado: kling-vz no puede mandar
// señales (ver arriba), así que quien reciba este error decide qué hacer con
// la VM (server.soltarFreno termina el proceso).
var ErrFrenoCaido = errors.New("the CPU brake process is gone")

// Freno es el lado de kling-vz.
type Freno struct {
	mu     sync.Mutex
	c      *net.UnixConn
	muerto bool // con mu: el socket dio EOF/EPIPE; no se vuelve a intentar
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
	// Su propio grupo de procesos: un Ctrl-C en la terminal (o una señal al
	// grupo) llega a kling-vz y no mata al freno a mitad de una parada; el
	// freno termina cuando kling-vz cierra el socket, soltando antes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	if f.muerto {
		return ErrFrenoCaido
	}
	if _, _, err := f.c.WriteMsgUnix([]byte{op}, rights, nil); err != nil {
		return f.fallo(err)
	}
	var r [1]byte
	if _, err := f.c.Read(r[:]); err != nil {
		return f.fallo(err)
	}
	if r[0] != 0 {
		return errors.New("the CPU brake process could not signal the VM's helper")
	}
	return nil
}

// fallo traduce un error del socket. Uno de proceso muerto (EOF, EPIPE,
// ECONNRESET) es para siempre: se marca y se devuelve ErrFrenoCaido. Con
// f.mu tomado.
func (f *Freno) fallo(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed) {
		f.muerto = true
		return fmt.Errorf("%w: %v", ErrFrenoCaido, err)
	}
	return fmt.Errorf("CPU brake process: %w", err)
}

// Track le pasa al freno la tubería de consola de la VM recién creada.
func (f *Freno) Track(fd uintptr) error {
	return f.orden(frenoVM, syscall.UnixRights(int(fd)))
}

// Close suelta el proceso freno: ve cerrarse el socket, reanuda el auxiliar
// si lo tenía parado y termina. Después, Track y Freeze dan ErrFrenoCaido.
func (f *Freno) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.muerto {
		return nil
	}
	f.muerto = true
	return f.c.Close()
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
		// mu protege parado frente a la goroutine de las señales.
		mu sync.Mutex
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
	// Una señal que lo terminaría (SIGTERM de quien limpia procesos, SIGHUP
	// al cerrar la terminal) no debe dejar el auxiliar parado: se suelta y se
	// sale. SIGKILL no se puede atender; para eso, kling-vz (ErrFrenoCaido).
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	go func() {
		<-sigs
		mu.Lock()
		soltar()
		os.Exit(0)
	}()
	for {
		n, oobn, _, _, err := uc.ReadMsgUnix(b, oob)
		mu.Lock()
		if err != nil || n == 0 {
			soltar()
			mu.Unlock()
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
			mu.Unlock()
			return 0
		}
		mu.Unlock()
	}
}
