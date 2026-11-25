package daemon

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// constructorConNieto deja un `sleep` en segundo plano con la salida
// heredada y escribe su pid en pidf.
func constructorConNieto(ctx context.Context, t *testing.T, resto string) (*exec.Cmd, string) {
	t.Helper()
	pidf := filepath.Join(t.TempDir(), "nieto.pid")
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 3600 & echo $! > "+pidf+"; echo construido; "+resto)
	return cmd, pidf
}

func nietoVivo(t *testing.T, pidf string) bool {
	t.Helper()
	b, err := os.ReadFile(pidf)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	// Un zombi también responde a kill(pid, 0): muerto, pero sin recoger.
	// Pasa donde el PID 1 no recoge a los huérfanos (un contenedor sin
	// init), y el nieto, huérfano tras salir sh, se queda así.
	if st, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if i := bytes.LastIndexByte(st, ')'); i >= 0 && i+2 < len(st) && st[i+2] == 'Z' {
			return false
		}
	}
	return true
}

// R4: un constructor que acaba bien pero deja un nieto con su salida no deja
// la construcción (ni sus cerrojos) colgada; el nieto muere. Y al vencer el
// plazo muere el grupo entero, no solo el hijo directo.
func TestConstructorConNietoNoCuelga(t *testing.T) {
	antes := esperaConstructor
	esperaConstructor = 200 * time.Millisecond
	t.Cleanup(func() { esperaConstructor = antes })

	esperar := func(cmd *exec.Cmd, salida *bytes.Buffer) error {
		hecho := make(chan error, 1)
		go func() { hecho <- correrConstructor(cmd, salida) }()
		select {
		case err := <-hecho:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("la construcción sigue colgada de la salida del nieto")
			return nil
		}
	}

	var salida bytes.Buffer
	cmd, pidf := constructorConNieto(context.Background(), t, "exit 0")
	if err := esperar(cmd, &salida); err != nil {
		t.Fatalf("el constructor acabó bien: %v", err)
	}
	if !strings.Contains(salida.String(), "construido") {
		t.Fatalf("salida = %q", salida.String())
	}
	time.Sleep(50 * time.Millisecond)
	if nietoVivo(t, pidf) {
		t.Fatal("el nieto sigue vivo")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	salida.Reset()
	cmd, pidf = constructorConNieto(ctx, t, "sleep 3600")
	if err := esperar(cmd, &salida); err == nil {
		t.Fatal("un constructor cortado por el plazo no puede salir bien")
	}
	time.Sleep(50 * time.Millisecond)
	if nietoVivo(t, pidf) {
		t.Fatal("el plazo mató al hijo pero no al nieto")
	}
}

// La salida del constructor se queda con el final, sin pasar del tope.
func TestColaSalidaGuardaElFinal(t *testing.T) {
	var buf bytes.Buffer
	c := &colaSalida{buf: &buf, max: 8}
	for _, s := range []string{"abc", "defgh", "ijk", "0123456789XY"} {
		if n, err := c.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
		if buf.Len() > 8 {
			t.Fatalf("pasó del tope: %q", buf.String())
		}
	}
	if buf.String() != "456789XY" {
		t.Fatalf("final = %q", buf.String())
	}
	c.Write([]byte("z"))
	if buf.String() != "56789XYz" {
		t.Fatalf("final = %q", buf.String())
	}
}
