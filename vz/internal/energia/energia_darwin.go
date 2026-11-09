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
//	VMM -> daemon   "power battery" | "power ac"   de dónde come: al
//	                               arrancar y en cada cambio (ac es también
//	                               un SAI: lo que no es la batería del Mac)
//
// Lo de la fuente ahorra al daemon preguntarla con pmset (un proceso por
// minuto): IOPSCreateLimitedPowerNotification avisa solo del cambio. Un daemon
// anterior ignora esas líneas, y uno nuevo con un vigía anterior sigue
// preguntando él.
//
// Sin respuesta en PlazoAck se deja dormir igual: nunca se retrasa el sueño
// más que eso. El sueño por inactividad, que pregunta antes, no se veta.
package energia

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
int energiaRegistrar(void);
int energiaFuente(void);
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
	"sync/atomic"
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

// fuente es la última lectura de la fuente (energiaFuente: 1 batería, 0
// corriente, -1 no se sabe) y fuentes avisa de que cambió. Aparte de
// eventos y sin bloquear: el aviso llega en el run loop, y mientras se espera
// un ack no debe atascarlo; vale la última.
const sinLeer = -2

var (
	fuente  atomic.Int32
	fuentes = make(chan struct{}, 1)
)

//export energiaEvento
func energiaEvento(tipo C.int, id C.long) {
	if tipo == 3 {
		fuente.Store(int32(id))
		select {
		case fuentes <- struct{}{}:
		default:
		}
		return
	}
	eventos <- evento{dormir: tipo == 1, id: int64(id)}
}

// lineaFuente es la línea para el daemon de la lectura v ("" si no se sabe).
func lineaFuente(v int32) string {
	switch v {
	case 1:
		return "power battery"
	case 0:
		return "power ac"
	}
	return ""
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
	fuente.Store(sinLeer)
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
	// La primera lectura, si el aviso no trajo ya otra más nueva.
	fuente.CompareAndSwap(sinLeer, int32(C.energiaFuente()))
	dicha := ""
	decirFuente := func() {
		if l := lineaFuente(fuente.Load()); l != "" && l != dicha {
			fmt.Fprintln(w, l)
			dicha = l
		}
	}
	decirFuente()
	for {
		select {
		case <-cerrado:
			return 0
		case <-fuentes:
			decirFuente()
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
