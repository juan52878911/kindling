package guest

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Un cmd.Start lento no puede serializar a los demás exec: el fork va fuera
// del cerrojo del cosechador.
func TestReaperStartLentoNoBloqueaOtroExec(t *testing.T) {
	r := &Reaper{tracked: map[int]chan syscall.WaitStatus{}}
	entra, suelta := make(chan struct{}), make(chan struct{})
	primero := true
	r.iniciar = func(c *exec.Cmd) error {
		if primero {
			primero = false
			close(entra)
			<-suelta
		}
		return c.Start()
	}
	lento := exec.Command("true")
	hecho := make(chan struct{})
	go func() {
		defer close(hecho)
		if _, err := r.StartTracked(lento); err != nil {
			t.Error(err)
		}
	}()
	<-entra // el primero está dentro de su Start, colgado
	rapido := exec.Command("true")
	listo := make(chan error, 1)
	go func() { _, err := r.StartTracked(rapido); listo <- err }()
	select {
	case err := <-listo:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("el segundo exec quedó serializado tras el Start lento")
	}
	close(suelta)
	<-hecho
	_ = lento.Wait()
	_ = rapido.Wait()
}

// Si el cosechador recoge al hijo antes de que quede registrado, su estado se
// entrega igualmente.
func TestReaperEstadoAdelantadoAlRegistro(t *testing.T) {
	r := &Reaper{tracked: map[int]chan syscall.WaitStatus{}}
	r.iniciar = func(c *exec.Cmd) error {
		if err := c.Start(); err != nil {
			return err
		}
		pid := c.Process.Pid
		for i := 0; i < 400; i++ {
			r.reapAll()
			r.mu.Lock()
			_, hay := r.adelantados[pid]
			r.mu.Unlock()
			if hay {
				return nil
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Skip("el cosechador no recogió al hijo en este entorno")
		return nil
	}
	cmd := exec.Command("sh", "-c", "exit 4")
	ch, err := r.StartTracked(cmd)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ws := <-ch:
		if ws.ExitStatus() != 4 {
			t.Fatalf("estado = %v, quería salida 4", ws)
		}
	default:
		t.Fatal("el estado adelantado no se entregó")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tracked) != 0 || r.adelantados != nil {
		t.Fatalf("quedó estado: tracked=%d adelantados=%v", len(r.tracked), r.adelantados)
	}
}

// R6-I2 (R4): con arranques solapados de continuo, la tabla de adelantados no
// crece sin límite, y un arranque posterior no hereda un estado anterior a él
// (pid reutilizado).
func TestReaperAdelantadosPorSecuencia(t *testing.T) {
	r := &Reaper{tracked: map[int]chan syscall.WaitStatus{}}
	suelta := make(chan struct{})
	dentro := make(chan struct{})
	r.iniciar = func(c *exec.Cmd) error {
		if c.Path == "viejo" {
			close(dentro)
			<-suelta
		}
		c.Process = &os.Process{Pid: 424242}
		return nil
	}
	hecho := make(chan struct{})
	go func() {
		defer close(hecho)
		_, _ = r.arrancar(&exec.Cmd{Path: "viejo"})
	}()
	<-dentro
	// Un huérfano se recoge mientras el arranque viejo sigue en curso.
	r.mu.Lock()
	r.adelantados = map[int]adelantado{424242: {ws: 7 << 8, seq: r.secuencia}}
	r.mu.Unlock()
	// Un arranque posterior con el mismo pid no hereda ese estado.
	ch, err := r.arrancar(&exec.Cmd{Path: "nuevo"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ws := <-ch:
		t.Fatalf("el arranque nuevo heredó un estado anterior: %v", ws)
	default:
	}
	// Con el viejo aún vivo, el estado sigue guardado para él; al acabar, se va.
	r.mu.Lock()
	n := len(r.adelantados)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("adelantados = %d con el arranque viejo en curso, quería 1", n)
	}
	close(suelta)
	<-hecho
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.adelantados != nil || len(r.vivos) != 0 {
		t.Fatalf("quedó estado: %v vivos=%d", r.adelantados, len(r.vivos))
	}
}
