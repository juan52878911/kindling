package machine

import (
	"context"
	"log"
	"os"
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
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := make(map[string]bool, len(m.byID))
	for _, mc := range m.byID {
		seen[knet.Plan(mc.NetIndex, mc.ID).NS] = true

		switch mc.State {
		case api.StateRunning:
			if sock, ok := m.adopt(mc); ok {
				// Sigue viva: la readoptamos con su socket.
				m.socket[mc.ID] = sock
				continue
			}
			// Su proceso ya no está: no podemos seguir diciendo que corre.
			log.Printf("reconcile: %s (%s) ya no corre, marcada como stopped", mc.Name, mc.ID[:8])
			mc.State = api.StateStopped
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
	m.schedulePersist()

	// Cgroups de máquinas que ya no corren.
	liveCg := make(map[string]bool)
	for _, mc := range m.byID {
		if mc.State == api.StateRunning {
			liveCg["kl-"+mc.ID[:8]] = true
		}
	}
	m.sweepCgroups(liveCg)

	// Namespaces de máquinas que ya no existen: basura de ejecuciones anteriores.
	for _, ns := range knet.ListNamespaces() {
		if !seen[ns] {
			log.Printf("reconcile: limpiando namespace huérfano %s", ns)
			knet.TeardownNamespace(ns)
		}
	}
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
		m.schedulePersist()
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
