package machine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/esquema"
	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"

	"github.com/juan52878911/kindling/pkg/panico"
)

// reconcile ajusta el estado guardado a la realidad del host al arrancar.
//
// El daemon puede reiniciarse, caerse o actualizarse mientras hay microVMs
// vivas. Sin esto, kling afirmaría cosas falsas: máquinas "running" cuyo proceso
// murió, o namespaces de máquinas que ya no existen.
func (m *Manager) reconcile() {
	resync, limpieza := m.reconciliarEstado()
	// Los desmontajes de red, sin m.mu y de 8 en 8: cada uno son procesos
	// (ip netns del, ip link del) y con cientos de máquinas en serie bajo el
	// candado el daemon tardaba segundos en escuchar (A14).
	limpieza.desmontarRedes(m)
	m.resyncTrasReconcile(resync)
	m.volumenesTrasReconcile(limpieza.volumenes)
	// Fuera de m.mu: escanea los procesos (ver killOrphanVMMs).
	m.killOrphanVMMs()
	// Tras matar a los huérfanos: el cgroup de un VMM que sigue vivo no se
	// borra, y releaseCPU reintentaba 20×25 ms por cada uno.
	for _, id := range limpieza.cpu {
		m.releaseCPU(id)
	}
}

// limpiezaReconcile es lo que reconciliarEstado deja para hacer sin m.mu.
type limpiezaReconcile struct {
	redes []redADesmontar
	cpu   []string
	// volumenes son las plantillas readoptadas tras un commit cortado que
	// hay que dejar con sus volúmenes montados (volumenesTrasReconcile).
	volumenes []string
}

type redADesmontar struct {
	n  *knet.Net
	id string
}

// sinVMM anota la limpieza de una máquina cuyo VMM ya no está.
func (l *limpiezaReconcile) sinVMM(mc *api.Machine) {
	l.redes = append(l.redes, redADesmontar{knet.Plan(mc.NetIndex, mc.ID), mc.ID})
	l.cpu = append(l.cpu, mc.ID)
}

// desmontarRedes desmonta las redes anotadas, de 8 en 8.
func (l *limpiezaReconcile) desmontarRedes(m *Manager) {
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, r := range l.redes {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			m.desmontarRed(r.n, r.id)
		}()
	}
	wg.Wait()
}

// reconciliarEstado es reconcile salvo la caza de VMMs huérfanos. Devuelve
// las máquinas readoptadas que necesitan un resync (un thaw cortado).
func (m *Manager) reconciliarEstado() (resyncPendientes []string, limpieza *limpiezaReconcile) {
	limpieza = &limpiezaReconcile{}
	// Lo primero es MIRAR, y hacerlo fuera del lock porque lee /proc.
	//
	// El estado en disco no es la verdad: se escribe con debounce y fuera del
	// candado, y sobre todo una microVM SOBREVIVE al daemon (firecracker se
	// lanza con setsid). Fiarse solo del fichero significa quitarle la red y el
	// techo de CPU a máquinas que están funcionando, y en el peor caso arrancar
	// un segundo firecracker sobre el mismo overlay.
	live := m.liveVMs()
	// Y a cada VMM vivo, en qué estado está su invitado (A7), también fuera
	// del lock: es una petición HTTP por VMM.
	estados := m.estadosVMM(live)
	// Y lo que readoptable necesita de disco y del VMM (el sello del volcado,
	// el Resume de un invitado en pausa), también fuera: un digest por
	// volcado y hasta plazoEstadoVMM por VMM, que con m.mu tomado hacían
	// esperar a todo lo que ya corre (el broker de kling-vz, los vigilantes
	// de los adoptados).
	previas := m.prepararReadopcion(live, estados)
	// Para las que corrían con el overlay sin sincronizar (fallidaTrasCorte).
	arranque, conArranque := arranqueHost()

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
		// El volcado temporal de un commit que el reinicio cortó (macOS,
		// publicar_volcado.go): ningún commit de este daemon está en curso
		// todavía, así que nadie lo va a publicar. Sin esto, la RAM entera de
		// la máquina se quedaba en su directorio hasta el rm (M15). Aunque el
		// VMM siga vivo y escribiéndolo: se borra el nombre, no su descriptor.
		quitarVolcadoTemporal(m.dir(mc.ID))

		// El VMM que el último Freeze dejó vivo (vmmDeCongelado, guardado en
		// disco) no se readopta: su volcado ya está sellado y la máquina es
		// warm. Readoptarlo la daba por running, y al morir el proceso
		// quedaba failed con su volcado. Sigue como si no estuviera vivo;
		// killOrphanVMMs, abajo, lo mata, y si sobrevive, Thaw (thawVMMViejo).
		if pid, alive := live[mc.ID]; alive && m.vmmCongeladoEnDisco(mc.ID, pid) {
			log.Printf("reconcile: %s (%s): the VMM its last freeze left alive (pid %d) is still there; not re-adopting it",
				mc.Name, shortID(mc.ID), pid)
			delete(live, mc.ID)
		} else if !alive {
			_ = os.Remove(filepath.Join(m.dir(mc.ID), ficheroVMMCongelado))
			// La marca de un commit cortado solo se retira con su VMM ya
			// muerto: antes, otro reinicio lo readoptaría con el disco dorado.
			quitarMarcaCommit(m.dir(mc.ID))
		}
		// Un VMM vivo que no se puede readoptar tal cual (A7): un commit que el
		// reinicio cortó, o un invitado en pausa que figura despierto.
		if pid, alive := live[mc.ID]; alive && !m.readoptable(mc, pid, estados[mc.ID], previas[mc.ID]) {
			delete(live, mc.ID)
		}
		if pid, alive := live[mc.ID]; alive {
			// Un commit cortado tras soltar los volúmenes (releaseVolumes): la
			// plantilla sigue sin ellos. Una en pausa no los soltó (el commit no
			// los suelta si ya estaba pausada) y VolumeWait no los tiene montados.
			if previas[mc.ID].commitRevertido && len(mc.Volumes) > 0 && !mc.VolumeWait && mc.State != api.StatePaused {
				limpieza.volumenes = append(limpieza.volumenes, mc.ID)
			}
			// Un thaw cortado tras cargar el volcado (el sello ya retirado, la
			// máquina aún warm en disco): su invitado corre, o corre ahora que
			// readoptable lo reanudó, con el reloj y la entropía del volcado.
			// Pasa por el resync que el thaw no llegó a hacer, en segundo
			// plano y tras soltar m.mu (resyncTrasReconcile).
			if mc.State == api.StateWarm || mc.State == api.StateFailed {
				resyncPendientes = append(resyncPendientes, mc.ID)
			}
			// Está viva: se readopta y NO se toca nada suyo.
			m.socket[mc.ID] = m.socketDe(mc.ID, pid)
			// Una pausada sigue pausada: su VMM vive, pero el invitado no
			// corre, y marcarla running la dejaría sorda figurando despierta.
			if mc.State != api.StateRunning && mc.State != api.StatePaused {
				log.Printf("reconcile: %s (%s) is still alive (pid %d) even though the state said %q; readopting it",
					mc.Name, mc.ID[:8], pid, mc.State)
				mc.State = api.StateRunning
			}
			mc.PID = pid
			// La red sobrevivió con el VMM: que freeze+thaw no la rehaga entera
			// y que soltarRedesDormidas la vea (A12).
			if existe(filepath.Join("/var/run/netns", nsName(mc.ID))) {
				m.redMontada.Store(mc.ID, true)
			}
			liveCg[cgName(mc.ID)] = true
			// No es hijo nuestro: su salida se espera con el kernel, no con
			// cmd.Wait (ver esperarVMM). Nunca llama a nada en el acto, así
			// que vale con m.mu tomado.
			m.vigilarAdoptado(mc.ID, pid)
			// Su netns y reglas sobrevivieron al daemon, pero el resolver dinámico
			// del modo allowlist es una goroutine nuestra y murió con nosotros.
			// Reanudarlo, o su DNS (DNATeado a un puerto sin nadie) se quedaría mudo.
			// Lo mismo con las aristas de un nodo de grafo (grafo_red.go):
			// sus proxies de enlace y, en egress none o internet, su resolver.
			spec, esNodo := m.especRedGrafoLocked(mc)
			// En egress none el resolver registra los nombres cortados
			// (internal/net/bloqueos.go): también hay que reanudarlo.
			esNone := mc.Egress == string(knet.EgressNone) || mc.Egress == ""
			if mc.Egress == string(knet.EgressAllowlist) || esNone || esNodo {
				var err error
				if mc.Egress == string(knet.EgressAllowlist) {
					err = knet.Plan(mc.NetIndex, mc.ID).StartAllowlistResolver(mc.AllowDomains)
				} else if esNone {
					err = knet.Plan(mc.NetIndex, mc.ID).ReanudarResolver(knet.EgressNone)
				}
				if err == nil && esNodo {
					if gerr := montarRedGrafo(knet.Plan(mc.NetIndex, mc.ID), spec); gerr != nil {
						log.Printf("reconcile: couldn't resume the graph edges of %s: %v", shortID(mc.ID), gerr)
					}
				}
				if err != nil {
					log.Printf("reconcile: couldn't resume the dns resolver for %s: %v", shortID(mc.ID), err)
				} else if n, err := m.reentregarCredenciales(context.Background(), mc, nil); err != nil {
					// Y sus credenciales, que también eran goroutines nuestras
					// (proxy) y estado del resolver. Su MMDS sigue en el VMM
					// vivo: solo se rehace el lado del host. Si falla, el
					// dominio no resuelve: falla cerrado, y se dice.
					log.Printf("reconcile: couldn't hand %s its credentials back: %v", shortID(mc.ID), err)
				} else if n > 0 {
					log.Printf("reconcile: %s (%s) got its %d credential(s) back", mc.Name, mc.ID[:8], n)
				}
			}
			continue
		}

		switch mc.State {
		case api.StateRunning, api.StatePaused:
			// Su proceso ya no está. Si dejó un snapshot completo está WARM, no
			// parada: decir "stopped" deja el snapshot varado, porque Thaw se
			// niega a descongelar lo que no esté warm y habría que editar el
			// state.json a mano para recuperarlo.
			if m.hasSnapshot(mc.ID) {
				log.Printf("reconcile: %s (%s) is no longer running but keeps its snapshot: warm",
					mc.Name, mc.ID[:8])
				mc.State = api.StateWarm
			} else if fallidaTrasCorte(mc, arranque, conArranque) {
				// Overlay sin sincronizar y el host se reinició: puede estar a
				// medias, y arrancarla sería montar un ext4 roto (desechable.go).
				log.Printf("reconcile: %s (%s) was running with a disposable overlay when the host went down: failed",
					mc.Name, mc.ID[:8])
				ahora := time.Now()
				mc.State, mc.LastErr, mc.FailedAt = api.StateFailed, errOverlaySinAsentar, &ahora
			} else {
				log.Printf("reconcile: %s (%s) is no longer running, marked as stopped", mc.Name, mc.ID[:8])
				mc.State = api.StateStopped
			}
			mc.PID = 0
			limpieza.sinVMM(mc)

		case api.StateCreated:
			// Sin VMM: el reinicio cortó el arranque entre persistirYa y el
			// spawn (o el host se reinició). Ni TTL ni gcFailed la recogían
			// (A10). Una copia sin entregar queda failed; el resto, stopped.
			if copiaSinEntregar(mc) {
				log.Printf("reconcile: %s (%s) was created from %s and its VMM is gone: failed",
					mc.Name, shortID(mc.ID), origenCopia(mc))
				ahora := time.Now()
				mc.State, mc.LastErr, mc.FailedAt = api.StateFailed, errCopiaCortada, &ahora
			} else {
				log.Printf("reconcile: %s (%s) was created and its VMM is gone: stopped", mc.Name, shortID(mc.ID))
				mc.State = api.StateStopped
			}
			mc.PID = 0
			limpieza.sinVMM(mc)

		case api.StateWarm, api.StateStopped, api.StateFailed:
			// Una warm sin volcado que valga no se descongela nunca: el caso
			// real es un thaw que retiró el sello (el invitado ya corrió) y un
			// daemon que murió antes de escribir el running. Seguir warm la
			// dejaba varada, con un thaw que siempre falla; es lo mismo que una
			// running sin proceso, y va a stopped (o a failed, si su overlay
			// desechable pudo quedar a medias). El volcado, que ya no carga
			// nadie, se retira como en Stop; uno de un kling más nuevo no se
			// toca (no está roto, este no lo sabe leer).
			//
			// Solo si el volcado ESTÁ y no vale (sello retirado, volcado a
			// medias): una warm sin snap.file ni mem.file no es ese caso, y
			// sigue como siempre (el thaw dice qué falta).
			dir := m.dir(mc.ID)
			hayVolcado := existe(filepath.Join(dir, "snap.file")) && existe(filepath.Join(dir, "mem.file"))
			if mc.State == api.StateWarm && hayVolcado {
				if err := volcadoValido(dir, mc.FrozenAt); err != nil && !esquema.EsMasNuevo(err) {
					ahora := time.Now()
					if fallidaTrasCorte(mc, arranque, conArranque) {
						log.Printf("reconcile: %s (%s) is warm without a usable memory dump and its disposable overlay may be incomplete: failed",
							mc.Name, shortID(mc.ID))
						mc.State, mc.LastErr, mc.FailedAt = api.StateFailed, errOverlaySinAsentar, &ahora
					} else {
						log.Printf("reconcile: %s (%s) is warm without a usable memory dump (%v): stopped, its disk is kept",
							mc.Name, shortID(mc.ID), err)
						mc.State = api.StateStopped
						mc.LastErr = "its memory dump could not be used (" + err.Error() + "); it was stopped with its disk intact, kling start boots it cold"
					}
					mc.FrozenAt = nil
					m.borrarVolcado(mc.ID)
				}
			}
			// Sin proceso: ni namespace ni cgroup hacen nada. Se recrean al
			// arrancarla o descongelarla.
			limpieza.sinVMM(mc)
		}
	}
	m.persist()

	m.sweepCgroups(liveCg)

	// Namespaces de máquinas que ya no existen: basura de ejecuciones
	// anteriores, pero solo los de ESTE daemon (ver barrerNamespaces).
	m.barrerNamespaces(seen)

	m.sweepMachineDirs()
	fc.BarrerEnlaces(m.root)
	return resyncPendientes, limpieza
}

// killOrphanVMMs mata los procesos de firecracker cuya microVM ya no existe o
// no debería estar corriendo.
//
// Un VMM vivo sin máquina registrada es un huérfano de verdad: Run y runFrom
// escriben la máquina en disco en el acto (persistirYa) antes de lanzar su VMM,
// así que ya no puede tratarse de una recién creada cuyo estado no llegó a
// escribirse.
//
// Aparecen cuando una instancia se marca failed —sweep le pone PID 0— y luego se
// elimina: el kill de Remove lee ese PID 0 y no mata nada, así que el VMM queda
// huérfano reteniendo su RAM para siempre, con el daemon sano y sin nada en su
// estado que lo explique. liveVMs sí lo ve (escanea /proc), y es la única forma
// de reconciliar con la realidad.
//
// Se llama SIN m.mu: el escaneo de /proc (un ps en macOS) va fuera, como en
// sweepOrphanVMMs, y el candado solo para decidir. Escanear con él tomado
// congelaba ps, run y thaw mientras duraba.
func (m *Manager) killOrphanVMMs() {
	if m.barridoBloqueado() {
		return
	}
	if m.pruebasEscaneoHuerfanos != nil {
		m.pruebasEscaneoHuerfanos()
	}
	live := m.liveVMs()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, pid := range live {
		mc := m.byID[id]
		// Vivo y debería estarlo: no se toca.
		if mc != nil && (mc.State == api.StateRunning || mc.State == api.StatePaused) {
			continue
		}
		// O no está registrado, o su estado dice que no corre: el proceso sobra.
		log.Printf("reconcile: killing orphan VMM of %s (pid %d, state %s)",
			shortID(id), pid, estadoDe(mc))
		_ = syscall.Kill(pid, syscall.SIGKILL)
		tel.huerfanos.Add(1)
	}
}

// sweepOrphanVMMs mata, en marcha, los VMM que ya no son de ninguna máquina o
// cuya máquina dice no estar corriendo.
//
// Es la versión periódica de killOrphanVMMs, y es MÁS prudente que ella a
// propósito. Al arrancar el daemon nadie está creando máquinas, así que allí se
// puede matar todo lo que no esté running. Aquí sí: una microVM en pleno
// arranque está registrada como "created" y su VMM ya existe. Por eso:
//
//   - "created" y "warm" no se tocan (naciendo, o en pleno thaw);
//   - lo reservado (makeMachineDir) tampoco, que es la misma señal que protege
//     los directorios del barrido;
//   - y lo demás tiene que parecer huérfano DOS vueltas seguidas, 10 s aparte,
//     antes de morir. Un fallo real no se cura solo; una transición en curso, sí.
func (m *Manager) sweepOrphanVMMs() {
	// El escaneo de /proc va fuera del candado: recorrerlo con el lock global
	// tomado congela ps, run y thaw mientras dura.
	if m.barridoBloqueado() {
		return
	}
	m.barrerHuerfanos(m.liveVMs())
}

// barrerHuerfanos es sweepOrphanVMMs sobre un escaneo ya hecho (liveVMs). El
// vigilante le pasa el de sweep, recién tomado: un solo recorrido de /proc
// (un solo ps en macOS) por vuelta, y no uno por cada tarea que lo necesita.
func (m *Manager) barrerHuerfanos(live map[string]int) {
	if m.barridoBloqueado() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.orphanSeen == nil {
		m.orphanSeen = map[string]int{}
	}
	for id := range m.orphanSeen {
		if _, sigue := live[id]; !sigue {
			delete(m.orphanSeen, id)
		}
	}
	for id, pid := range live {
		mc := m.byID[id]
		if m.reserved[id] > 0 {
			continue
		}
		// Con una operación de ciclo de vida en curso (su cerrojo tomado), lo
		// que diga el estado puede ser el de antes (M2): el thaw de una failed
		// la deja failed hasta que su VMM nuevo termina de cargar el volcado,
		// y matarlo a la segunda vuelta era cortarle el LoadSnapshot. Quien
		// tiene el cerrojo decide; si deja un huérfano, la vuelta siguiente lo ve.
		if m.lifecycle.ocupado(id) {
			delete(m.orphanSeen, id)
			continue
		}
		if mc != nil && (mc.State == api.StateRunning || mc.State == api.StatePaused ||
			mc.State == api.StateCreated || mc.State == api.StateWarm) {
			delete(m.orphanSeen, id)
			continue
		}
		m.orphanSeen[id]++
		if m.orphanSeen[id] < 2 {
			continue
		}
		log.Printf("watch: killing orphan VMM of %s (pid %d, state %s)", shortID(id), pid, estadoDe(mc))
		_ = syscall.Kill(pid, syscall.SIGKILL)
		tel.huerfanos.Add(1)
		delete(m.orphanSeen, id)
	}
}

func estadoDe(mc *api.Machine) string {
	if mc == nil {
		return "not registered"
	}
	return string(mc.State)
}

// reserveDir aparta un id para que sweepMachineDirs no borre su directorio
// mientras se está construyendo.
//
// Entre el MkdirAll de una máquina y su entrada en byID pasan cientos de
// milisegundos —copiar el overlay dorado son ~100 MiB, más resolver volúmenes y
// montar la red—, y en esa ventana el directorio no es de "ninguna máquina
// conocida": el barrido lo veía como basura y lo borraba bajo los pies de quien
// lo estaba llenando. El síntoma era un error sobre un firecracker.log que no
// existe, que no dice absolutamente nada de la causa real.
//
// Devuelve la función que suelta la reserva. Se usa con defer, para que cubra
// también los caminos de error que borran el directorio a medio hacer.
//
// Las reservas se CUENTAN: el mismo nombre lo pueden tener varios a la vez. Con
// ids de máquina no pasa (cada una tiene el suyo), pero con "snap:<nombre>" sí:
// cada restauración reserva el snapshot del que lee (M-15), y un commit el que
// escribe. Con un bool, la primera que terminara borraba la reserva de todas
// las demás y dejaba al resto sin protección a mitad de su trabajo. La función
// devuelta es idempotente: soltar dos veces no descuenta la reserva de otro.
func (m *Manager) reserveDir(id string) func() {
	m.mu.Lock()
	if m.reserved == nil {
		m.reserved = make(map[string]int)
	}
	m.reserved[id]++
	m.mu.Unlock()

	var una sync.Once
	return func() {
		una.Do(func() {
			m.mu.Lock()
			if m.reserved[id]--; m.reserved[id] <= 0 {
				delete(m.reserved, id)
			}
			m.mu.Unlock()
		})
	}
}

// makeMachineDir crea el directorio de una máquina y lo protege del barrido
// hasta que quien la construye suelte la reserva.
//
// Reservar y crear van SOLDADOS en la misma función a propósito: el fallo que
// esto arregla fue exactamente olvidar que entre las dos cosas hay una ventana.
// Mientras el único camino para tener un directorio de máquina pase por aquí, no
// hay forma de reintroducirlo.
func (m *Manager) makeMachineDir(id string) (string, func(), error) {
	release := m.reserveDir(id)
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		release()
		return "", nil, err
	}
	return dir, release, nil
}

// dirGrace es la edad mínima que ha de tener un directorio para ser candidato al
// barrido. Es el cinturón sobre los tirantes de reserveDir: cubre cualquier
// camino que en el futuro cree un directorio antes de registrar su id, sin que
// haya que acordarse de reservarlo. Retrasar unos minutos la recuperación de
// basura real no cuesta nada —el barrido corre cada 10 s—; borrar el directorio
// de una máquina que está naciendo cuesta la máquina.
const dirGrace = 2 * time.Minute

// sweepMachineDirs borra los directorios de machines/ que no pertenecen a
// ninguna máquina conocida ni viva.
//
// Cada directorio guarda un mem.file del tamaño de la RAM de su microVM, así que
// uno huérfano no es un despiste inofensivo: es un gigabyte que no se recupera
// nunca. Aparecen por un corte entre crear el directorio y persistir el estado,
// o por un registro que se pierde — y sin esto, se acumulan hasta llenar el
// disco, con el daemon sano y sin nada en su estado que lo explique.
//
// Se llama con m.mu tomado. Solo borra lo que NO está en byID, NO está reservado
// y lleva un rato quieto: una máquina viva cuyo registro aún no ha llegado al
// disco sigue teniendo su entrada en memoria, y una que aún se está construyendo
// tiene su id en reserved, así que ninguna de las dos es candidata.
func (m *Manager) sweepMachineDirs() {
	entries, _ := os.ReadDir(filepath.Join(m.root, "machines"))
	m.sweepMachineDirsDe(entries)
}

// escanearBarrido lee, sin m.mu, lo que el barrido mira: las entradas de
// machines/ y los snapshots que parecen un commit a medias.
func (m *Manager) escanearBarrido() (maquinas, restos []os.DirEntry) {
	maquinas, _ = os.ReadDir(filepath.Join(m.root, "machines"))
	return maquinas, m.candidatosRestos()
}

// sweepMachineDirsDe es sweepMachineDirs sobre entries, las de machines/
// leídas antes (con o sin m.mu). Se llama con m.mu tomado.
func (m *Manager) sweepMachineDirsDe(entries []os.DirEntry) {
	if m.barridoBloqueado() {
		return
	}
	dir := filepath.Join(m.root, "machines")
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, conocida := m.byID[e.Name()]; conocida {
			continue
		}
		if m.reserved[e.Name()] > 0 {
			continue
		}
		// Recién tocado: o lo está llenando alguien ahora mismo, o acaba de
		// quedarse huérfano y el próximo barrido lo recogerá igual.
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) < dirGrace {
			continue
		}
		// Solo se MUEVE a la papelera: renombrar es instantáneo y se puede hacer
		// con m.mu tomado. Borrar no: un directorio de máquina guarda un
		// mem.file del tamaño de su RAM, y recorrerlo y borrarlo bajo el cerrojo
		// global dejaba a ps, run y thaw esperando mientras el disco trabajaba.
		// Lo vacía vaciarPapelera, ya sin cerrojo.
		p := filepath.Join(dir, e.Name())
		papelera := filepath.Join(dir, papeleraDir)
		if err := os.MkdirAll(papelera, 0o700); err != nil {
			log.Printf("reconcile: couldn't create the trash directory: %v", err)
			return
		}
		destino := filepath.Join(papelera, fmt.Sprintf("%s-%d", e.Name(), time.Now().UnixNano()))
		if err := os.Rename(p, destino); err != nil {
			log.Printf("reconcile: couldn't move orphan directory %s to the trash: %v", e.Name(), err)
		}
	}
}

// papeleraDir es donde esperan los directorios huérfanos a ser borrados. Empieza
// por punto para que ningún barrido lo tome por el directorio de una máquina.
const papeleraDir = ".papelera"

// vaciarPapelera borra lo que sweepMachineDirs apartó. Se llama SIN m.mu: es la
// parte lenta, y nadie más toca esos directorios.
func (m *Manager) vaciarPapelera() {
	papelera := filepath.Join(m.root, "machines", papeleraDir)
	entries, err := os.ReadDir(papelera)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(papelera, e.Name())
		size := diskUsage(p)
		if err := os.RemoveAll(p); err != nil {
			log.Printf("reconcile: couldn't delete orphan directory %s: %v", e.Name(), err)
			continue
		}
		log.Printf("reconcile: orphan directory %s deleted (%d MiB recovered)",
			e.Name()[:min(12, len(e.Name()))], size>>20)
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

// marcaCommit es la marca de un commit con el disco de la plantilla
// reapuntado al overlay dorado (snapshot.go): se escribe, durable, antes de
// reapuntarlo y se retira en cuanto la plantilla recupera el suyo. Si el
// daemon muere en medio, el VMM sigue vivo y en pausa, y si alguien lo
// reanuda escribe en el overlay del dorado y no en el suyo. reconcile, al
// verla con el VMM vivo, no lo readopta: lo mata y la deja failed.
const marcaCommit = "commit.en-curso"

// errCopiaCortada es el LastErr de una copia de un dorado (run -from o un
// snapshot por imagen) cuyo arranque cortó un reinicio del daemon.
const errCopiaCortada = "the daemon restarted while this copy was starting from its snapshot, before it was resynced " +
	"and handed its credentials and volumes; its VMM was stopped. Remove it and run it again"

// copiaSinEntregar dice si mc es una copia de un dorado que runFrom publicó
// (created) y nunca llegó a entregar: StartedAt solo se pone al entregarla, y
// un Start posterior (que vuelve a created) arranca en frío con StartedAt ya
// puesto. También la que el arranque dejó failed sin entregarla: su VMM
// pudo seguir vivo, y readoptarla la daba por running sin resync (F20).
func copiaSinEntregar(mc *api.Machine) bool {
	return (mc.State == api.StateCreated || mc.State == api.StateFailed) && mc.StartedAt == nil && origenCopia(mc) != ""
}

// origenCopia es el snapshot del que se restauró mc, o "" si arrancó en frío.
func origenCopia(mc *api.Machine) string {
	if mc.From != "" {
		return mc.From
	}
	return mc.ImageSnapshot
}

// errCommitCortado es el LastErr de una plantilla cuyo commit cortó un
// reinicio del daemon.
const errCommitCortado = "the daemon restarted in the middle of a commit, with the machine paused and its disk " +
	"pointed at the snapshot's copy; its VMM was stopped so it could not write there. Its own disk is " +
	"as it was at the pause: kling start boots it again"

// marcarCommitEnCurso deja la marca en dir, en disco antes de seguir, con el
// PID del VMM que tiene el disco reapuntado (0 si no se sabe). Con el PID,
// reconcile solo hace caso a la marca si el VMM vivo es ese: una marca que
// sobrevivió a su VMM no tumba al siguiente (A3). deFork la marca como el
// temporal de un fork, que tiene el plazo de volcado corto (plazoCommitCortado).
func marcarCommitEnCurso(dir, nombre string, pid int, deFork bool) error {
	contenido := nombre + "\n"
	if pid > 0 {
		contenido += strconv.Itoa(pid) + "\n"
	}
	if deFork {
		contenido += lineaMarcaFork + "\n"
	}
	return escribirMarca(filepath.Join(dir, marcaCommit), []byte(contenido), 0o600)
}

// lineaMarcaFork es la línea que marca el commit cortado como el de un fork.
const lineaMarcaFork = "fork"

// plazoCommitCortado es lo que reconcile espera a un VMM que contesta
// «ocupado» con el volcado de un commit cortado: lo mismo que el commit le
// daba (snapshot.go). El de un dorado, plazoVolcadoDorado (C9-19); el
// temporal de un fork, plazoVolcado. Una marca sin tipo (las de antes) o
// ilegible cuenta como de dorado: esperar de más a quien contesta no cuesta.
func plazoCommitCortado(dir string, memMiB int) time.Duration {
	raw, err := os.ReadFile(filepath.Join(dir, marcaCommit))
	if err == nil {
		lineas := strings.Split(strings.TrimSpace(string(raw)), "\n")
		for _, l := range lineas[1:] {
			if strings.TrimSpace(l) == lineaMarcaFork {
				return plazoVolcado(memMiB)
			}
		}
	}
	return plazoVolcadoDorado(memMiB)
}

// marcaCommitDe dice si dir tiene marca de commit y de qué VMM es. pid 0:
// marca sin PID (las de antes de A3), que vale para cualquier VMM.
func marcaCommitDe(dir string) (hay bool, pid int) {
	raw, err := os.ReadFile(filepath.Join(dir, marcaCommit))
	if err != nil {
		// Una que existe y no se puede leer cuenta como marca sin PID: mejor
		// matar un VMM de más que readoptar uno que escribe en el dorado.
		return existe(filepath.Join(dir, marcaCommit)), 0
	}
	lineas := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lineas) >= 2 {
		if n, err := strconv.Atoi(strings.TrimSpace(lineas[1])); err == nil && n > 0 {
			return true, n
		}
	}
	return true, 0
}

// quitarMarcaCommit la retira, si estaba.
func quitarMarcaCommit(dir string) { _ = os.Remove(filepath.Join(dir, marcaCommit)) }

// conPID dice si la máquina id sigue registrada con un VMM (PID != 0).
func (m *Manager) conPID(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mc := m.byID[id]
	return mc != nil && mc.PID != 0
}

// plazoEstadoVMM es lo que espera reconcile a que cada VMM diga su estado.
// Uno que no contesta en esto se readopta como siempre: no se sabe más.
const plazoEstadoVMM = 3 * time.Second

// estadosVMM pregunta a cada VMM vivo en qué estado está su invitado (GET /:
// "Running", "Paused"…), a todos a la vez. Los que no contestan no salen.
func (m *Manager) estadosVMM(live map[string]int) map[string]string {
	out := make(map[string]string, len(live))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for id, pid := range live {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), plazoEstadoVMM)
			defer cancel()
			st, err := m.clienteReconcile(id, pid).State(ctx)
			if err != nil || st == "" {
				return
			}
			mu.Lock()
			out[id] = st
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// clienteReconcile es el cliente del VMM vivo de id que usa reconcile.
func (m *Manager) clienteReconcile(id string, pid int) *fc.Client {
	if m.pruebasSocketVMM != nil {
		if s := m.pruebasSocketVMM(id); s != "" {
			return fc.New(s)
		}
	}
	return fc.New(m.socketDe(id, pid))
}

// readoptable decide, con m.mu, si el VMM vivo pid de mc se readopta (A7).
// Antes se readoptaba sin preguntarle nada, y un reinicio del daemon a mitad
// de un Freeze o de un Commit (que pausan el invitado y persisten running)
// dejaba una máquina running con el invitado en pausa: el gateway le enrutaba
// peticiones que nadie contestaba, y la de un Commit, si alguien la reanudaba,
// escribía en el overlay del dorado. Ahora:
//
//   - con la marca de commit: no. Queda failed y killOrphanVMMs mata el VMM.
//   - en pausa figurando despierta, con su volcado sellado (un Freeze que
//     selló y no llegó a escribir warm, o un Thaw cortado antes de retirar
//     el sello): no. reconcile la deja warm y el VMM muere; reanudarlo
//     haría divergir el disco de un volcado que sigue valiendo.
//   - en pausa figurando despierta, sin volcado que valga (un Freeze o una
//     pausa cortados): se reanuda. Si no contesta, queda pendiente como
//     tras un volcado fallido (reanudar_pendiente.go).
//   - despierta figurando en pausa (un Resume cortado antes de persistir):
//     pasa a running.
//
// Lo que eso pide de disco y del VMM (el sello y el Resume) ya viene hecho en
// previa, de prepararReadopcion, fuera de m.mu.
func (m *Manager) readoptable(mc *api.Machine, pid int, estado string, previa previaReadopcion) bool {
	dir := m.dir(mc.ID)
	hay, pidMarca := marcaCommitDe(dir)
	if hay && pidMarca > 0 && pidMarca != pid {
		// La marca es de un VMM que ya no está (el del commit cortado, que
		// murió): este es otro, arrancado después. Sobra.
		log.Printf("reconcile: %s (%s): dropping the mark of a cut commit from VMM pid %d; its VMM now is pid %d",
			mc.Name, shortID(mc.ID), pidMarca, pid)
		quitarMarcaCommit(dir)
		hay = false
	}
	if hay && previa.commitRevertido {
		log.Printf("reconcile: %s (%s): its VMM (pid %d) was in the middle of a commit; gave it its own disk back and readopted it",
			mc.Name, shortID(mc.ID), pid)
		return true
	}
	if hay {
		log.Printf("reconcile: %s (%s): its VMM (pid %d) was in the middle of a commit; stopping it: failed",
			mc.Name, shortID(mc.ID), pid)
		ahora := time.Now()
		mc.State, mc.LastErr, mc.FailedAt = api.StateFailed, errCommitCortado, &ahora
		mc.PID = 0
		return false
	}
	if copiaSinEntregar(mc) {
		// S3: una copia de un dorado cuyo arranque cortó el reinicio. Su VMM
		// puede haber reanudado ya el invitado, pero sin el resync (reloj y
		// CSPRNG del dorado), sin credenciales y sin volúmenes: readoptarla
		// como running la entregaría así. Se para y queda failed.
		log.Printf("reconcile: %s (%s): a copy of %s whose start the restart cut (VMM pid %d); stopping it: failed",
			mc.Name, shortID(mc.ID), origenCopia(mc), pid)
		// Una que ya estaba failed conserva su error: dice por qué falló.
		if mc.State != api.StateFailed || mc.LastErr == "" {
			ahora := time.Now()
			mc.State, mc.LastErr, mc.FailedAt = api.StateFailed, errCopiaCortada, &ahora
		}
		mc.PID = 0
		return false
	}
	if estado == estadoNoArrancado {
		// F6: el VMM vive pero nunca cargó un volcado ni arrancó un
		// invitado (el reinicio cortó un thaw o un arranque entre lanzarlo y
		// cargar). Readoptarlo como running la daba por despierta sin nadie
		// dentro. Se deja como si no estuviera vivo: killOrphanVMMs lo mata, y
		// una warm cuyo sello devolvió prepararReadopcion sigue warm.
		log.Printf("reconcile: %s (%s): its VMM (pid %d) never started a guest; not re-adopting it",
			mc.Name, shortID(mc.ID), pid)
		if previa.selloDevuelto {
			log.Printf("reconcile: %s (%s): its memory dump was never loaded; its seal is back and it stays warm",
				mc.Name, shortID(mc.ID))
		}
		return false
	}
	switch {
	case estado == "Paused" && mc.State != api.StatePaused:
		if previa.sellado {
			log.Printf("reconcile: %s (%s): its VMM (pid %d) is paused and its memory dump is sealed; not re-adopting it",
				mc.Name, shortID(mc.ID), pid)
			return false
		}
		err := previa.errReanudar
		if !previa.reanudado {
			// No se intentó (no debería pasar: prepararReadopcion mira todas
			// las de este caso). Sin E/S aquí: lo reintenta el vigilante.
			err = errors.New("not attempted while reconciling")
		}
		if err == nil {
			log.Printf("reconcile: %s (%s) was paused although its state said %q; resumed it",
				mc.Name, shortID(mc.ID), mc.State)
			return true
		}
		// Running con la transición freezing: el planificador no le enruta
		// nada mientras el vigilante reintenta (reanudarPendientes).
		log.Printf("reconcile: %s (%s) is paused and resuming it failed (%v): the watcher will retry",
			mc.Name, shortID(mc.ID), err)
		if m.reanudaciones == nil {
			m.reanudaciones = map[string]reanudacion{}
		}
		if m.transicion == nil {
			m.transicion = map[string]string{}
		}
		m.reanudaciones[mc.ID] = reanudacion{pid: pid, desde: time.Now(),
			causa: fmt.Sprintf("the daemon restarted with the machine paused, and resuming it failed: %v", err)}
		m.transicion[mc.ID] = api.TransitionFreezing
	case estado == "Running" && mc.State == api.StatePaused:
		log.Printf("reconcile: %s (%s) is running although its state said paused", mc.Name, shortID(mc.ID))
		mc.State = api.StateRunning
	}
	return true
}

// reintentoCommitSinRespuesta es lo que reconcile reintenta, en total, con un
// VMM de un commit cortado que no contesta (R3-5): reconcile corre antes de
// que el daemon escuche, y uno colgado no puede retrasarlo un plazoVolcado.
const reintentoCommitSinRespuesta = 10 * time.Second

// revertirCommitCortado devuelve a un VMM vivo, cuyo commit cortó el reinicio,
// su overlay propio (el invitado no llegó a escribir en el dorado: estaba en
// pausa) y lo reanuda si no era una pausa pedida. Quita la marca solo si todo
// salió bien; si no, devuelve false y readoptable lo da por failed.
func (m *Manager) revertirCommitCortado(id, dir string, pid int, estado string, pausada bool, memMiB int) bool {
	c := m.clienteReconcile(id, pid)
	overlay := filepath.Join(dir, "overlay.ext4")
	inicio := time.Now()
	intentar := func(plazo time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), plazo)
		defer cancel()
		return c.PatchDrive(ctx, "overlay", overlay)
	}
	err := intentar(plazoEstadoVMM)
	switch {
	case err != nil && vmmContestaOcupado(err):
		// Un VMM que CONTESTA «ocupado» sigue volcando la memoria (una
		// plantilla grande): se le espera lo que el commit le daba al volcado
		// (R29, C9-19), no los plazoEstadoVMM de una consulta.
		plazo := plazoCommitCortado(dir, memMiB)
		log.Printf("reconcile: %s: its VMM is still busy with the dump of a cut commit; waiting up to %s", shortID(id), plazo)
		fin := inicio.Add(plazo)
		for err != nil && vmmContestaOcupado(err) && time.Until(fin) > 500*time.Millisecond {
			time.Sleep(500 * time.Millisecond)
			err = intentar(min(plazoEstadoVMM, time.Until(fin)))
		}
	case errors.Is(err, context.DeadlineExceeded):
		// Sin respuesta no se sabe si vuelca o está colgado: se reintenta,
		// pero como mucho reintentoCommitSinRespuesta en total. Después,
		// failed: killOrphanVMMs para el VMM y un kling start la arranca.
		fin := inicio.Add(reintentoCommitSinRespuesta)
		for errors.Is(err, context.DeadlineExceeded) && time.Until(fin) > 0 {
			err = intentar(min(plazoEstadoVMM, time.Until(fin)))
		}
		if err != nil {
			log.Printf("reconcile: %s: the VMM of a cut commit hasn't answered in %s; giving up on it",
				shortID(id), time.Since(inicio).Round(time.Millisecond))
		}
	}
	if err != nil {
		log.Printf("reconcile: %s: couldn't give the overlay back to the VMM of a cut commit: %v", shortID(id), err)
		return false
	}
	if estado == "Paused" && !pausada {
		ctx, cancel := context.WithTimeout(context.Background(), plazoEstadoVMM)
		defer cancel()
		if err := c.Resume(ctx); err != nil {
			log.Printf("reconcile: %s: couldn't resume the VMM of a cut commit: %v", shortID(id), err)
			return false
		}
	}
	quitarMarcaCommit(dir)
	return true
}

// vmmContestaOcupado dice si err es la RESPUESTA de un VMM que dice estar
// ocupado (un volcado en marcha). Un plazo vencido no lo es: no se sabe si
// vuelca o está colgado (ver revertirCommitCortado).
func vmmContestaOcupado(err error) bool {
	if !errorDelVMM(err) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	t := strings.ToLower(err.Error())
	return strings.Contains(t, "busy") || strings.Contains(t, "in progress")
}

// devolverVolumenes devuelve los volúmenes a una plantilla readoptada tras un
// commit cortado; variable para los tests.
var devolverVolumenes = func(m *Manager, mc *api.Machine) error { return m.acquireVolumes(mc) }

// volumenesTrasReconcile hace, fuera de m.mu y a la vez, acquireVolumes de las
// máquinas ids (R6): un commit cortado tras releaseVolumes las dejó sin
// volúmenes, y seguir así las haría escribir en su overlay. Si no se pueden
// devolver, la máquina no sigue en marcha: failed, como en commit.
func (m *Manager) volumenesTrasReconcile(ids []string) {
	var wg sync.WaitGroup
	for _, id := range ids {
		m.mu.RLock()
		var copia api.Machine
		viva := m.byID[id]
		if viva != nil {
			copia = *viva
		}
		m.mu.RUnlock()
		if viva == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := devolverVolumenes(m, &copia); err != nil {
				log.Printf("reconcile: %s: its commit was cut and the template couldn't mount its volumes again: %v", shortID(id), err)
				m.fail(viva, fmt.Errorf("commit was cut and the template could not get its volumes back: %w", err))
			}
		}()
	}
	wg.Wait()
}

// estadoNoArrancado es lo que contesta el VMM (Firecracker, kling-vz y
// kling-hv) antes de cargar un volcado o arrancar un invitado.
const estadoNoArrancado = "Not started"

// previaReadopcion es lo que readoptable necesita saber de un VMM en pausa
// que figura despierto, mirado antes de tomar m.mu (prepararReadopcion).
type previaReadopcion struct {
	// sellado: su volcado sigue sellado y vale; no se reanuda.
	sellado bool
	// selloDevuelto: su VMM no llegó a arrancar (estadoNoArrancado) en un
	// thaw y el sello que este retiró volvió a su sitio (restaurarSello).
	selloDevuelto bool
	// reanudado: se le pidió el Resume, con el resultado en errReanudar.
	reanudado   bool
	errReanudar error
	// commitRevertido: el VMM de un commit cortado recuperó su overlay propio
	// (y se reanudó, si no estaba en pausa): se readopta como cualquier otro.
	commitRevertido bool
}

// prepararReadopcion hace, fuera de m.mu, la E/S de readoptable para cada VMM
// vivo cuyo invitado está en pausa sin que la máquina figure en pausa:
// comprobar el sello del volcado (un digest) y, si no vale, reanudarlo (hasta
// plazoEstadoVMM por VMM, todos a la vez como estadosVMM). Se salta lo que
// reconcile no va a reanudar: el VMM que dejó vivo un Freeze
// (vmmCongeladoEnDisco) y el de un commit cortado (marcaCommitDe). Al
// arrancar el daemon nadie más cambia las máquinas, así que lo leído aquí
// sigue valiendo cuando reconcile toma m.mu.
func (m *Manager) prepararReadopcion(live map[string]int, estados map[string]string) map[string]previaReadopcion {
	type candidata struct {
		id, dir   string
		pid       int
		congelada *time.Time
		pausada   bool
		memMiB    int
	}
	var cands, commits []candidata
	var sinArrancar []string
	m.mu.RLock()
	for id, pid := range live {
		mc := m.byID[id]
		// Un commit cortado con su VMM vivo (A11): se le devuelve su disco.
		if mc != nil && !copiaSinEntregar(mc) {
			if hay, pidMarca := marcaCommitDe(m.dir(id)); hay && (pidMarca <= 0 || pidMarca == pid) &&
				pidCongeladoApuntado(m.dir(id)) != pid {
				commits = append(commits, candidata{id: id, dir: m.dir(id), pid: pid, pausada: mc.State == api.StatePaused,
					memMiB: max(mc.MemMiB, mc.MemMaxMiB)})
				continue
			}
		}
		// Un thaw cortado antes de cargar: el VMM no ha cargado nada, así
		// que el volcado sigue intacto y su sello (que el thaw convirtió en
		// la marca en curso) puede volver.
		if mc != nil && estados[id] == estadoNoArrancado && mc.State == api.StateWarm {
			sinArrancar = append(sinArrancar, id)
			continue
		}
		// Una copia sin entregar no se reanuda: readoptable la para (S3).
		if mc == nil || estados[id] != "Paused" || mc.State == api.StatePaused || copiaSinEntregar(mc) {
			continue
		}
		var congelada *time.Time
		if mc.FrozenAt != nil {
			t := *mc.FrozenAt
			congelada = &t
		}
		cands = append(cands, candidata{id: id, dir: m.dir(id), pid: pid, congelada: congelada})
	}
	m.mu.RUnlock()

	out := make(map[string]previaReadopcion, len(cands)+len(sinArrancar))
	for _, id := range sinArrancar {
		dir := m.dir(id)
		if !existe(filepath.Join(dir, marcaEnCurso)) {
			continue
		}
		if err := restaurarSello(dir); err != nil {
			log.Printf("reconcile: %s: couldn't put back the seal of a memory dump its VMM never loaded: %v", shortID(id), err)
			continue
		}
		out[id] = previaReadopcion{selloDevuelto: true}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range commits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok := m.revertirCommitCortado(c.id, c.dir, c.pid, estados[c.id], c.pausada, c.memMiB)
			mu.Lock()
			out[c.id] = previaReadopcion{commitRevertido: ok}
			mu.Unlock()
		}()
	}
	for _, c := range cands {
		if pidCongeladoApuntado(c.dir) == c.pid {
			continue
		}
		if hay, pidMarca := marcaCommitDe(c.dir); hay && (pidMarca <= 0 || pidMarca == c.pid) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var p previaReadopcion
			if existe(filepath.Join(c.dir, marcaOK)) && volcadoValido(c.dir, c.congelada) == nil {
				p.sellado = true
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), plazoEstadoVMM)
				p.errReanudar = m.clienteReconcile(c.id, c.pid).Resume(ctx)
				cancel()
				p.reanudado = true
			}
			mu.Lock()
			out[c.id] = p
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// pidCongeladoApuntado es el PID que guarda la marca de VMM vivo de un Freeze
// (ficheroVMMCongelado) en dir, o 0. Solo lee: vmmCongeladoEnDisco, con m.mu,
// es quien decide y limpia.
func pidCongeladoApuntado(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, ficheroVMMCongelado))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

// resyncTrasReconcile manda, en segundo plano, el resync de un thaw a las
// máquinas ids que reconcile readoptó a medio despertar (un thaw cortado
// tras cargar el volcado): sin él seguían con el reloj de cuando se
// congelaron y el CSPRNG del volcado. Como resyncTrasReanudar, atado a la
// ejecución readoptada: si para entonces es otra, no se manda.
func (m *Manager) resyncTrasReconcile(ids []string) {
	for _, id := range ids {
		if ej, ok := m.ejecucionDe(id); ok {
			log.Printf("reconcile: %s: re-adopted in the middle of a thaw; resyncing its clock and entropy", shortID(id))
			go m.resyncEnFondo(id, ej, api.ResyncThaw)
		}
	}
}

// hasSnapshot dice si una máquina tiene un congelado completo en disco.
func (m *Manager) hasSnapshot(id string) bool {
	// No basta con que los ficheros existan: un volcado interrumpido también
	// los deja. Ver volcado.go. Sin FrozenAt: aquí la máquina figura running o
	// paused, y el de una pausa no dice cuándo se escribió el volcado.
	if err := volcadoValido(m.dir(id), nil); err != nil {
		if !errors.Is(err, errVolcadoIncompleto) || !strings.Contains(err.Error(), "missing") {
			log.Printf("reconcile: %s: %v", shortID(id), err)
		}
		return false
	}
	return true
}

// watch vigila periódicamente que lo que decimos que corre, corra de verdad.
//
// Una microVM puede morir por su cuenta: pánico del kernel invitado, OOM del
// host, o alguien matando el proceso. Reportar "running" sobre algo muerto es
// peor que no reportar nada, porque el gateway enrutaría peticiones a la nada.
//
// El intervalo se recalcula en cada vuelta: en batería es más largo
// (intervaloVigilante), con un temporizador y no un ticker para que el cambio
// valga desde la vuelta siguiente.
func (m *Manager) watch(ctx context.Context, every time.Duration) {
	m.reloj.durmio() // la base del reloj del host
	// Un solo temporizador, rearmado en cada vuelta sobre la rejilla de su
	// intervalo (esperaAlineada): la batería y la QoS van dentro de la vuelta
	// (vueltaBateria), no en un reloj aparte.
	t := time.NewTimer(esperaAlineada(time.Now(), m.intervaloVigilante(every)))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			t.Reset(esperaAlineada(time.Now(), m.intervaloVigilante(every)))
			m.vueltaVigilante(ctx)
		}
	}
}

// pasoVigilante es una tarea de la vuelta del vigilante.
type pasoVigilante struct {
	nombre string
	f      func()
}

// vueltaVigilante es una vuelta del vigilante, paso a paso.
//
// Cada paso va bajo su propio Contener: si uno entra en pánico se pierde ESE
// paso, no el vigilante ni el resto de la vuelta. Antes iba todo bajo uno
// solo, y un pánico en sweep (el primero) se llevaba por delante el TTL, el
// gc y la presión de memoria de todas las máquinas, vuelta tras vuelta. Un
// daemon que sigue vivo pero ha dejado de reconciliar no da ningún síntoma, y
// eso es peor que caerse.
func (m *Manager) vueltaVigilante(ctx context.Context) {
	// El escaneo de sweep, para barrerHuerfanos. Si sweep cae, o en esta
	// vuelta no escaneó (sin VMs, ver sweep), nil: sin escaneo reciente no se
	// mata a nadie (barrerHuerfanos no tiene a quién).
	var vmms map[string]int
	pasos := []pasoVigilante{
		{"sweep", func() { vmms = m.sweep() }},
		// Procesos de firecracker que ya no son de nadie, con el mismo escaneo
		// que acaba de usar sweep. Hasta ahora esto solo corría al arrancar el
		// daemon, así que un VMM huérfano —cada restauración fallida dejaba
		// uno— retenía su RAM hasta el siguiente reinicio, invisible para
		// `kling ps` y para la contabilidad de memoria que decide si cabe la
		// siguiente microVM. Justo detrás de sweep y no al final de la vuelta:
		// el PID que mata tiene que ser de un escaneo reciente.
		{"barrerHuerfanos", func() {
			if vmms != nil {
				m.barrerHuerfanos(vmms)
			}
		}},
		// Las que un volcado fallido dejó en pausa sin poder reanudarlas
		// (reanudar_pendiente.go).
		{"reanudarPendientes", m.reanudarPendientes},
		// Y las borradas cuyo VMM no murió al primer SIGKILL (M4).
		{"reintentarTumbas", m.reintentarTumbas},
		// Las carpetas vivas de las máquinas que corren y no tienen conexión
		// (tras reiniciar el daemon, sobre todo).
		{"ensureShares", m.ensureShares},
		{"gcUploads", m.gcUploads},
		// En segundo plano y de dos en dos (expireTTLEnSegundoPlano): un
		// freeze de varios GiB no retiene la vuelta.
		{"ttl", func() { m.expireTTLEnSegundoPlano(ctx) }},
		// Recoger los cadáveres: una failed conserva su motivo un tiempo para
		// poder diagnosticarla, y después se recoge sola. Sin esto se
		// acumulaban indefinidamente, una por intento fallido.
		{"gcFailed", m.gcFailed},
		// La red que Freeze dejó montada, en las que llevan mucho tiempo
		// congeladas (ver red.go).
		{"soltarRedesDormidas", m.soltarRedesDormidas},
		// Las barreras del host, con máquinas con red en pie: un reload del
		// cortafuegos las borra, y antes solo se reponían al montar otra red.
		{"barrerasHost", m.vigilarBarrerasHost},
		// Errores de disco que el invitado vio (errores_disco.go), antes de
		// rotar: si no, se perderían.
		{"revisarErroresDisco", m.revisarErroresDisco},
		// Consolas serie que se han pasado de consolaMaxBytes (M-07): un
		// invitado hostil escribiendo a /dev/ttyS0 sin tope llenaría el disco
		// y degradaría a las demás microVMs (SECURITY.md §4).
		{"rotarConsolas", m.rotarConsolas},
		// Barrer directorios huérfanos también en marcha: no solo aparecen al
		// arrancar. Bajo el lock, como reconcile.
		// El escaneo (ReadDir de los dos y dos stat por snapshot) sin el
		// cerrojo (P9): bajo él solo se decide y se mueve lo que el escaneo
		// encontró, que casi nunca es nada.
		{"sweepDirs", func() {
			maquinas, restos := m.escanearBarrido()
			if m.pruebasTrasEscaneo != nil {
				m.pruebasTrasEscaneo()
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			m.sweepMachineDirsDe(maquinas)
			m.sweepSnapshotLeftoversDe(restos)
		}},
		// Los snapshots temporales de fork sin copias (ver fork.go).
		{"barrerForks", m.barrerForks},
		// Los snapshots por imagen que ya no valen o no caben en su tope
		// (snapshot_imagen.go).
		{"podarSnapshotsImagen", func() { m.podarSnapshotsImagen(topeSnapshotsImagen()) }},
		{"barrerPrepImagen", m.barrerPrepImagen},
		{"vaciarPapelera", m.vaciarPapelera},
		// Overlays del almacén de discos sin máquina, y bases de dorados que
		// ya no existen (ver cow.go).
		{"barrerAlmacen", m.barrerAlmacen},
		// Los enlaces cortos a sockets de máquinas que ya no existen (macOS,
		// rutas largas: ver fc.BarrerEnlaces).
		{"barrerEnlaces", func() { fc.BarrerEnlaces(m.root) }},
		// Y, si el disco aprieta, recuperar espacio eliminando instancias
		// dormidas que se pueden recrear desde su snapshot.
		{"gcDisk", func() { m.gcDisk(ctx) }},
		// El disco se recalcula aquí y no en List(): así `kling ps` no paga un
		// recorrido por máquina, y el dato sigue fresco para las que están
		// escribiendo.
		{"refreshDiskUsage", m.refreshDiskUsage},
		// Si el host durmió, la hora de los invitados que corren; y el modo
		// batería (bateria.go).
		{"revisarSueno", func() { m.revisarSueno(ctx) }},
		{"vueltaBateria", func() { m.vueltaBateria(ctx) }},
		// Y la presión de memoria: apretar o congelar ociosas antes de que
		// jetsam (o el OOM killer) mate VMMs.
		{"presion", func() { m.vigilarPresion(ctx) }},
	}
	for _, p := range pasos {
		panico.Contener("machine.watch: "+p.nombre, func() {
			if m.pruebasPasoVigilante != nil && !m.pruebasPasoVigilante(p.nombre) {
				return
			}
			p.f()
		})
	}
}

// asegurarBarrerasHost es knet.AsegurarHost detrás de una variable, para los
// tests.
var asegurarBarrerasHost = knet.AsegurarHost

// errBarrerasHost es el último fallo de vigilarBarrerasHost, para no repetir
// el mismo error en el log cada vuelta.
var errBarrerasHost atomic.Pointer[string]

// vigilarBarrerasHost comprueba (y repone) las reglas del host si este daemon
// tiene alguna red en pie: una que montó él (redMontada) o la de una máquina
// running o pausada, que tras reiniciar el daemon es readoptada y no está en
// redMontada. Sin ninguna, no hay a quién proteger y no se lanza ningún
// iptables. Un fallo se dice una vez, y su arreglo también.
func (m *Manager) vigilarBarrerasHost() {
	hay := false
	m.redMontada.Range(func(any, any) bool { hay = true; return false })
	if !hay {
		m.mu.RLock()
		for _, mc := range m.byID {
			if mc.State == api.StateRunning || mc.State == api.StatePaused {
				hay = true
				break
			}
		}
		m.mu.RUnlock()
	}
	if !hay {
		return
	}
	err := asegurarBarrerasHost()
	if err == nil {
		if errBarrerasHost.Swap(nil) != nil {
			log.Printf("watch: the host firewall rules for the machines are back")
		}
		return
	}
	msg := err.Error()
	if previo := errBarrerasHost.Swap(&msg); previo == nil || *previo != msg {
		log.Printf("watch: ERROR: the host firewall rules for the machines: %v (retrying every round)", err)
	}
}

// vueltasEscaneoSinVMs: sin máquinas running ni pausadas, sweep busca VMMs
// huérfanos una vuelta de cada tantas (un minuto con el vigilante de 10 s).
const vueltasEscaneoSinVMs = 6

// errProcesoDesaparecido es el LastErr de una máquina que CORRÍA y cuyo VMM
// murió: su disco tiene lo que escribió mientras vivía (ver retieneDatos).
const errProcesoDesaparecido = "the microVM process disappeared"

// sweep da por failed las máquinas running o pausadas cuyo VMM ya no existe.
// Devuelve el escaneo de VMMs vivos (liveVMs) que hizo, para que el resto de
// la vuelta lo reutilice en vez de recorrer /proc otra vez.
func (m *Manager) sweep() map[string]int {
	var died []*api.Machine
	live := make(map[string]bool)

	// adopt va fuera del candado: en macOS lanza un ps por máquina, y hacerlo
	// con m.mu tomado congelaba ps, run y thaw en cada vuelta del vigilante.
	// Se comprueba sobre una copia y, al volver a tomar el candado, solo se
	// marca la que sigue running con el mismo PID (entre medias pudo pararse,
	// congelarse o relanzarse).
	type vista struct {
		mc  *api.Machine
		pid int
	}
	var vistas []vista
	m.mu.RLock()
	for _, mc := range m.byID {
		if mc.State == api.StateRunning || mc.State == api.StatePaused {
			vistas = append(vistas, vista{mc, mc.PID})
		}
	}
	sospechosos := len(m.orphanSeen) > 0
	m.mu.RUnlock()
	// El escaneo DESPUÉS de copiar: una máquina que pasó a running justo antes
	// de la copia ya tiene su VMM en la tabla. Al revés, se daría por muerta.
	//
	// Sin ninguna running ni pausada el escaneo solo sirve para los huérfanos,
	// y recorrer todos los procesos del host cada 10 s para no encontrar nada
	// es el grueso del trabajo del daemon en reposo: se hace en la primera
	// vuelta sin VMs y después una de cada vueltasEscaneoSinVMs, o en la
	// siguiente si la anterior vio un sospechoso (barrerHuerfanos lo quiere
	// ver dos veces). nil: esta vuelta no hubo escaneo.
	var vmms map[string]int
	if len(vistas) > 0 || sospechosos || m.vueltasSinVMs%vueltasEscaneoSinVMs == 0 {
		vmms = m.liveVMs()
	}
	if len(vistas) > 0 {
		m.vueltasSinVMs = 0
	} else {
		m.vueltasSinVMs++
	}
	var muertas []vista
	for _, v := range vistas {
		if m.vmmSigue(vmms, v.mc.ID, v.pid) {
			live["kl-"+v.mc.ID[:8]] = true
			continue
		}
		muertas = append(muertas, v)
	}

	// Una máquina con su cerrojo de ciclo de vida tomado está en manos de
	// alguien: Freeze mata el VMM (killPaused) y solo después, con el volcado
	// ya sellado, la apunta warm con PID 0. En ese hueco el proceso ya no
	// existe y la máquina sigue "running" con el PID de antes, así que el
	// vigilante la daba por muerta: la marcaba failed, le desmontaba la red
	// que el thaw iba a reutilizar y publicaba un EvFailed falso, y luego
	// Freeze la pisaba con warm. Si está ocupada se deja para la próxima
	// vuelta; quien la tiene sabrá lo que pasó. TryLock y no Lock: el
	// vigilante no debe esperar a un volcado de segundos.
	var soltar []func()
	defer func() {
		for _, s := range soltar {
			s()
		}
	}()
	libres := muertas[:0]
	for _, v := range muertas {
		if s, ok := m.tryLock(v.mc.ID); ok {
			soltar = append(soltar, s)
			libres = append(libres, v)
		}
	}
	muertas = libres

	m.mu.Lock()
	for _, v := range muertas {
		mc := v.mc
		if (mc.State != api.StateRunning && mc.State != api.StatePaused) || mc.PID != v.pid || m.byID[mc.ID] != mc {
			continue
		}
		m.marcarMuertaLocked(mc)
		died = append(died, mc.Clone())
	}
	if len(died) > 0 {
		m.persist()
	}
	m.mu.Unlock()

	// Recoger cgroups que se resistieron a morir en su momento.
	m.sweepCgroups(live)

	// Fuera del mutex: publicar eventos y liberar red puede tardar.
	for _, mc := range died {
		m.trasMorir(mc, m.motivoFinVMM(mc.ID))
	}
	return vmms
}

// marcarMuertaLocked deja failed una máquina running o pausada cuyo VMM ya no
// existe. Con m.mu tomado; quien llama persiste y después llama a trasMorir.
func (m *Manager) marcarMuertaLocked(mc *api.Machine) {
	now := time.Now()
	mc.State = api.StateFailed
	mc.LastErr = errProcesoDesaparecido
	mc.FailedAt = &now
	mc.PID = 0
	delete(m.socket, mc.ID)
}

// trasMorir es lo que queda tras marcarMuertaLocked, fuera de m.mu: soltar su
// red y su CPU, asentar su overlay si iba sin sincronizar (lo que escribió
// sigue en la caché del host, y una failed conserva su disco) y avisar.
// detalle, si lo hay, es cómo terminó el proceso ("signal: killed") y lo que
// dijo el VMM al acabar ("the guest stopped: ..."). Va en el evento y no en
// LastErr, que se queda en errProcesoDesaparecido: retieneDatos lo compara
// tal cual (y un daemon anterior que lea state.json, también).
func (m *Manager) trasMorir(mc *api.Machine, detalle string) {
	m.desmontarRed(knet.Plan(mc.NetIndex, mc.ID), mc.ID)
	m.releaseCPU(mc.ID)
	if err := m.asentarOverlay(mc); err != nil {
		log.Printf("warning: %s: %v", shortID(mc.ID), err)
	}
	msg := errProcesoDesaparecido
	if detalle != "" {
		msg += " (" + detalle + ")"
	}
	m.bus.Publish(api.Event{
		Time: time.Now(), Type: api.EvFailed, ID: mc.ID, Name: mc.Name,
		Message: msg, Owner: mc.Labels[api.LabelOwner],
	})
}

// SALIDA DEL VMM.
//
// El vigilante (sweep) da una vuelta cada 10 s, y en batería cada 30: una
// máquina cuyo VMM moría estando ociosa (un kill -9, un jetsam de macOS)
// seguía "running" en `kling ps` ese rato, y `kling start` contestaba que la
// había arrancado sin hacer nada. Ahora la salida del proceso se ve en el
// acto: con cmd.Wait si es hijo nuestro (esperarVMM), y con kqueue (macOS) o
// pidfd (Linux) si es uno readoptado tras reiniciar el daemon
// (vigilarSalida). El vigilante sigue de respaldo para lo que esto no vea.

// esperarVMM recoge el VMM recién lanzado cmd de la máquina id cuando
// termina, cierra su consola y mira si la máquina se ha quedado sin él.
func (m *Manager) esperarVMM(id string, cmd *exec.Cmd, logf *os.File) {
	_ = cmd.Wait()
	logf.Close()
	detalle := ""
	if cmd.ProcessState != nil {
		detalle = cmd.ProcessState.String()
	}
	m.vmmTermino(id, detalle)
}

// vigilarAdoptado avisa cuando muera el VMM pid de la máquina id, que no es
// hijo de este daemon (lo lanzó uno anterior). Si la plataforma no sabe
// esperar a un proceso ajeno, queda el vigilante.
func (m *Manager) vigilarAdoptado(id string, pid int) {
	// kqueue y pidfd dicen que terminó, no cómo: sin código de salida, el log
	// decía "exited: failed" sin más. Se dice por qué falta, y
	// comprobarVMMLocked añade lo que el VMM dejara en su consola.
	if !vigilarSalida(pid, func() { m.vmmTermino(id, detalleAdoptado) }) {
		log.Printf("reconcile: can't watch the VMM of %s (pid %d) for its exit; the watcher will notice it",
			shortID(id), pid)
	}
}

// detalleAdoptado es el detalle de la salida de un VMM readoptado, del que
// no hay código de salida (no es hijo de este daemon).
const detalleAdoptado = "re-adopted, no exit status"

// vmmTermino: un VMM de la máquina id ha terminado. No se sabe si era el
// suyo de ahora ni si alguien lo mató a propósito, así que se espera al
// cerrojo de ciclo de vida: Stop, Freeze, Remove y compañía matan su VMM con
// él tomado y lo sueltan con el estado ya cambiado (stopped, warm, borrada).
// Con el cerrojo en la mano, si sigue running o pausada y su VMM no está, es
// que murió solo.
//
// Tras Close no hace nada: el aviso llega asíncrono (cmd.Wait, kqueue, pidfd)
// y, al soltar el cerrojo con el Manager cerrado, trasCierre reescribía
// state.json después de que Close dijese que ya no se escribe (en las
// pruebas, encima del RemoveAll de su TempDir). Un VMM que muere con el
// daemon apagándose lo verá reconcile en el siguiente arranque.
func (m *Manager) vmmTermino(id, detalle string) {
	if m.cerrado() {
		return
	}
	// Antes de esperar el cerrojo: si lo tiene la creación, este aviso es de
	// una máquina que aún no se ha entregado (y cuenta para la puerta).
	origen, creando := m.creando.Load(id)
	soltar := m.lifecycle.tomar(id)
	defer soltar()
	// Close pudo llegar mientras se esperaba el cerrojo.
	if m.cerrado() {
		return
	}
	o, _ := origen.(string)
	m.comprobarVMMLocked(id, detalle, creando, o)
}

// cerrado dice si Close ya paró la escritura del estado.
func (m *Manager) cerrado() bool {
	select {
	case <-m.quit:
		return true
	default:
		return false
	}
}

// comprobarVMMLocked marca failed la máquina id si figura running o pausada
// y su VMM ya no existe. Con el cerrojo de ciclo de vida de id tomado (no
// m.mu). Dice si la marcó.
func (m *Manager) comprobarVMMLocked(id, detalle string, creando bool, origen string) bool {
	m.mu.RLock()
	mc := m.byID[id]
	if mc == nil || (mc.State != api.StateRunning && mc.State != api.StatePaused) {
		m.mu.RUnlock()
		return false
	}
	pid := mc.PID
	m.mu.RUnlock()
	// adopt fuera de m.mu: en macOS lee la tabla de procesos. Mira la línea
	// de órdenes, no solo el PID: el que terminó pudo ser un VMM anterior, y
	// un PID reciclado no cuenta como vivo.
	if _, vivo := m.adopt(&api.Machine{ID: id, PID: pid}); vivo {
		return false
	}
	m.mu.Lock()
	mc = m.byID[id]
	if mc == nil || (mc.State != api.StateRunning && mc.State != api.StatePaused) || mc.PID != pid {
		m.mu.Unlock()
		return false
	}
	m.marcarMuertaLocked(mc)
	m.persist()
	copia := mc.Clone()
	m.mu.Unlock()
	// Antes de trasMorir, que borra el cgroup y el jail.
	var diag string
	if detalle == detalleEnjaulado {
		diag = m.diagnosticoVMM(id)
	}
	m.puertaRetroceder("unexpected VMM death", origen, !creando)
	// Lo que dijo el VMM al acabar (fin_vmm.go): con el código de salida
	// solo, un reinicio del invitado era "exit status 0".
	if motivo := m.motivoFinVMM(id); motivo != "" {
		if detalle != "" {
			detalle += "; "
		}
		detalle += motivo
	} else if strings.HasPrefix(detalle, "signal: ") {
		// Muerto de una señal (un SIGTRAP de HVF, un jetsam): sin línea de
		// fin, lo último de su consola suele decir por qué.
		if l := m.ultimaLineaConsola(id); l != "" {
			detalle += "; last console line: " + l
		}
	}
	if diag != "" {
		detalle += "; " + diag
	}
	if detalle != "" {
		log.Printf("watch: the VMM of %s (pid %d) exited (%s): failed", shortID(id), pid, detalle)
	} else {
		log.Printf("watch: the VMM of %s (pid %d) exited: failed", shortID(id), pid)
	}
	m.trasMorir(copia, detalle)
	return true
}

// vmmSigue dice si el VMM pid sigue siendo el de la máquina id, mirando
// primero el escaneo de la vuelta (vmms). Casa en el caso normal, y entonces
// solo falta lo que adopt comprueba además de la línea de órdenes: que el
// socket exista. Si no casa —la máquina murió, o hay dos VMM con su id— se
// pregunta como siempre, con adopt: es lo raro, y ahí manda la exactitud.
func (m *Manager) vmmSigue(vmms map[string]int, id string, pid int) bool {
	if pid > 0 && vmms[id] == pid && (existe(m.dir(id)+"/fc.sock") || existe(m.jailSock(id))) {
		return true
	}
	_, ok := m.adopt(&api.Machine{ID: id, PID: pid})
	return ok
}

// Watch lanza el vigilante en segundo plano.
func (m *Manager) Watch(ctx context.Context, every time.Duration) {
	// Las máquinas readoptadas al arrancar recuperan sus carpetas vivas ya, no
	// a la primera vuelta del vigilante.
	m.lanzarVigilante(ctx, func(context.Context) { m.ensureShares() })
	m.lanzarVigilante(ctx, func(ctx context.Context) { m.watch(ctx, every) })
	// Los dorados con la firma v1 pasan a la v2, una vez por host (firma.go).
	m.lanzarVigilante(ctx, m.MigrarFirmas)
	m.lanzarVigilante(ctx, m.vigilarAlmacen)
	// macOS: pausar las máquinas antes de que el Mac duerma (energia.go).
	m.lanzarVigilante(ctx, m.vigilarEnergia)
}

// lanzarVigilante corre f en su goroutine, con un ctx que Close cancela, y
// contada en vigWG para que Close la espere. Tras Close no lanza nada y
// devuelve false.
//
// Desde dentro de otro vigilante (vigilarPresion) el Add es legal aunque
// Close ya esté esperando: el contador no está a cero, lo sostiene quien
// llama.
func (m *Manager) lanzarVigilante(ctx context.Context, f func(context.Context)) bool {
	m.vigMu.Lock()
	if m.cerrando.Load() {
		m.vigMu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(ctx)
	m.vigCancel = append(m.vigCancel, cancel)
	m.vigWG.Add(1)
	m.vigMu.Unlock()
	go func() {
		defer m.vigWG.Done()
		defer cancel()
		f(ctx)
	}()
	return true
}

// pararVigilantes cancela los vigilantes y espera a que acaben, hasta plazo.
// cerrando ya está puesto: no se lanza ninguno más.
func (m *Manager) pararVigilantes(plazo time.Duration) {
	m.vigMu.Lock()
	cancelar := m.vigCancel
	m.vigCancel = nil
	m.vigMu.Unlock()
	for _, c := range cancelar {
		c()
	}
	hecho := make(chan struct{})
	go func() { m.vigWG.Wait(); close(hecho) }()
	select {
	case <-hecho:
	case <-time.After(plazo):
		log.Printf("shutdown: background watchers still running after %s; what they finish later may not be saved", plazo)
	}
}

// cerrandose dice si Close ya empezó: lo que corre en segundo plano no
// empieza nada nuevo (freezeSi, removeSi).
func (m *Manager) cerrandose() bool { return m.cerrando.Load() || m.cerrado() }

// errCerrando es la respuesta de una operación que llega con Close en curso.
var errCerrando = errors.New("the daemon is shutting down")

// ttlDesde es cuándo empezó a contar el TTL de mc.
//
// TTLAt es el reloj bueno; StartedAt es el respaldo para las máquinas creadas
// antes de que TTLAt existiera, que es exactamente el comportamiento que tenían.
func ttlDesde(mc *api.Machine) time.Time {
	if mc.TTLAt != nil {
		return *mc.TTLAt
	}
	if mc.StartedAt != nil {
		return *mc.StartedAt
	}
	return time.Time{}
}

// expireTTL congela las máquinas cuyo tiempo de vida se agotó (o las destruye,
// si se crearon con OnTTL "remove").
//
// Congelar, no matar: es la diferencia entre serverless y apagar cosas. La
// herramienta deja de costar CPU y RAM, pero vuelve en ~30 ms cuando haga falta.
func (m *Manager) expireTTL(ctx context.Context) {
	// Hasta ttlParalelo a la vez: un freeze que vuelca GiB, o una máquina
	// cuyo cerrojo tiene otro, no retiene a las demás vencidas. Más no: cada
	// freeze es E/S de disco del tamaño de la RAM, y en serie no competían.
	sem := make(chan struct{}, ttlParalelo)
	var wg sync.WaitGroup
	for _, id := range m.ttlVencidas() {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// Un pánico con una máquina no se lleva el TTL de las otras.
			panico.Contener("ttl "+shortID(id), func() { m.aplicarTTL(ctx, id) })
		}()
	}
	wg.Wait()
}

// ttlParalelo es cuántas máquinas atiende expireTTL a la vez.
const ttlParalelo = 2

// expireTTLEnSegundoPlano es expireTTL fuera de la vuelta del vigilante, de
// una en una vuelta: si la anterior aún no acabó, esta no lanza otra (las que
// queden vencidas las verá la siguiente). Antes iba dentro de la vuelta, en
// serie, y un freeze lento de una máquina dejaba sin vigilante a todas.
func (m *Manager) expireTTLEnSegundoPlano(ctx context.Context) {
	if !m.ttlEnCurso.CompareAndSwap(false, true) {
		return
	}
	if !m.lanzarVigilante(ctx, func(ctx context.Context) {
		defer m.ttlEnCurso.Store(false)
		m.expireTTL(ctx)
	}) {
		m.ttlEnCurso.Store(false)
	}
}

// aplicarTTL aplica el TTL vencido de la máquina id.
func (m *Manager) aplicarTTL(ctx context.Context, id string) {
	// Con on_ttl=remove la máquina se destruye en vez de congelarse: es lo
	// que quiere un sandbox. Lo que se ejecutó dentro no tiene por qué seguir
	// existiendo, y congelarlo guardaría en disco una memoria que nadie va a
	// volver a usar.
	//
	// La decisión se tomó con una foto; freezeSi y removeSi vuelven a mirar el
	// plazo con el cerrojo de la máquina tomado, que es también el de Renew.
	// Un renew que llegó entre medias gana.
	vence := func(mc *api.Machine) bool { return ttlVence(mc, time.Now()) }
	if mc, ok := m.Get(id); ok && mc.OnTTL == api.OnTTLRemove {
		if err := m.removeSi(id, vence); err != nil && !errors.Is(err, errYaNoToca) && !errors.Is(err, errCerrando) {
			log.Printf("ttl: couldn't remove %s: %v", shortID(id), err)
		}
		return
	}
	if _, err := m.freezeSi(ctx, id, vence); err != nil {
		if errors.Is(err, errYaNoToca) {
			m.clearFreezeFailures(id)
			return
		}
		if errors.Is(err, errCerrando) {
			return
		}
		m.handleFreezeFailure(id, err)
		return
	}
	m.clearFreezeFailures(id)
}

// ttlVence dice si a mc le toca ya aplicar su TTL: running o pausada con el
// plazo cumplido, o congelada que al vencer se destruye.
func ttlVence(mc *api.Machine, ahora time.Time) bool {
	if mc.TTLSeconds <= 0 {
		return false
	}
	// Una máquina congelada solo vence si al vencer se DESTRUYE: congelar lo
	// ya congelado no tiene sentido, pero un sandbox dormido que nadie
	// reclama sí debe desaparecer, o dormir sería una forma de no morir
	// nunca.
	switch mc.State {
	case api.StateRunning, api.StatePaused:
	case api.StateWarm:
		if mc.OnTTL != api.OnTTLRemove {
			return false
		}
	default:
		return false
	}
	desde := ttlDesde(mc)
	if desde.IsZero() {
		return false
	}
	return ahora.Sub(desde) >= time.Duration(mc.TTLSeconds)*time.Second
}

// ttlVencidas son las máquinas a las que expireTTL tiene que aplicar su TTL
// ahora. Aparte para poder probar la decisión sin un VMM que congelar.
func (m *Manager) ttlVencidas() []string {
	var due []string

	ahora := time.Now()
	m.mu.RLock()
	for _, mc := range m.byID {
		if ttlVence(mc, ahora) {
			due = append(due, mc.ID)
		}
	}
	m.mu.RUnlock()
	return due
}

// maxFreezeFailures es cuántos fallos CONSECUTIVOS de congelación por TTL se
// toleran antes de dar la máquina por perdida. A un tic de ~10 s son unos cinco
// minutos: lo transitorio de verdad —disco lento, un timeout puntual— se
// resuelve mucho antes, y lo que sigue fallando pasado eso es estructural
// aunque no sepamos ponerle nombre. Sin este tope, un fallo no clasificado se
// reintentaba cada tic para siempre — se observaron 260 horas seguidas.
const maxFreezeFailures = 30

// freezeErrIsStructural reconoce los fallos de congelación que NO se arreglan
// reintentando: el socket de control ya no existe (ENOENT al conectar) o hay
// fichero pero nadie escucha (ECONNREFUSED: firecracker no vuelve a abrir su
// API). Un timeout o un error de E/S puntual, en cambio, sí merece otro intento.
func freezeErrIsStructural(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNREFUSED)
}

// handleFreezeFailure decide qué hacer con una máquina cuyo TTL venció pero no
// se pudo congelar: reintentar, o darla por perdida y dejar de insistir.
//
// Antes esto era un log y a otra cosa, y el resultado fue el peor de los casos
// observados: una máquina con el proceso vivo pero el fc.sock desaparecido se
// reintentó cada 10 segundos durante 260 horas. Perdida es perdida: se marca
// failed (estado terminal), se mata el proceso zombi y el segador no vuelve.
func (m *Manager) handleFreezeFailure(id string, err error) {
	cur, ok := m.Get(id)
	if !ok || cur.State != api.StateRunning {
		// Otro camino la congeló, paró o marcó mientras tanto: nada que hacer.
		m.clearFreezeFailures(id)
		return
	}
	// Una máquina con secretos no se congela POR DISEÑO (ver Freeze): el fallo
	// es una negativa de política sobre una máquina sana, no una avería. Matarla
	// por acumular negativas sería perder trabajo del usuario.
	//
	// Pero reintentar tampoco vale: la negativa es permanente mientras el
	// secreto esté dentro, así que el TTL generaba un rechazo cada 10 s para
	// siempre y no se aplicaba nunca. Se apaga el TTL de esa máquina, una vez y
	// diciéndolo: lo que promete `ttl` no se puede cumplir aquí, y fingir que
	// sigue vigente es peor que retirarlo.
	// Una máquina anidada tampoco se congela nunca (anidada.go): misma salida.
	if cur.Nested {
		m.mu.Lock()
		if vivo := m.byID[id]; vivo != nil {
			vivo.TTLSeconds = 0
			m.persist()
		}
		m.mu.Unlock()
		log.Printf("ttl: %s runs with nested virtualization and can't be frozen; dropping its TTL (%v)", shortID(id), err)
		return
	}
	if cur.HasSecrets {
		m.mu.Lock()
		if vivo := m.byID[id]; vivo != nil {
			vivo.TTLSeconds = 0
			m.persist()
		}
		m.mu.Unlock()
		// Sin evento: la máquina no ha fallado ni ha cambiado de estado, y
		// publicar EvFailed sobre una máquina sana mentiría a quien escucha
		// /events. Queda en el log del daemon, que es donde se mira un "¿por qué
		// esta máquina no se congeló?".
		log.Printf("ttl: %s has an injected secret and can't be frozen; dropping its TTL (%v)", shortID(id), err)
		return
	}
	if freezeErrIsStructural(err) || !m.controlSockAlive(cur) {
		m.giveUpOn(cur, fmt.Errorf("unreachable: couldn't freeze it when its TTL expired (%v); "+
			"its control socket is gone, so no retry can succeed", err))
		return
	}
	n := m.noteFreezeFailure(id)
	if n >= maxFreezeFailures {
		m.giveUpOn(cur, fmt.Errorf("gave up freezing it after %d consecutive attempts; last error: %v", n, err))
		return
	}
	log.Printf("ttl: couldn't freeze %s (attempt %d/%d): %v", shortID(id), n, maxFreezeFailures, err)
}

// controlSockAlive dice si el VMM de la máquina sigue siendo alcanzable: su
// proceso es suyo Y su socket de control existe. Es la misma comprobación que
// usa el vigilante (adopt), reutilizada aquí porque el caso observado era
// exactamente el hueco entre ambas: proceso vivo, socket desaparecido.
func (m *Manager) controlSockAlive(mc *api.Machine) bool {
	_, ok := m.adopt(mc)
	return ok
}

// giveUpOn marca una máquina como perdida (failed, terminal) y limpia su
// contador de reintentos. fail() además mata el proceso si sigue vivo: es lo
// que evita que un firecracker sordo retenga su RAM para siempre.
//
// visto es la copia sobre la que se decidió. Con el cerrojo de ciclo de vida
// tomado tiene que seguir siendo la misma: running con el mismo VMM. Sin el
// cerrojo, fail mataba lo que hubiera en ese momento, y si entre medias un
// Stop y un Thaw la habían relanzado, era el VMM nuevo y sano.
func (m *Manager) giveUpOn(visto *api.Machine, err error) {
	id := visto.ID
	m.clearFreezeFailures(id)
	defer m.lock(id)()
	m.mu.RLock()
	mc := m.byID[id]
	igual := mc != nil && mc.State == api.StateRunning && mc.PID == visto.PID
	m.mu.RUnlock()
	if !igual {
		return
	}
	log.Printf("ttl: giving up on %s: %v", shortID(id), err)
	m.fail(mc, err)
}

// noteFreezeFailure apunta un fallo consecutivo más y devuelve cuántos van.
func (m *Manager) noteFreezeFailure(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.freezeFails == nil {
		m.freezeFails = make(map[string]int)
	}
	m.freezeFails[id]++
	return m.freezeFails[id]
}

// clearFreezeFailures borra el contador: los fallos solo cuentan si son seguidos.
func (m *Manager) clearFreezeFailures(id string) {
	m.mu.Lock()
	delete(m.freezeFails, id)
	m.mu.Unlock()
}
