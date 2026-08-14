package main

// Cosecha de procesos huérfanos.
//
// El puente es PID 1 dentro de la microVM. Todo proceso que se queda sin padre
// —los nietos que deja un `npx` al morir, los workers de node— se reparenta a
// él, y si nadie hace wait4 queda zombi reteniendo un PID. Una microVM cuyas
// sesiones rotan los acumula sin límite hasta agotar la tabla de procesos, y
// entonces arrancar un servidor MCP falla con un error de fork que es imposible
// de diagnosticar desde fuera.
//
// La dificultad no es cosechar, es no pisar a quien ya espera. wait4(-1) no
// permite elegir a quién recoge: puede llevarse al hijo directo de una sesión
// antes de que su cmd.Wait() lo haga, y entonces Wait devuelve ECHILD y se
// pierde el código de salida — justo el dato que distingue "se cerró bien" de
// "se lo llevó el OOM killer", que aquí vale oro.
//
// La solución es no competir: el cosechador recoge a quien sea y, si el
// recogido estaba registrado, le ENTREGA el estado a su dueño por un canal. El
// dueño llama a cmd.Wait() como siempre y solo mira el canal si pierde la
// carrera. Gane quien gane el wait4, el estado no se pierde.

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type reaper struct {
	mu      sync.Mutex
	tracked map[int]chan syscall.WaitStatus
}

var procReaper = &reaper{tracked: map[int]chan syscall.WaitStatus{}}

// startTracked arranca el proceso y lo registra, ambas cosas bajo el mismo
// candado que usa el cosechador.
//
// Tiene que ser bajo el candado: un hijo que muere en el acto podría ser
// recogido ANTES de quedar registrado, y su estado se descartaría como el de un
// huérfano cualquiera.
func (r *reaper) startTracked(cmd *exec.Cmd) (chan syscall.WaitStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Con buffer: el cosechador entrega y sigue, sin esperar a que nadie lea.
	ch := make(chan syscall.WaitStatus, 1)
	r.tracked[cmd.Process.Pid] = ch
	return ch, nil
}

// forget retira un pid ya esperado con éxito, para no acumular entradas.
func (r *reaper) forget(pid int) {
	r.mu.Lock()
	delete(r.tracked, pid)
	r.mu.Unlock()
}

// run atiende SIGCHLD de por vida.
//
// Solo siendo PID 1: fuera de la microVM —tests, desarrollo en un Mac— el init
// del sistema ya cosecha, y un wait4(-1) en un proceso normal robaría hijos
// legítimos de quien sea que los esté esperando.
func (r *reaper) run() {
	if os.Getpid() != 1 {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)
	// El ticker es la red de seguridad: las señales se agrupan, y una ráfaga de
	// muertes puede quedarse en un solo aviso.
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ch:
		case <-tick.C:
		}
		r.reapAll()
	}
}

func (r *reaper) reapAll() {
	for {
		r.mu.Lock()
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			// 0: hay hijos pero ninguno ha terminado. ECHILD: no hay hijos.
			// En ambos casos, a esperar la próxima señal.
			r.mu.Unlock()
			return
		}
		if owner, ok := r.tracked[pid]; ok {
			owner <- ws
			delete(r.tracked, pid)
		} else {
			log.Printf("init: orphan %d reaped (%v)", pid, ws)
		}
		r.mu.Unlock()
	}
}

// reapedExit es la salida de un proceso que recogió el cosechador.
//
// Lleva el código dentro y no solo un texto: quien llama distingue "el comando
// falló con código N" de "ni pude lanzarlo", y confundirlos haría que un
// `npm install` correcto se reportara como un binario inexistente.
type reapedExit struct {
	status syscall.WaitStatus
}

func (e *reapedExit) ExitCode() int {
	if e.status.Signaled() {
		// Mismo criterio que exec.ExitError: 128 + señal.
		return 128 + int(e.status.Signal())
	}
	return e.status.ExitStatus()
}

func (e *reapedExit) Error() string {
	if e.status.Signaled() {
		return fmt.Sprintf("signal: %v", e.status.Signal())
	}
	return fmt.Sprintf("exit status %d", e.status.ExitStatus())
}

// waitStatusErr traduce un estado recogido por el cosechador al mismo error que
// habría devuelto cmd.Wait(), para que quien lo reciba no note la diferencia.
func waitStatusErr(ws syscall.WaitStatus) error {
	if ws.Exited() && ws.ExitStatus() == 0 {
		return nil
	}
	return &reapedExit{status: ws}
}

// exitCodeOf saca el código de salida de un error de espera, venga de cmd.Wait
// o del cosechador. Devuelve -1 y false si el proceso ni llegó a lanzarse.
func exitCodeOf(err error) (int, bool) {
	switch e := err.(type) {
	case *exec.ExitError:
		return e.ExitCode(), true
	case *reapedExit:
		return e.ExitCode(), true
	}
	return -1, false
}

// waitFor espera al proceso, recuperando el estado del cosechador si este se
// adelantó.
func waitFor(cmd *exec.Cmd, exitCh chan syscall.WaitStatus) error {
	err := cmd.Wait()
	if err == nil {
		return nil
	}
	// ECHILD significa que el cosechador llegó primero. cmd.Wait() ya ha hecho
	// su trabajo real —cerrar descriptores y esperar a sus goroutines—, así que
	// aquí solo falta recuperar el estado que se llevó.
	if !isECHILD(err) {
		return err
	}
	// Se ESPERA un momento al canal, no se mira con default.
	//
	// cmd.Wait() puede devolver ECHILD en cuanto el cosechador hace su wait4,
	// una línea antes de que ejecute `owner <- ws`. Con default, esa carrera de
	// microsegundos perdía el estado —y con él la distinción "se cerró" vs "se
	// lo llevó el OOM killer", que es justo el dato que se quería preservar—.
	//
	// El envío va bufferizado y ocurre en la misma sección crítica que el wait4,
	// así que llega enseguida; el plazo es una red de seguridad para no colgar el
	// cierre si por lo que sea no llegara nunca.
	select {
	case ws := <-exitCh:
		return waitStatusErr(ws)
	case <-time.After(2 * time.Second):
		return nil
	}
}

func isECHILD(err error) bool {
	var errno syscall.Errno
	if e, ok := err.(*os.SyscallError); ok {
		if n, ok := e.Err.(syscall.Errno); ok {
			errno = n
		}
	}
	if errno == 0 {
		// exec.Cmd envuelve el error de wait de varias formas según la
		// plataforma; el texto es el último recurso.
		return err != nil && (syscall.ECHILD.Error() == err.Error() ||
			containsECHILD(err.Error()))
	}
	return errno == syscall.ECHILD
}

func containsECHILD(s string) bool {
	for i := 0; i+len("no child processes") <= len(s); i++ {
		if s[i:i+len("no child processes")] == "no child processes" {
			return true
		}
	}
	return false
}
