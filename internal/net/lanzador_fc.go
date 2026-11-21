//go:build linux

package net

import (
	"bytes"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
)

// LANZADOR EN UN NETNS SIN `ip netns exec`.
//
// `ip netns exec NS cmd` hace unshare(CLONE_NEWNS) y remonta /sys antes de
// lanzar cmd: son dos procesos y dos operaciones que serializan en el kernel
// (namespace_sem, kernfs), y para entrar en un netns no hacen falta. Aquí se
// fija el netns del HILO con setns y se lanza el hijo desde él (clone hereda
// el netns del hilo que lo crea); Go no migra una goroutine fijada con
// LockOSThread, así que el resto del daemon no se entera.

// hilosPerdidos cuenta los hilos que no se pudieron devolver al netns del host
// y se sacrificaron (la goroutine acaba con el hilo fijado: Go lo termina).
var hilosPerdidos atomic.Int64

// opsNetns son las llamadas al sistema del lanzador; sustituibles en tests.
var opsNetns = struct {
	abrir  func(ruta string) (int, error)
	setns  func(fd int) error
	cerrar func(fd int)
}{
	abrir: func(ruta string) (int, error) { return syscall.Open(ruta, syscall.O_RDONLY|syscall.O_CLOEXEC, 0) },
	setns: func(fd int) error {
		if sysSetns < 0 {
			return syscall.ENOSYS
		}
		_, _, e := syscall.RawSyscall(uintptr(sysSetns), uintptr(fd), syscall.CLONE_NEWNET, 0)
		if e != 0 {
			return e
		}
		return nil
	},
	cerrar: func(fd int) { _ = syscall.Close(fd) },
}

// dirNetns es donde `ip netns add` deja el fichero de cada netns.
var dirNetns = "/var/run/netns/"

// enHiloDeNetns ejecuta f en un hilo del SO dentro del netns ns y lo devuelve
// al del host. Si no puede volver, el hilo NO se devuelve a Go: se pierde (y
// muere) antes que servir goroutines de otro en el netns equivocado.
// devuelto dice si el hilo volvió.
func enHiloDeNetns(ns string, f func() error) (devuelto bool, err error) {
	type res struct {
		devuelto bool
		err      error
	}
	c := make(chan res, 1)
	go func() {
		runtime.LockOSThread()
		r := res{}
		defer func() {
			if r.devuelto {
				runtime.UnlockOSThread()
			} else {
				hilosPerdidos.Add(1)
			}
			c <- r
		}()
		orig, err := opsNetns.abrir("/proc/thread-self/ns/net")
		if err != nil {
			r.devuelto, r.err = true, fmt.Errorf("opening the thread's netns: %w", err)
			return // no se tocó nada: el hilo sigue en su netns
		}
		defer opsNetns.cerrar(orig)
		destino, err := opsNetns.abrir(dirNetns + ns)
		if err != nil {
			r.devuelto, r.err = true, fmt.Errorf("opening netns %s: %w", ns, err)
			return
		}
		defer opsNetns.cerrar(destino)
		if err := opsNetns.setns(destino); err != nil {
			// Un setns que falla no cambia el netns del hilo.
			r.devuelto, r.err = true, fmt.Errorf("setns %s: %w", ns, err)
			return
		}
		r.err = f()
		if err := opsNetns.setns(orig); err != nil {
			r.devuelto = false
			if r.err == nil {
				r.err = fmt.Errorf("returning the thread from netns %s: %w", ns, err)
			}
			return
		}
		r.devuelto = true
	}()
	r := <-c
	return r.devuelto, r.err
}

// ejecutarEnNetns arranca cmd dentro del netns ns y espera a que acabe. El
// netns solo se fija mientras se hace el clone: la espera ya es del hijo.
func ejecutarEnNetns(ns string, cmd *exec.Cmd) error {
	if _, err := enHiloDeNetns(ns, cmd.Start); err != nil {
		if cmd.Process != nil { // arrancó pero el hilo no volvió bien
			_ = cmd.Wait()
		}
		return err
	}
	return cmd.Wait()
}

// ordenEnNetnsReal lanza args dentro del netns ns, como `ip netns exec ns args...`
// con el mismo formato de error que run.
func ordenEnNetnsReal(ns string, args ...string) error {
	args = conEspera(args)
	var out bytes.Buffer
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := ejecutarEnNetns(ns, cmd); err != nil {
		return fmt.Errorf("in netns %s: %s: %v: %s", ns, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

// ordenEnNetns es el lanzador de Setup; sustituible en tests.
var ordenEnNetns = ordenEnNetnsReal
