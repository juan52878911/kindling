package machine

import (
	"errors"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// Borrado en segundo plano (kling rm).
//
// Medido en el lab (CT de disco local, imagen min): de ~40 ms que tardaba rm,
// ~20 eran esperar a que el VMM muriese tras el SIGKILL (el sondeo era de 20 ms),
// ~7 el desmontaje de la red y apenas ~3 entre jail, overlay y RemoveAll. El
// directorio no era el grueso, pero el resto sigue sin tener por qué hacer
// esperar a quien borra: tras matar el VMM la máquina ya no gasta RAM ni CPU.
//
// Por eso Remove contesta en cuanto el VMM ha muerto y deja la máquina como
// TUMBA: sigue en byID (así nadie reutiliza su índice de red ni sus ficheros,
// y el vigilante no la barre) pero con un nombre que nadie puede pedir, sin
// volúmenes ni carpetas (que quedan libres al instante) y con Transition
// "removing", que es lo que `kling ps` enseña mientras dura. El nombre real
// queda libre al momento: un run con el mismo nombre justo después no choca.
//
// La tumba se escribe en el estado ANTES de contestar. Si el daemon muere a
// mitad, al arrancar recuperarTumbas termina la limpieza.

// prefijoTumba precede al ID en el nombre de una máquina que se está borrando.
const prefijoTumba = "~rm-"

// nombreTumba es el nombre de la tumba de id. Exacto, no solo por prefijo: un
// usuario que llame a una máquina "~rm-algo" no debe verla borrada al arrancar.
func nombreTumba(id string) string { return prefijoTumba + id }

func esTumba(mc *api.Machine) bool { return mc != nil && mc.Name == nombreTumba(mc.ID) }

// borrarEnSegundoPlano es el final de removeSi cuando nadie pide esperar: mata
// el VMM, deja la tumba escrita y lanza la limpieza. soltar es el cerrojo de
// ciclo de vida: lo conserva la limpieza hasta acabar, así un start o un thaw
// que llegue entre medias espera en vez de resucitar lo que se está borrando, y
// Close espera a las limpiezas igual que a cualquier operación de ciclo de vida.
func (m *Manager) borrarEnSegundoPlano(mc *api.Machine, soltar func()) {
	id := mc.ID
	quitarTransicion := m.marcarTransicion(id, api.TransitionRemoving)
	murio := m.kill(id)

	m.mu.Lock()
	name, dueño := mc.Name, mc.Labels[api.LabelOwner]
	if live := m.byID[id]; live != nil {
		name, dueño = live.Name, live.Labels[api.LabelOwner]
		live.Name = nombreTumba(id)
		live.State = api.StateStopped
		// Un VMM que sobrevivió al SIGKILL deja su PID en la tumba: la
		// limpieza no suelta nada mientras siga vivo (M4).
		if murio {
			live.PID = 0
		}
		live.Volumes, live.Shares, live.Forwards = nil, nil, nil
		live.Labels = nil
		live.TTLSeconds, live.TTLAt, live.OnTTL = 0, nil, ""
	}
	m.mu.Unlock()
	// Escrita ya, no con el debounce: contestar "borrada" con la tumba aún sin
	// escribir dejaría, si el daemon muere en esos 250 ms, una máquina viva con
	// su nombre de siempre.
	m.persistirYa()
	// El evento del borrado sale aquí, cuando el nombre ya está libre, y con
	// el nombre y el dueño de antes: la tumba ya no los tiene, y sin el dueño
	// el inquilino no lo vería (daemon/authz.go, eventoVisible).
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStopped, ID: id, Name: name, Message: "removed", Owner: dueño})

	go func() {
		defer soltar()
		// Con el VMM aún vivo la transición se queda: `kling ps` la sigue
		// enseñando como removing y el vigilante lo reintenta
		// (reintentarTumbas).
		if m.limpiarTumba(id) != errVMMVivo {
			quitarTransicion()
		}
	}()
}

// errVMMVivo es el de una tumba cuyo VMM no ha muerto: no se limpia aún.
var errVMMVivo = errors.New("its VMM is still alive after SIGKILL")

// rematarVMM vuelve a matar el VMM que una tumba conserva y dice si ya no
// existe. Solo si sigue siendo el VMM de esa máquina (liveVMs lo reconoce por
// su línea de órdenes): un PID que murió y se reutilizó es de otro proceso.
func (m *Manager) rematarVMM(id string, pid int) bool {
	if pid == 0 || m.liveVMs()[id] != pid {
		return true
	}
	senalMatar(pid)
	return waitGone(pid, min(plazoMuerte, time.Second))
}

// reintentarTumbas termina las tumbas cuyo VMM no murió al borrarlas. La llama
// el vigilante; solo mira las que conservan PID y nadie tiene tomadas.
func (m *Manager) reintentarTumbas() {
	m.mu.RLock()
	var ids []string
	for id, mc := range m.byID {
		if esTumba(mc) && mc.PID != 0 {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		soltar, ok := m.tryLock(id)
		if !ok {
			continue
		}
		quitar := m.marcarTransicion(id, api.TransitionRemoving)
		if m.limpiarTumba(id) != errVMMVivo {
			quitar()
		}
		soltar()
	}
}

// limpiarTumbaReintentos: veces que se intenta antes de dejar la tumba para el
// siguiente arranque; limpiarTumbaEspera, la pausa entre intentos.
const (
	limpiarTumbaReintentos = 3
	limpiarTumbaEspera     = time.Second
)

// antesDeLimpiar es un gancho de pruebas: se llama antes de cada intento de
// limpieza y permite sujetarla para ver la tumba o simular un corte.
var antesDeLimpiar = func(m *Manager, id string) {}

func (m *Manager) limpiarTumba(id string) error {
	antesDeLimpiar(m, id)
	for i := 0; i < limpiarTumbaReintentos; i++ {
		m.mu.RLock()
		var mc *api.Machine
		if live := m.byID[id]; live != nil {
			mc = live.Clone()
		}
		m.mu.RUnlock()
		if mc == nil {
			return nil
		}
		if mc.PID != 0 {
			if !m.rematarVMM(id, mc.PID) {
				log.Printf("rm: %s: VMM pid %d is still alive; its network and disk stay until it exits", shortID(id), mc.PID)
				return errVMMVivo
			}
			m.mu.Lock()
			if live := m.byID[id]; live != nil && live.PID == mc.PID {
				live.PID = 0
			}
			m.mu.Unlock()
			mc.PID = 0
		}
		err := m.limpiarMaquina(mc)
		if err == nil {
			return nil
		}
		log.Printf("rm: cleaning up %s failed (attempt %d/%d): %v", shortID(id), i+1, limpiarTumbaReintentos, err)
		select {
		case <-m.quit:
			return err
		case <-time.After(limpiarTumbaEspera):
		}
	}
	log.Printf("rm: %s stays as a tombstone until the next daemon start", shortID(id))
	return errors.New("cleanup failed")
}

// recuperarTumbas termina las limpiezas que un reinicio cortó. Va después de
// reconcile, que ya ha desmontado lo que tenía vivo; cada una toma su cerrojo
// y su marca de transición como si la hubiera lanzado un rm.
func (m *Manager) recuperarTumbas() {
	m.mu.RLock()
	var ids []string
	for id, mc := range m.byID {
		if esTumba(mc) {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		log.Printf("rm: finishing the removal of %s that a restart interrupted", shortID(id))
		quitar := m.marcarTransicion(id, api.TransitionRemoving)
		soltar := m.lock(id)
		go func(id string) {
			defer soltar()
			// Sin kill: una tumba solo conserva PID si su VMM sobrevivió al
			// SIGKILL, y tras un reinicio ese número puede ser ya de otro
			// proceso. limpiarTumba lo remata solo si sigue siendo su VMM.
			if m.limpiarTumba(id) != errVMMVivo {
				quitar()
			}
		}(id)
	}
}

// limpiarMaquina es todo lo que Remove hace tras matar el VMM: red, CPU, jail,
// directorio, auditoría, overlay, registro y aviso. Es idempotente (cada paso
// tolera que ya esté hecho), que es lo que permite reintentarla tras un
// reinicio. Con el cerrojo de ciclo de vida de mc.ID tomado.
func (m *Manager) limpiarMaquina(mc *api.Machine) error {
	m.desmontarRed(knet.Plan(mc.NetIndex, mc.ID), mc.ID)
	m.releaseCPU(mc.ID)
	// El chroot del jail vive aparte del directorio de la máquina: se limpia
	// también, o cada restauración jailed deja un árbol huérfano.
	if err := m.borrarJail(mc.ID); err != nil {
		log.Printf("warning: %v", err)
	}
	if err := os.RemoveAll(m.dir(mc.ID)); err != nil {
		return err
	}
	m.borrarAuditoria(mc.ID)
	// Su overlay en el almacén de discos, si lo tenía (ver cow.go).
	m.borrarOverlayAlmacen(mc.ID)
	// Su enlace corto en /tmp/kling-<uid> (macOS) ya no apunta a nada; el
	// vigilante lo barrería en la siguiente vuelta, pero así no queda ni ese rato.
	fc.BarrerEnlaces(m.root)
	m.mu.Lock()
	delete(m.byID, mc.ID)
	delete(m.socket, mc.ID)
	delete(m.discoMedido, mc.ID)
	delete(m.freezeFails, mc.ID)
	m.persist()
	m.mu.Unlock()
	// La marca de "sin agente" de su último freeze: sin esto se quedaba hasta
	// el siguiente thaw de un ID que ya no volverá.
	m.resyncSinAgente.Delete(claveThaw(mc.ID))
	// La numeración de sus vigías de "listo": sin podarla, cada máquina que
	// pasó por aquí dejaba su entrada para siempre. Se adelanta antes de
	// soltarla, para que una vigía aún viva se sepa jubilada.
	if v, ok := m.vigiasListo.LoadAndDelete(mc.ID); ok {
		v.(*atomic.Uint64).Add(1)
	}
	m.ganchosPendientes.Delete(mc.ID)
	m.invalidarSesiones(mc.ID, "removed")
	// Con el dueño: ya no está en byID, y el filtro por inquilino del bus no
	// tendría a quién preguntar (daemon/authz.go, eventoVisible). Una tumba
	// ya lo publicó al hacerse (borrarEnSegundoPlano).
	if !esTumba(mc) {
		m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStopped, ID: mc.ID, Name: mc.Name, Message: "removed",
			Owner: mc.Labels[api.LabelOwner]})
	}
	return nil
}

// EsperarBorrados espera, con plazo, a que acaben las limpiezas en segundo
// plano de kling rm (y cualquier otra operación de ciclo de vida en curso). Sirve
// a los que necesitan ver el disco ya limpio: tests, y quien cierre el daemon.
func (m *Manager) EsperarBorrados(plazo time.Duration) bool {
	return m.lifecycle.esperarLibres(plazo)
}
