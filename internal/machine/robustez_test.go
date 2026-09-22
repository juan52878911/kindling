package machine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// vmmFalso deja corriendo un proceso cuya línea de comandos imita la de
// firecracker para la máquina id, que es lo que liveVMs busca en /proc.
//
// Devuelve su pid y un canal que se cierra cuando muere DE VERDAD. Hace falta el
// canal y no vale `kill(pid, 0)`: al matar un hijo de este proceso queda un
// zombi hasta que alguien lo recoge, y un zombi contesta a la señal 0 como si
// estuviera vivo. El test se creía que el barrido no había matado nada.
func vmmFalso(t *testing.T, m *Manager, id string) (int, <-chan struct{}) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("liveVMs lee /proc; solo aplica en Linux")
	}
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(m.dir(id), "fc.sock")
	// Igual que en reconcile_test: el sh sigue vivo y conserva la ruta en $0.
	cmd := exec.Command("/bin/sh", "-c", "sleep 30", sock)
	if err := cmd.Start(); err != nil {
		t.Skipf("no pude lanzar el proceso de prueba: %v", err)
	}
	muerto := make(chan struct{})
	go func() { _ = cmd.Wait(); close(muerto) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-muerto:
		case <-time.After(5 * time.Second):
		}
	})
	esperarCmdline(t, cmd.Process.Pid, sock)
	return cmd.Process.Pid, muerto
}

// sigueVivo dice si el proceso no ha muerto todavía.
func sigueVivo(muerto <-chan struct{}) bool {
	select {
	case <-muerto:
		return false
	default:
		return true
	}
}

// El caso real: una restauración que falla marca la máquina failed con PID 0, y
// su firecracker se queda vivo reteniendo la RAM. Hasta ahora solo lo recogía el
// arranque del daemon; el vigilante pasaba de largo.
func TestSweepOrphanVMMsMataElHuerfanoDeUnaFailed(t *testing.T) {
	m := newTestManager(t)
	id := "fa11ed0000000001"
	_, muerto := vmmFalso(t, m, id)

	mc := m.addForTest(id)
	m.mu.Lock()
	mc.State, mc.PID = api.StateFailed, 0
	m.mu.Unlock()

	// Primera vuelta: solo lo apunta. Un fallo real sigue ahí a la siguiente;
	// una transición en curso, no.
	m.sweepOrphanVMMs()
	if !sigueVivo(muerto) {
		t.Fatal("lo mató en la primera vuelta: eso es una carrera con quien esté creando la máquina")
	}
	m.sweepOrphanVMMs()
	select {
	case <-muerto:
	case <-time.After(5 * time.Second):
		t.Fatal("el VMM huérfano sigue vivo tras dos vueltas del barrido")
	}
}

// Una máquina en pleno arranque está registrada como "created" y su VMM ya
// existe: matarla sería peor que el problema que esto arregla.
func TestSweepOrphanVMMsRespetaLoQueEstaNaciendo(t *testing.T) {
	m := newTestManager(t)
	for _, caso := range []struct {
		nombre string
		estado api.State
	}{
		{"created", api.StateCreated},
		{"running", api.StateRunning},
		{"warm", api.StateWarm},
	} {
		id := "0000000000000" + caso.nombre[:3]
		_, muerto := vmmFalso(t, m, id)
		mc := m.addForTest(id)
		m.mu.Lock()
		mc.State = caso.estado
		m.mu.Unlock()

		m.sweepOrphanVMMs()
		m.sweepOrphanVMMs()
		if !sigueVivo(muerto) {
			t.Errorf("%s: mató el VMM de una máquina que no había fallado", caso.nombre)
		}
	}
}

// Y lo reservado tampoco: es la misma señal que protege los directorios del
// barrido mientras se construye la máquina.
func TestSweepOrphanVMMsRespetaLaReserva(t *testing.T) {
	m := newTestManager(t)
	id := "5e5e5e5e5e5e5e5e"
	_, muerto := vmmFalso(t, m, id)
	soltar := m.reserveDir(id)
	defer soltar()

	m.sweepOrphanVMMs()
	m.sweepOrphanVMMs()
	if !sigueVivo(muerto) {
		t.Fatal("mató un VMM cuya máquina se estaba creando")
	}
}

// El TTL se medía desde StartedAt, y Thaw lo reescribe: una máquina que se
// congela y despierta reiniciaba su cuenta y podía no vencer nunca.
func TestElTTLNoSeReiniciaAlDespertar(t *testing.T) {
	m := newTestManager(t)
	hace10m := time.Now().Add(-10 * time.Minute)
	ahora := time.Now()

	mc := m.addForTest("ttl0000000000001")
	m.mu.Lock()
	mc.TTLSeconds = 300
	mc.TTLAt = &hace10m   // el reloj del TTL: arrancó hace 10 minutos
	mc.StartedAt = &ahora // despertó hace un instante
	m.mu.Unlock()

	if d := ttlDesde(mc); !d.Equal(hace10m) {
		t.Fatalf("ttlDesde = %v, quería el reloj del TTL (%v)", d, hace10m)
	}

	// Sin TTLAt (máquinas anteriores a este campo) se conserva el comportamiento
	// de antes: se cuenta desde el arranque.
	m.mu.Lock()
	mc.TTLAt = nil
	m.mu.Unlock()
	if d := ttlDesde(mc); !d.Equal(ahora) {
		t.Fatalf("sin TTLAt, ttlDesde = %v, quería StartedAt (%v)", d, ahora)
	}
}

// Renovar pone el reloj a cero, y se puede renovar un sandbox dormido: si no,
// conservarlo exigiría despertarlo.
func TestRenewReiniciaElRelojYValeCongelada(t *testing.T) {
	m := newTestManager(t)
	hace1h := time.Now().Add(-time.Hour)
	mc := m.addForTest("ren0000000000001")
	m.mu.Lock()
	mc.State = api.StateWarm
	mc.TTLSeconds = 60
	mc.TTLAt = &hace1h
	m.mu.Unlock()

	out, err := m.Renew("ren0000000000001", 600)
	if err != nil {
		t.Fatalf("renew de una congelada: %v", err)
	}
	if out.TTLSeconds != 600 {
		t.Errorf("ttl = %d, want 600", out.TTLSeconds)
	}
	if out.TTLAt == nil || time.Since(*out.TTLAt) > time.Minute {
		t.Errorf("no reinició el reloj: %v", out.TTLAt)
	}

	m.mu.Lock()
	mc.State = api.StateStopped
	m.mu.Unlock()
	if _, err := m.Renew("ren0000000000001", 600); !errors.Is(err, ErrNotRunning) {
		t.Errorf("renew de una parada: %v, quería ErrNotRunning", err)
	}
}

// Una máquina con un secreto inyectado no se puede congelar nunca, así que su
// TTL generaba una negativa cada 10 s para siempre. Ahora se retira el TTL, una
// vez y diciéndolo.
func TestTTLDeUnaMaquinaConSecretosSeRetira(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "5ec0000000000001"
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.HasSecrets = true
	mc.TTLSeconds = 60
	m.mu.Unlock()

	m.handleFreezeFailure(id, errors.New("has an injected secret"))

	cur, _ := m.Get(id)
	if cur.TTLSeconds != 0 {
		t.Fatalf("ttl = %d; quería 0: el TTL no se puede cumplir y fingir que sigue vigente engaña", cur.TTLSeconds)
	}
	if cur.State != api.StateRunning {
		t.Errorf("estado = %s; la máquina está sana, no ha fallado", cur.State)
	}
	// Y no cuenta como fallo de congelación: no debe acercar a la máquina al
	// límite que mata a las averiadas.
	if n := m.freezeFails[id]; n != 0 {
		t.Errorf("fallos apuntados = %d, want 0", n)
	}
}
