package machine

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	knet "github.com/juan52878911/kindling/internal/net"
)

// reconcile ajusta el estado guardado a la realidad del host al arrancar.
//
// El daemon puede reiniciarse, caerse o actualizarse mientras hay microVMs
// vivas. Sin esto, kling afirmaría cosas falsas: máquinas "running" cuyo proceso
// murió, o namespaces de máquinas que ya no existen.
func (m *Manager) reconcile() {
	// Lo primero es MIRAR, y hacerlo fuera del lock porque lee /proc.
	//
	// El estado en disco no es la verdad: se escribe con debounce y fuera del
	// candado, y sobre todo una microVM SOBREVIVE al daemon (firecracker se
	// lanza con setsid). Fiarse solo del fichero significa quitarle la red y el
	// techo de CPU a máquinas que están funcionando, y en el peor caso arrancar
	// un segundo firecracker sobre el mismo overlay.
	live := m.liveVMs()

	m.mu.Lock()
	defer m.mu.Unlock()

	seen := make(map[string]bool, len(m.byID))
	liveCg := make(map[string]bool)

	// Los namespaces y cgroups de lo que está VIVO se protegen aunque el estado
	// no lo conozca: una máquina creada justo antes de un corte puede no haber
	// llegado al fichero, y desmontarle la red la deja corriendo a ciegas.
	for id := range live {
		seen[nsName(id)] = true
		liveCg[cgName(id)] = true
	}

	for _, mc := range m.byID {
		seen[knet.Plan(mc.NetIndex, mc.ID).NS] = true

		if pid, alive := live[mc.ID]; alive {
			// Está viva: se readopta y NO se toca nada suyo.
			m.socket[mc.ID] = m.dir(mc.ID) + "/fc.sock"
			if mc.State != api.StateRunning {
				log.Printf("reconcile: %s (%s) sigue viva (pid %d) aunque el estado decía %q; la readopto",
					mc.Name, mc.ID[:8], pid, mc.State)
				mc.State = api.StateRunning
			}
			mc.PID = pid
			liveCg[cgName(mc.ID)] = true
			continue
		}

		switch mc.State {
		case api.StateRunning:
			// Su proceso ya no está. Si dejó un snapshot completo está WARM, no
			// parada: decir "stopped" deja el snapshot varado, porque Thaw se
			// niega a descongelar lo que no esté warm y habría que editar el
			// state.json a mano para recuperarlo.
			if m.hasSnapshot(mc.ID) {
				log.Printf("reconcile: %s (%s) ya no corre pero conserva su snapshot: warm",
					mc.Name, mc.ID[:8])
				mc.State = api.StateWarm
			} else {
				log.Printf("reconcile: %s (%s) ya no corre, marcada como stopped", mc.Name, mc.ID[:8])
				mc.State = api.StateStopped
			}
			mc.PID = 0
			knet.Plan(mc.NetIndex, mc.ID).Teardown()
			m.releaseCPU(mc.ID)

		case api.StateWarm, api.StateStopped, api.StateFailed:
			// Sin proceso: ni namespace ni cgroup hacen nada. Se recrean al
			// arrancarla o descongelarla.
			knet.Plan(mc.NetIndex, mc.ID).Teardown()
			m.releaseCPU(mc.ID)
		}
	}
	m.persist()

	m.sweepCgroups(liveCg)

	// Namespaces de máquinas que ya no existen: basura de ejecuciones anteriores.
	for _, ns := range knet.ListNamespaces() {
		if !seen[ns] {
			log.Printf("reconcile: limpiando namespace huérfano %s", ns)
			knet.TeardownNamespace(ns)
		}
	}
}

// nsName y cgName derivan del id igual que knet.Plan y el gestor de cgroups, y
// existen para poder proteger lo de una máquina viva sin conocer su NetIndex
// —que es justo lo que falta cuando no está en el estado guardado—.
func nsName(id string) string { return "kl-" + shortID(id) }
func cgName(id string) string { return "kl-" + shortID(id) }

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// hasSnapshot dice si una máquina tiene un congelado completo en disco.
func (m *Manager) hasSnapshot(id string) bool {
	d := m.dir(id)
	for _, f := range []string{"snap.file", "mem.file"} {
		if _, err := os.Stat(filepath.Join(d, f)); err != nil {
			return false
		}
	}
	return true
}

// liveVMs escanea /proc y devuelve qué microVM posee cada firecracker vivo.
//
// Es adopt() del revés: en vez de "¿sigue vivo el PID que apunté?", pregunta
// "¿qué está corriendo ahí fuera?". Esa vuelta es lo que permite reconciliar sin
// creerse el fichero de estado.
func (m *Manager) liveVMs() map[string]int {
	out := map[string]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out // no es Linux, o /proc no está montado
	}
	prefix := filepath.Join(m.root, "machines") + "/"

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // no es un proceso
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue // murió mientras mirábamos, o no es nuestro
		}
		for _, arg := range strings.Split(strings.TrimRight(string(b), "\x00"), "\x00") {
			rest, ok := strings.CutPrefix(arg, prefix)
			if !ok {
				continue
			}
			id, ok := strings.CutSuffix(rest, "/fc.sock")
			if ok && id != "" && !strings.Contains(id, "/") {
				out[id] = pid
			}
		}
	}
	return out
}

// adopt comprueba si el proceso de una microVM sigue vivo y es realmente suyo.
//
// No basta con mirar si el PID existe: los PID se reciclan. Se verifica que la
// línea de comandos del proceso menciona el socket de ESTA máquina.
func (m *Manager) adopt(mc *api.Machine) (string, bool) {
	if mc.PID <= 0 {
		return "", false
	}
	cmdline, err := os.ReadFile("/proc/" + itoa(mc.PID) + "/cmdline")
	if err != nil {
		return "", false
	}
	sock := m.dir(mc.ID) + "/fc.sock"
	if !strings.Contains(strings.ReplaceAll(string(cmdline), "\x00", " "), sock) {
		return "", false
	}
	if _, err := os.Stat(sock); err != nil {
		return "", false
	}
	return sock, true
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// watch vigila periódicamente que lo que decimos que corre, corra de verdad.
//
// Una microVM puede morir por su cuenta: pánico del kernel invitado, OOM del
// host, o alguien matando el proceso. Reportar "running" sobre algo muerto es
// peor que no reportar nada, porque el gateway enrutaría peticiones a la nada.
func (m *Manager) watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweep()
			m.expireTTL(ctx)
			// El disco se recalcula aquí y no en List(): así `kling ps` no paga
			// un recorrido por máquina, y el dato sigue fresco para las que
			// están escribiendo.
			m.refreshDiskUsage()
		}
	}
}

func (m *Manager) sweep() {
	var died []*api.Machine
	live := make(map[string]bool)

	m.mu.Lock()
	for _, mc := range m.byID {
		if mc.State != api.StateRunning {
			continue
		}
		if _, ok := m.adopt(mc); ok {
			live["kl-"+mc.ID[:8]] = true
			continue
		}
		mc.State = api.StateFailed
		mc.LastErr = "el proceso de la microVM desapareció"
		mc.PID = 0
		delete(m.socket, mc.ID)
		died = append(died, mc)
	}
	if len(died) > 0 {
		m.persist()
	}
	m.mu.Unlock()

	// Recoger cgroups que se resistieron a morir en su momento.
	m.sweepCgroups(live)

	// Fuera del mutex: publicar eventos y liberar red puede tardar.
	for _, mc := range died {
		knet.Plan(mc.NetIndex, mc.ID).Teardown()
		m.releaseCPU(mc.ID)
		m.bus.Publish(api.Event{
			Time: time.Now(), Type: api.EvFailed, ID: mc.ID, Name: mc.Name,
			Message: "el proceso de la microVM desapareció",
		})
	}
}

// Watch lanza el vigilante en segundo plano.
func (m *Manager) Watch(ctx context.Context, every time.Duration) {
	go m.watch(ctx, every)
}

// expireTTL congela las máquinas cuyo tiempo de vida se agotó.
//
// Congelar, no matar: es la diferencia entre serverless y apagar cosas. La
// herramienta deja de costar CPU y RAM, pero vuelve en ~30 ms cuando haga falta.
func (m *Manager) expireTTL(ctx context.Context) {
	var due []string

	m.mu.RLock()
	for _, mc := range m.byID {
		if mc.State != api.StateRunning || mc.TTLSeconds <= 0 || mc.StartedAt == nil {
			continue
		}
		if time.Since(*mc.StartedAt) >= time.Duration(mc.TTLSeconds)*time.Second {
			due = append(due, mc.ID)
		}
	}
	m.mu.RUnlock()

	for _, id := range due {
		if _, err := m.Freeze(ctx, id); err != nil {
			log.Printf("ttl: no pude congelar %s: %v", id[:8], err)
		}
	}
}
