//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// Cerrar la terminal (SIGHUP) en mitad de un checkout cancela el contexto, como
// Ctrl-C: así se deshace lo que iba a medias en vez de morir sin más.
func TestSignalCtxCapturaSIGHUP(t *testing.T) {
	// Un receptor propio para que el SIGHUP de la prueba no mate el proceso
	// si signalCtx no lo captura.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGHUP)
	defer signal.Stop(guard)

	ctx, stop := signalCtx()
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not cancel the context")
	}
}
