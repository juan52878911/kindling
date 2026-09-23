package machine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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
				log.Printf("reconcile: %s (%s) is still alive (pid %d) even though the state said %q; readopting it",
					mc.Name, mc.ID[:8], pid, mc.State)
				mc.State = api.StateRunning
			}
			mc.PID = pid
			liveCg[cgName(mc.ID)] = true
			// Su netns y reglas sobrevivieron al daemon, pero el resolver dinámico
			// del modo allowlist es una goroutine nuestra y murió con nosotros.
			// Reanudarlo, o su DNS (DNATeado a un puerto sin nadie) se quedaría mudo.
			if mc.Egress == string(knet.EgressAllowlist) {
				if err := knet.Plan(mc.NetIndex, mc.ID).StartAllowlistResolver(mc.AllowDomains); err != nil {
					log.Printf("reconcile: couldn't resume the dns resolver for %s: %v", shortID(mc.ID), err)
				}
			}
			continue
		}

		switch mc.State {
		case api.StateRunning:
			// Su proceso ya no está. Si dejó un snapshot completo está WARM, no
			// parada: decir "stopped" deja el snapshot varado, porque Thaw se
			// niega a descongelar lo que no esté warm y habría que editar el
			// state.json a mano para recuperarlo.
			if m.hasSnapshot(mc.ID) {
				log.Printf("reconcile: %s (%s) is no longer running but keeps its snapshot: warm",
					mc.Name, mc.ID[:8])
				mc.State = api.StateWarm
			} else {
				log.Printf("reconcile: %s (%s) is no longer running, marked as stopped", mc.Name, mc.ID[:8])
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
			log.Printf("reconcile: cleaning up orphan namespace %s", ns)
			knet.TeardownNamespace(ns)
		}
	}

	m.sweepMachineDirs()
	m.killOrphanVMMs()
	fc.BarrerEnlaces(m.root)
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
// de reconciliar con la realidad. Se llama con m.mu tomado.
func (m *Manager) killOrphanVMMs() {
	for id, pid := range m.liveVMs() {
		mc := m.byID[id]
		// Vivo y debería estarlo: no se toca.
		if mc != nil && mc.State == api.StateRunning {
			continue
		}
		// O no está registrado, o su estado dice que no corre: el proceso sobra.
		log.Printf("reconcile: killing orphan VMM of %s (pid %d, state %s)",
			shortID(id), pid, estadoDe(mc))
		_ = syscall.Kill(pid, syscall.SIGKILL)
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
	live := m.liveVMs()

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
		if m.reserved[id] {
			continue
		}
		if mc != nil && (mc.State == api.StateRunning || mc.State == api.StateCreated || mc.State == api.StateWarm) {
			delete(m.orphanSeen, id)
			continue
		}
		m.orphanSeen[id]++
		if m.orphanSeen[id] < 2 {
			continue
		}
		log.Printf("watch: killing orphan VMM of %s (pid %d, state %s)", shortID(id), pid, estadoDe(mc))
		_ = syscall.Kill(pid, syscall.SIGKILL)
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
func (m *Manager) reserveDir(id string) func() {
	m.mu.Lock()
	if m.reserved == nil {
		m.reserved = make(map[string]bool)
	}
	m.reserved[id] = true
	m.mu.Unlock()

	return func() {
		m.mu.Lock()
		delete(m.reserved, id)
		m.mu.Unlock()
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
	dir := filepath.Join(m.root, "machines")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, conocida := m.byID[e.Name()]; conocida {
			continue
		}
		if m.reserved[e.Name()] {
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

// hasSnapshot dice si una máquina tiene un congelado completo en disco.
func (m *Manager) hasSnapshot(id string) bool {
	// No basta con que los ficheros existan: un volcado interrumpido también
	// los deja. Ver volcado.go.
	if err := volcadoValido(m.dir(id)); err != nil {
		if !errors.Is(err, errVolcadoIncompleto) || !strings.Contains(err.Error(), "missing") {
			log.Printf("reconcile: %s: %v", shortID(id), err)
		}
		return false
	}
	return true
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
			// Una vuelta entera por iteracion: si cualquiera de estas tareas
			// entra en panico se pierde ESA vuelta, no el vigilante. Un daemon
			// que sigue vivo pero ha dejado de reconciliar no da ningun sintoma,
			// y eso es peor que caerse.
			panico.Contener("machine.watch", func() {
				m.sweep()
				m.expireTTL(ctx)
				// Recoger los cadáveres: una failed conserva su motivo un tiempo
				// para poder diagnosticarla, y después se recoge sola. Sin esto se
				// acumulaban indefinidamente, una por intento fallido.
				m.gcFailed()
				// Procesos de firecracker que ya no son de nadie. Hasta ahora esto
				// solo corría al arrancar el daemon, así que un VMM huérfano
				// —cada restauración fallida dejaba uno— retenía su RAM hasta el
				// siguiente reinicio, invisible para `kling ps` y para la
				// contabilidad de memoria que decide si cabe la siguiente microVM.
				m.sweepOrphanVMMs()
				// Barrer directorios huérfanos también en marcha: no solo aparecen
				// al arrancar. Bajo el lock, como reconcile.
				m.mu.Lock()
				m.sweepMachineDirs()
				m.sweepSnapshotLeftovers()
				m.mu.Unlock()
				m.vaciarPapelera()
				// Los enlaces cortos a sockets de máquinas que ya no existen
				// (macOS, rutas largas: ver fc.BarrerEnlaces).
				fc.BarrerEnlaces(m.root)
				// Y, si el disco aprieta, recuperar espacio eliminando instancias
				// dormidas que se pueden recrear desde su snapshot.
				m.gcDisk(ctx)
				// El disco se recalcula aquí y no en List(): así `kling ps` no paga
				// un recorrido por máquina, y el dato sigue fresco para las que
				// están escribiendo.
				m.refreshDiskUsage()
			})
		}
	}
}

func (m *Manager) sweep() {
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
		if mc.State == api.StateRunning {
			vistas = append(vistas, vista{mc, mc.PID})
		}
	}
	m.mu.RUnlock()
	var muertas []vista
	for _, v := range vistas {
		c := api.Machine{ID: v.mc.ID, PID: v.pid}
		if _, ok := m.adopt(&c); ok {
			live["kl-"+v.mc.ID[:8]] = true
			continue
		}
		muertas = append(muertas, v)
	}

	m.mu.Lock()
	for _, v := range muertas {
		mc := v.mc
		if mc.State != api.StateRunning || mc.PID != v.pid || m.byID[mc.ID] != mc {
			continue
		}
		now := time.Now()
		mc.State = api.StateFailed
		mc.LastErr = "the microVM process disappeared"
		mc.FailedAt = &now
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
			Message: "the microVM process disappeared",
		})
	}
}

// Watch lanza el vigilante en segundo plano.
func (m *Manager) Watch(ctx context.Context, every time.Duration) {
	go m.watch(ctx, every)
}

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
	var due []string

	m.mu.RLock()
	for _, mc := range m.byID {
		if mc.TTLSeconds <= 0 {
			continue
		}
		// Una máquina congelada solo vence si al vencer se DESTRUYE: congelar lo
		// ya congelado no tiene sentido, pero un sandbox dormido que nadie
		// reclama sí debe desaparecer, o dormir sería una forma de no morir
		// nunca.
		switch mc.State {
		case api.StateRunning:
		case api.StateWarm:
			if mc.OnTTL != api.OnTTLRemove {
				continue
			}
		default:
			continue
		}
		desde := ttlDesde(mc)
		if desde.IsZero() {
			continue
		}
		if time.Since(desde) >= time.Duration(mc.TTLSeconds)*time.Second {
			due = append(due, mc.ID)
		}
	}
	m.mu.RUnlock()

	for _, id := range due {
		// Con on_ttl=remove la máquina se destruye en vez de congelarse: es lo
		// que quiere un sandbox. Lo que se ejecutó dentro no tiene por qué
		// seguir existiendo, y congelarlo guardaría en disco una memoria que
		// nadie va a volver a usar.
		if mc, ok := m.Get(id); ok && mc.OnTTL == api.OnTTLRemove {
			if err := m.Remove(id); err != nil {
				log.Printf("ttl: couldn't remove %s: %v", shortID(id), err)
			}
			continue
		}
		if _, err := m.Freeze(ctx, id); err != nil {
			m.handleFreezeFailure(id, err)
			continue
		}
		m.clearFreezeFailures(id)
	}
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
		m.giveUpOn(id, fmt.Errorf("unreachable: couldn't freeze it when its TTL expired (%v); "+
			"its control socket is gone, so no retry can succeed", err))
		return
	}
	n := m.noteFreezeFailure(id)
	if n >= maxFreezeFailures {
		m.giveUpOn(id, fmt.Errorf("gave up freezing it after %d consecutive attempts; last error: %v", n, err))
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
func (m *Manager) giveUpOn(id string, err error) {
	m.clearFreezeFailures(id)
	m.mu.RLock()
	mc := m.byID[id]
	m.mu.RUnlock()
	if mc == nil {
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
