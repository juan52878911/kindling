package main

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// F11: cerrar la terminal (SIGHUP) o un `| head -1` (SIGPIPE) cancelan el
// contexto como Ctrl-C, para que los defer retiren las máquinas temporales.
func TestCtxWithSignalsCortaConHUP(t *testing.T) {
	defer interrumpido.Store(false)
	for _, sig := range []syscall.Signal{syscall.SIGHUP} {
		interrumpido.Store(false)
		ctx, stop := ctxWithSignals()
		if err := syscall.Kill(os.Getpid(), sig); err != nil {
			stop()
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			stop()
			t.Fatalf("%v no canceló el contexto", sig)
		}
		stop()
		if !interrumpido.Load() {
			t.Fatalf("%v no apuntó la interrupción", sig)
		}
	}
}

// SIGPIPE se prueba como pasa de verdad: escribiendo en una salida estándar
// cuyo lector ya cerró (el runtime de Go la convierte en SIGPIPE solo para los
// descriptores 1 y 2). Sin Notify, el proceso muere ahí, sin pasar por los
// defer. Corre en un subproceso: el propio binario del test.
func TestCtxWithSignalsCortaConSalidaRota(t *testing.T) {
	if os.Getenv("KLING_TEST_SALIDA_ROTA") == "1" {
		ctx, stop := ctxWithSignals()
		defer stop()
		_, _ = os.Stdout.Write([]byte("x\n"))
		select {
		case <-ctx.Done():
			if interrumpido.Load() {
				os.Exit(0)
			}
			os.Exit(4)
		case <-time.After(5 * time.Second):
			os.Exit(3)
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCtxWithSignalsCortaConSalidaRota$")
	cmd.Env = append(os.Environ(), "KLING_TEST_SALIDA_ROTA=1")
	cmd.Stdout = w
	err = cmd.Run()
	w.Close()
	if err != nil {
		t.Fatalf("con la salida rota, el proceso no canceló su contexto: %v", err)
	}
}

// El daemon no se apaga porque un cliente cuelgue: con SIGPIPE en su
// signal.Notify, Go le entregaba la señal de cualquier socket roto (no solo de
// la salida estándar) y el daemon se cerraba como con un SIGTERM. Pasó en el
// e2e de Linux (sección 6, carpetas compartidas); un exec cortado a media
// salida basta. Aquí, un EPIPE en un socket que no es la salida estándar no
// corta su contexto; SIGTERM sí. (Es en Linux donde falla sin el arreglo: en
// macOS ese write no llega al canal.)
func TestDaemonSigueTrasUnSocketRoto(t *testing.T) {
	defer interrumpido.Store(false)
	interrumpido.Store(false)
	ctx, stop := ctxDelDaemon()
	defer stop()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	syscall.Close(fds[1]) // el cliente cuelga
	if _, err := syscall.Write(fds[0], []byte("respuesta")); err != syscall.EPIPE {
		t.Fatalf("la escritura en el socket roto dio %v, no EPIPE", err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("un socket roto (SIGPIPE) apagó el daemon")
	case <-time.After(500 * time.Millisecond):
	}
	if interrumpido.Load() {
		t.Fatal("un socket roto contó como interrupción")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM no paró el daemon")
	}
}
