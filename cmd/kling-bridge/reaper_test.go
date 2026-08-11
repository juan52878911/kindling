package main

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// El cosechador puede llevarse al hijo directo de una sesión antes que su
// Wait(). Cuando eso pasa, el código de salida NO debe perderse.
//
// Es el dato que distingue "la sesión se cerró" de "se lo llevó el OOM killer",
// y en una microVM la consola serie es la única ventana al interior. Un Wait que
// devuelve ECHILD sin más deja ese log diciendo nada.
func TestElEstadoNoSePierdeSiElCosechadorSeAdelanta(t *testing.T) {
	casos := []struct {
		nombre string
		args   []string
		code   int
	}{
		{"salida limpia", []string{"-c", "exit 0"}, 0},
		{"código de error", []string{"-c", "exit 3"}, 3},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			cmd := exec.Command("sh", c.args...)
			exitCh, err := procReaper.startTracked(cmd)
			if err != nil {
				t.Fatal(err)
			}
			pid := cmd.Process.Pid

			// Que el cosechador gane la carrera, como dentro de la microVM.
			var recogido bool
			for i := 0; i < 200 && !recogido; i++ {
				procReaper.reapAll()
				procReaper.mu.Lock()
				_, sigue := procReaper.tracked[pid]
				procReaper.mu.Unlock()
				recogido = !sigue
				if !recogido {
					time.Sleep(5 * time.Millisecond)
				}
			}
			if !recogido {
				t.Skip("el cosechador no llegó a recogerlo en este entorno")
			}

			err = waitFor(cmd, exitCh)
			procReaper.forget(pid)

			if c.code == 0 {
				if err != nil {
					t.Errorf("una salida limpia se reportó como error: %v", err)
				}
				return
			}
			got, ok := exitCodeOf(err)
			if !ok {
				t.Fatalf("se perdió el código de salida: %v", err)
			}
			if got != c.code {
				t.Errorf("código = %d, want %d", got, c.code)
			}
		})
	}
}

// Un proceso muerto por señal se distingue de uno que salió con código.
func TestUnaSenalSeDistingueDeUnCodigo(t *testing.T) {
	var ws syscall.WaitStatus
	// Estado sintético: matado por SIGKILL (9).
	ws = syscall.WaitStatus(int(syscall.SIGKILL))
	if !ws.Signaled() {
		t.Skip("la codificación de WaitStatus difiere en esta plataforma")
	}
	err := waitStatusErr(ws)
	if err == nil {
		t.Fatal("una muerte por señal no es una salida limpia")
	}
	if !strings.Contains(err.Error(), "signal") {
		t.Errorf("el error no dice que fue una señal: %v", err)
	}
	if code, ok := exitCodeOf(err); !ok || code != 128+int(syscall.SIGKILL) {
		t.Errorf("código = %d (ok=%v), quería 128+9", code, ok)
	}
}

// El cosechador solo debe correr siendo PID 1. En cualquier otro sitio robaría
// hijos legítimos de quien los esté esperando.
func TestElCosechadorSoloCorreComoPID1(t *testing.T) {
	hecho := make(chan struct{})
	go func() { procReaper.run(); close(hecho) }()
	select {
	case <-hecho:
	case <-time.After(time.Second):
		t.Fatal("run() no retornó fuera de PID 1: estaría cosechando hijos ajenos")
	}
}
