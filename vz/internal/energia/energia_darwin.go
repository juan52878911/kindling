//go:build darwin

// Package energia avisa al daemon de que el Mac se va a dormir, para que
// pause las VM antes, y de que despertó.
//
// El núcleo no usa cgo, y sin cgo no hay forma de enterarse ANTES de dormir:
// el reloj de pared contra el monotónico (internal/machine/despertar_host.go)
// solo lo dice al despertar, y kern.sleeptime/kern.waketime son la hora del
// último sueño, también a posteriori. IORegisterForSystemPower sí: macOS
// espera a que quien se registró conteste (IOAllowPowerChange), hasta ~30 s,
// antes de dormir. Así que lo hace el binario del VMM, que ya es cgo, en un
// modo aparte (`kling-vz --power-events`) que el daemon lanza y con el que
// habla por stdin/stdout:
//
//	VMM -> daemon   "sleep <id>"   el Mac se va a dormir
//	daemon -> VMM   "ack <id>"     ya están pausadas: que duerma
//	VMM -> daemon   "wake"         despertó
//
// Sin respuesta en PlazoAck se deja dormir igual: nunca se retrasa el sueño
// más que eso. El sueño por inactividad, que pregunta antes, no se veta.
package energia

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
int energiaRegistrar(void);
void energiaCorrer(void);
void energiaPermitir(long id);
*/
import "C"

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PlazoAck es cuánto se espera al daemon antes de dejar dormir al Mac.
const PlazoAck = 10 * time.Second

// Arg es el argumento con el que el binario del VMM hace de vigía.
const Arg = "--power-events"

type evento struct {
	dormir bool
	id     int64
}

var eventos = make(chan evento, 16)

//export energiaEvento
func energiaEvento(tipo C.int, id C.long) {
	eventos <- evento{dormir: tipo == 1, id: int64(id)}
}

// Perfil es el sandbox del vigía: abrir IOPMrootDomain y nada más (ni
// ficheros, ni red, ni procesos).
const Perfil = `(version 1)
(deny default)
(import "system.sb")
(allow iokit-open (iokit-user-client-class "RootDomainUserClient"))
`

// Main es el modo vigía del binario del VMM: se encierra con aplicar
// (sandbox.Aplicar) en Perfil y sirve por stdin/stdout.
func Main(aplicar func(perfil string, pares []string) error) int {
	signal.Ignore(syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	if err := aplicar(Perfil, nil); err != nil {
		fmt.Fprintln(os.Stderr, "power events: sandbox:", err)
		return 1
	}
	return Servir(os.Stdin, os.Stdout)
}

// Servir es el modo vigía: registra el aviso, lo reenvía por w y espera las
// respuestas por r hasta que r se cierra (el daemon murió). Devuelve el
// código de salida.
func Servir(r io.Reader, w io.Writer) int {
	listo := make(chan error, 1)
	go func() {
		// El aviso llega por el run loop del hilo que se registró.
		runtime.LockOSThread()
		if C.energiaRegistrar() != 0 {
			listo <- fmt.Errorf("IORegisterForSystemPower failed")
			return
		}
		listo <- nil
		C.energiaCorrer()
	}()
	if err := <-listo; err != nil {
		fmt.Fprintln(os.Stderr, "power events:", err)
		return 1
	}
	acks := make(chan int64, 16)
	cerrado := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if s, ok := strings.CutPrefix(sc.Text(), "ack "); ok {
				if id, err := strconv.ParseInt(s, 10, 64); err == nil {
					acks <- id
				}
			}
		}
		close(cerrado)
	}()
	fmt.Fprintln(w, "ready")
	for {
		select {
		case <-cerrado:
			return 0
		case e := <-eventos:
			if !e.dormir {
				fmt.Fprintln(w, "wake")
				continue
			}
			fmt.Fprintf(w, "sleep %d\n", e.id)
			esperarAck(e.id, acks, cerrado)
			C.energiaPermitir(C.long(e.id))
		}
	}
}

// esperarAck espera el ack de id, como mucho PlazoAck.
func esperarAck(id int64, acks <-chan int64, cerrado <-chan struct{}) {
	t := time.NewTimer(PlazoAck)
	defer t.Stop()
	for {
		select {
		case a := <-acks:
			if a == id {
				return
			}
		case <-t.C:
			return
		case <-cerrado:
			return
		}
	}
}
