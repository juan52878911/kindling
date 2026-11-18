package main

import (
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

// M1: un exec interrumpido sale con 130, no con 1.
func TestExecInterrumpidoSaleCon130(t *testing.T) {
	defer interrumpido.Store(false)
	empezo := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /machines/{ref}/exec", func(w http.ResponseWriter, r *http.Request) {
		close(empezo)
		<-r.Context().Done()
	})
	c := fakeDaemon(t, mux)
	type res struct {
		code int
		err  error
	}
	fin := make(chan res, 1)
	go func() {
		code, err := cmdExec([]string{"-H", c.Endpoint(), "m", "sleep", "60"})
		fin <- res{code, err}
	}()
	select {
	case <-empezo:
	case <-time.After(10 * time.Second):
		t.Fatal("el exec no llegó al daemon")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-fin:
		if r.code != salidaInterrumpido {
			t.Fatalf("código %d (%v), quería %d", r.code, r.err, salidaInterrumpido)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("el exec no acabó tras SIGINT")
	}
}
