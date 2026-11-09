package machine

// Start: arrancar otra vez una máquina parada (`kling start`, el `docker
// start` de kindling).
//
// Parar mata el VMM y conserva el directorio: el overlay —el disco propio de
// la máquina, con todo lo que escribió— y su configuración. Hasta ahora stopped
// era un estado terminal y lo único que se podía hacer con ella era `rm`.
// Start la arranca EN FRÍO sobre ese mismo overlay, con la misma imagen,
// memoria, vCPUs, volúmenes, salida de red y carpetas: lo que tenía en
// memoria se perdió al pararla (para conservarlo está freeze/thaw).
//
// El entorno de -e es la excepción. El daemon no guarda los valores (solo los
// nombres, EnvKeys; ver entorno.go), así que quien arranca tiene que volver a
// darlos: Start exige todas las claves de EnvKeys y dice cuáles faltan, en vez
// de arrancar el servicio sin su contraseña.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// entornoParaArrancar valida el entorno de un Start contra las claves con las
// que la máquina se arrancó la primera vez (EnvKeys). Los errores nombran
// claves, nunca valores.
func entornoParaArrancar(mc *api.Machine, kv []string) (map[string]string, error) {
	env, err := api.MachineEnvMap(kv)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEnvRequest, err)
	}
	var faltan []string
	for _, k := range mc.EnvKeys {
		if _, ok := env[k]; !ok {
			faltan = append(faltan, k)
		}
	}
	if len(faltan) > 0 {
		return nil, fmt.Errorf("%w: machine %q was started with an environment, and kindling keeps only its names: "+
			"pass %s again with -e or -env-file", ErrEnvRequest, mc.Name, strings.Join(faltan, ", "))
	}
	return env, nil
}

// reclamarParada pasa la máquina id de stopped a created y le resuelve los
// volúmenes, en la MISMA sección crítica: una parada no cuenta como usuaria
// de sus volúmenes (volumeUsersLocked), así que mirar y marcar por separado
// dejaba que dos arranques —este y un run con el mismo volumen en escritura—
// pasaran los dos. En created ya cuenta, y el vigilante no toma su VMM por
// huérfano (sweepOrphanVMMs).
func (m *Manager) reclamarParada(id string) ([]resolvedVolume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	live := m.byID[id]
	if live == nil {
		return nil, noExiste(id)
	}
	if !arrancable(live) {
		return nil, estadoInvalido("only a stopped or failed machine can be started (it is %s)", live.State)
	}
	// Vuelve a contar como máquina y como memoria de su dueño (su disco ya
	// contaba): se decide aquí, en la misma sección crítica en que deja de
	// estar parada, por lo mismo que en publicar.
	if err := m.cuotaExcedidaLocked(live.Labels[api.LabelOwner], UsoCuota{
		Maquinas: 1, MemMiB: memCuota(live.MemMiB, live.MemMaxMiB)}, live.ID); err != nil {
		return nil, err
	}
	var vols []resolvedVolume
	if len(live.Volumes) > 0 {
		var err error
		vols, err = comprobarVolumenes(m, api.RunRequest{Volumes: live.Volumes}, m.volumeUsersLocked())
		if err != nil {
			return nil, err
		}
		if err := m.adjudicarVolumenesLocked(vols); err != nil {
			return nil, err
		}
	}
	live.State = api.StateCreated
	live.FailedAt = nil
	// VolumeWait es del primer arranque: un Start monta como siempre (nadie
	// le mandaría el acquire).
	live.VolumeWait = false
	// Lo listo era del arranque anterior: hasta que el agente nuevo conteste,
	// no se sabe (como en Run). Si no, GET /machines decía "ready" mientras
	// el invitado aún arrancaba, y un exec en ese rato fallaba.
	live.Ready = api.ReadyUnknown
	// Ni una tanda de ganchos pendiente de una restauración anterior (una que
	// murió sola, sin pasar por Stop): este arranque es en frío, y lanzarla
	// mandaría un /hooks de copia a un invitado recién arrancado.
	m.ganchosPendientes.Delete(id)
	live.HooksPending = ""
	m.persist()
	return vols, nil
}

// arrancable: parada, created sin VMM o failed. Lo segundo es un arranque
// que no terminó porque el daemon murió en medio (el cerrojo, que es de
// memoria, ya no lo tiene nadie): con el cerrojo de ciclo de vida tomado, una
// created no la está arrancando nadie más. Lo tercero, una cuyo VMM murió (o
// cuyo arranque o descongelado falló): mientras conserve su disco se arranca
// en frío sobre él como una parada; start comprueba que lo tenga.
func arrancable(mc *api.Machine) bool {
	return mc.State == api.StateStopped || (mc.State == api.StateCreated && mc.PID == 0) ||
		(mc.State == api.StateFailed && mc.LastErr != errOverlaySinAsentar)
}

// Start arranca en frío una máquina parada sobre su propio disco. env es el
// entorno (KEY=valor) con el que se arranca: tiene que traer todas las claves
// de su EnvKeys.
//
// Cuenta en la telemetría como "start" (/metrics): pasa por la misma admisión
// que Run, y sus rechazos y fallos tienen que verse igual.
func (m *Manager) Start(ctx context.Context, ref string, envKV []string) (*api.Machine, error) {
	mc, err := m.start(ctx, ref, envKV)
	tel.fin(OpStart, err)
	return mc, err
}

func (m *Manager) start(ctx context.Context, ref string, envKV []string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, noExiste(ref)
	}
	defer m.lock(mc.ID)()
	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, noExiste(ref)
	}
	// Running de verdad, con su VMM vivo: no hay nada que arrancar. Si el VMM
	// murió y nadie lo ha visto aún (ver vmmTermino), se apunta failed aquí y
	// se arranca en frío como cualquier failed: contestar con la máquina tal
	// cual era decir que había arrancado sin hacer nada.
	if cur.State == api.StateRunning {
		if !m.comprobarVMMLocked(cur.ID, "") {
			return cur, nil
		}
		if cur, ok = m.Get(mc.ID); !ok {
			return nil, noExiste(ref)
		}
	}
	if !arrancable(cur) {
		hint := ""
		switch {
		case cur.State == api.StateWarm || cur.State == api.StatePaused:
			hint = " (kling thaw wakes it with its memory)"
		case cur.State == api.StateFailed:
			hint = ": " + cur.LastErr
		}
		return nil, estadoInvalido("only a stopped or failed machine can be started (it is %s)%s", cur.State, hint)
	}
	mc = cur
	// Lo que no necesita nada del host, primero: un entorno incompleto no
	// tiene que esperar admisión ni montar red para decirse.
	env, err := entornoParaArrancar(mc, envKV)
	if err != nil {
		return nil, err
	}
	// El servicio de una imagen lee su entorno UNA vez, al arrancar (ver
	// pkg/guest/machine_env.go), y los marcadores de las credenciales se
	// reentregan después, por otra clave de MMDS que solo usa el puente MCP:
	// sin esto, un servicio con `machine credential` nunca veía su variable.
	creds, err := m.cargarCredenciales(mc.ID)
	if err != nil {
		return nil, fmt.Errorf("machine %q can't be started: reading its credentials: %w", mc.Name, err)
	}
	bootEnv := entornoConMarcadores(env, creds)
	if m.JailerBlocked != "" {
		return nil, errors.New(m.JailerBlocked)
	}
	dir := m.dir(mc.ID)
	overlay := filepath.Join(dir, "overlay.ext4")
	if _, err := os.Stat(overlay); err != nil {
		return nil, fmt.Errorf("machine %q can't be started: its disk is gone (%v). Remove it (kling rm %s)",
			mc.Name, err, mc.Name)
	}
	// Su disco vive en el almacén y no queda sitio: como en Thaw, se dice
	// ahora y no con un EIO dentro del invitado.
	if err := m.comprobarAlmacenPara(mc.ID); err != nil {
		return nil, fmt.Errorf("machine %q can't be started: %w", mc.Name, err)
	}
	// Desde que se mira la imagen hasta que la máquina deja de estar parada
	// (reclamarParada; desde ahí ImageReplaceable ya la cuenta), nadie la
	// sustituye por debajo: con una capa, la base y la capa se resuelven por
	// separado y se podía arrancar una base vieja bajo una capa nueva.
	defer m.reservarImagen(mc.Image)()
	// La imagen pudo borrarse mientras estaba parada (una parada no la
	// retiene, ver blobs.go): se dice antes de reservar nada, y con cómo
	// salir. El disco sigue ahí; basta con volver a traer la imagen con el
	// mismo nombre.
	if m.imagenBorrada(mc.Image) {
		return nil, fmt.Errorf("machine %q can't be started: its image %q is gone (a stopped machine doesn't keep it). "+
			"Its disk is kept: import or build the image again under that name (kling image import -name %s <ref>) and start it again",
			mc.Name, mc.Image, mc.Image)
	}
	src, layer, err := m.imageLayer(mc.Image)
	if err != nil {
		return nil, fmt.Errorf("machine %q can't be started: %w", mc.Name, err)
	}
	if _, err := os.Stat(m.kernelDe(mc.Nested)); err != nil {
		return nil, fmt.Errorf("missing kernel at %s", m.kernelDe(mc.Nested))
	}
	// Sus copias de carpetas (-share SRC:DST): los ext4 siguen en su
	// directorio, detrás de los volúmenes como en Run.
	var copies []resolvedVolume
	for i, s := range mc.Shares {
		if s.Live() {
			continue
		}
		p := shareImagePath(dir, i)
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("machine %q can't be started: the copy for %s is gone (%v)", mc.Name, s.Mount, err)
		}
		copies = append(copies, resolvedVolume{path: p, mount: s.Mount, readOnly: true})
	}
	// La cuota de su dueño, antes de lo caro: admitir y hacer sitio pueden
	// apretar los globos de otros inquilinos y esperar hasta 60 s para
	// acabar en un 429. Es el filtro previo, como en Run; quien decide sigue
	// siendo reclamarParada. Su propio disco ya contaba y no pide más.
	if err := m.comprobarCuotaSin(mc.Labels[api.LabelOwner], UsoCuota{
		Maquinas: 1, MemMiB: memCuota(mc.MemMiB, mc.MemMaxMiB)}, mc.ID); err != nil {
		return nil, err
	}
	// Admisión, como Run: un arranque en frío ocupa lo mismo aquí. Con el
	// cerrojo de la máquina tomado, como en Thaw (ver allí): un stop o un rm
	// de esta máquina esperan, acotado, a que se decida su memoria.
	if err := m.admitir(); err != nil {
		return nil, err
	}
	releaseMem, err := m.reserveMemoryMakingRoom(ctx, mc.MemMiB, "", mc.ID)
	if err != nil {
		return nil, err
	}
	defer releaseMem()

	// Una failed no debería tener VMM; si aún le queda uno (un fallo que no
	// lo mató), es un huérfano sobre su mismo overlay, y arrancar otro encima
	// sería corromperlo. Muere antes: desde reclamarParada figura created, y
	// el barrido de huérfanos ya no lo tocaría.
	eraFallida := mc.State == api.StateFailed
	// Lo que tenía de failed, por si el arranque falla antes de tocar su
	// volcado (ver devolver).
	errFallida, fallidaEn := mc.LastErr, mc.FailedAt
	if eraFallida {
		if err := m.matarVMMDeFallida(mc); err != nil {
			return nil, fmt.Errorf("machine %q can't be started: %w", mc.Name, err)
		}
	}
	vols, err := m.reclamarParada(mc.ID)
	if err != nil {
		return nil, err
	}

	// devolver deja la máquina parada otra vez, con el motivo: un arranque
	// que falla no la convierte en failed (la recogería el GC), y se puede
	// reintentar con lo que faltaba.
	//
	// Salvo una failed que aún conserva su volcado entero (volcadoBorrado
	// dice si ya se retiró, justo antes de lanzar el VMM): parada, su volcado
	// seguía en disco pero ya no lo cargaba nadie, porque Thaw solo
	// descongela warm o failed. Vuelve a failed, con su error y su fecha de
	// antes, y un thaw la sigue pudiendo restaurar (thawFallida comprueba que
	// el disco no se tocó después del volcado; un e2fsck que lo arregló
	// cuenta como tocarlo).
	var netcfg *knet.Net
	var pid int
	volcadoBorrado := false
	devolver := func(err error) (*api.Machine, error) {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			waitGone(pid, 5*time.Second)
		}
		if netcfg != nil {
			m.desmontarRed(netcfg, mc.ID)
		}
		m.releaseCPU(mc.ID)
		conVolcado := eraFallida && !volcadoBorrado && volcadoValido(dir, nil) == nil
		m.mu.Lock()
		if live := m.byID[mc.ID]; live != nil {
			now := time.Now()
			if conVolcado {
				live.State = api.StateFailed
				live.LastErr = errFallida
				live.FailedAt = fallidaEn
				if live.FailedAt == nil {
					live.FailedAt = &now
				}
			} else {
				live.State = api.StateStopped
				live.LastErr = err.Error()
				live.StoppedAt = &now
			}
			live.PID = 0
			delete(m.socket, mc.ID)
			m.persist()
			m.ajustarDuenosLocked(nombresVolumenes(live.Volumes))
		}
		m.mu.Unlock()
		if conVolcado {
			log.Printf("start: %s: failed before booting (%v); it stays failed with its memory dump", mc.Name, err)
			return nil, fmt.Errorf("%w (it stays failed and keeps its memory dump: kling thaw %s restores it)", err, mc.Name)
		}
		return nil, err
	}

	// Su overlay se revisa antes de montarlo: es ext4 SIN journal
	// (createOverlay) y overlay-init lo monta sin fsck. Un stop de una pausada,
	// o una que murió con el anfitrión, lo deja sucio, y arrancar encima sin
	// mirar puede corromper lo que guarda (una base de datos en su disco). Lo
	// que e2fsck -p no arregla solo no se monta: la máquina sigue parada y se
	// dice. En el almacén de copia al escribir es un enlace, y el daemon lo
	// abre siguiéndolo para pasárselo a e2fsck, que corre confinado (A1).
	reparado, salida, err := revisarExt4(ctx, overlay)
	if err != nil {
		return devolver(fmt.Errorf("machine %q can't be started: checking its disk: %w", mc.Name, err))
	}
	if reparado {
		log.Printf("start: %s: disk repaired before booting: %s", mc.Name, strings.TrimSpace(string(salida)))
	}
	for _, v := range vols {
		if !v.readOnly {
			repairVolume(ctx, v.path)
		}
	}

	egress, err := knet.ParseEgress(mc.Egress)
	if err != nil {
		return devolver(err)
	}
	// Su índice de red si sigue libre en el host (conserva la IP), si no
	// uno nuevo, como al descongelar sin la red montada.
	n, err := m.redParaRehacer(mc)
	if err != nil {
		return devolver(fmt.Errorf("setting up the network: %w", err))
	}
	if err := m.montarRed(n, mc.ID, egress, mc.AllowDomains); err != nil {
		return devolver(fmt.Errorf("setting up the network: %w", err))
	}
	netcfg = n
	// Las aristas del nodo, si es de un grafo (como en Thaw).
	if spec, ok := m.especRedGrafo(mc); ok {
		if err := montarRedGrafo(netcfg, spec); err != nil {
			log.Printf("start: %s started without its graph edges: %v", mc.Name, err)
		}
	}
	// El VMM solo puede escribir en lo suyo. Un overlay en el almacén de
	// copia al escribir es un enlace, y Own (chown, que sigue enlaces) se lo
	// daría al VMM: ahí lo usa por grupo (ver runFrom).
	ceder := []string{dir}
	if fi, err := os.Lstat(overlay); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		ceder = append(ceder, overlay)
	}
	if err := m.priv.Own(ceder...); err != nil {
		return devolver(err)
	}

	// El volcado que pueda conservar una failed deja de valer en cuanto el
	// invitado arranque y escriba en su disco: cargarlo después (un thaw, o
	// reconcile dándola por warm) pondría aquella memoria sobre otro disco.
	if eraFallida {
		m.borrarVolcado(mc.ID)
		volcadoBorrado = true
	}
	start := time.Now()
	// Sin su máquina en disco no se lanza: un VMM sin registro es, al volver
	// el daemon, un huérfano que reconcile mata (ver persistirYa).
	if err := m.persistirYa(); err != nil {
		return devolver(err)
	}
	pid, err = m.boot(ctx, mc.ID, mc.VCPUs, mc.MemMiB, mc.MemMaxMiB, src, layer, overlay, netcfg,
		append(vols, copies...), mc.AllowExec, m.ipv6DeReceta(mc.Image), bootEnv, shareWaitDe(mc.ShareWait, mc.Shares))
	if err != nil {
		return devolver(err)
	}
	if mc.CPUPct <= 0 {
		mc.CPUPct = techoDelDaemon(mc.VCPUs)
	}
	impulso := m.nuevoImpulso(mc.ID, mc.CPUPct, mc.VCPUs, mc.CPUPctFixed)
	defer impulso.fin()
	if warn := m.limitCPU(mc.ID, pid, impulso.tope, memoriaCgroup(mc)); warn != "" {
		log.Printf("warning: %s: %s", mc.Name, warn)
	}
	// Sus credenciales: el proxy y el resolver se fueron con la red al
	// pararla, y el VMM es nuevo. Un fallo no tumba el arranque: el dominio
	// con credencial falla cerrado, y queda dicho.
	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if _, err := m.reentregarCredenciales(ctx, mc, fc.New(sock)); err != nil {
		log.Printf("start: %s started without its credentials: %v", mc.Name, err)
	}

	m.mu.Lock()
	live := m.byID[mc.ID]
	if live == nil {
		// Con el cerrojo tomado no debería pasar; si pasa, el VMM no es de
		// nadie y se mata aquí (ver el mismo caso en Thaw).
		delete(m.socket, mc.ID)
		m.mu.Unlock()
		_ = syscall.Kill(pid, syscall.SIGKILL)
		m.desmontarRed(netcfg, mc.ID)
		return nil, retiradaDurante(mc.Name, "started")
	}
	now := time.Now()
	live.State = api.StateRunning
	live.PID = pid
	live.StartedAt = &now
	live.VMM = m.origenActual().vmm
	live.StoppedAt = nil
	live.FrozenAt = nil
	live.FailedAt = nil
	live.LastErr = ""
	live.Hold = ""
	// Nunca 0: BootMS > 0 es lo que dice que arrancó en frío y ya no mapea
	// el mem.file de su dorado aunque conserve From (ver mapeaSuDorado).
	live.BootMS = max(time.Since(start).Milliseconds(), 1)
	// Lo que midió la última transición de antes (un thaw, su desglose) ya no
	// es de esta: `kling ps` seguía enseñando "thaw N ms" bajo LAST OP.
	live.ThawMS, live.Wake = 0, nil
	live.TTLAt = &now
	live.CPUPct = mc.CPUPct
	live.IP, live.NetIndex = netcfg.NSIP, netcfg.Index
	live.MemShared = false
	live.DiffBase = ""
	// Forwards NO se toca: Stop ya los borró, y boot acaba de abrir los del
	// VMM nuevo (abrirReenvios, en macOS). Borrarlos aquí dejaba la máquina
	// con IP:puerto, que el Mac no alcanza: ni listo, ni exec, ni servicios.
	//
	// Los secretos de MMDS eran del VMM anterior: este arrancó con la RAM y
	// el almacén vacíos. Con la marca puesta, Freeze y Commit la rechazaban
	// para siempre y el vigilante le quitaba el TTL.
	live.HasSecrets = false
	delete(m.secretos, mc.ID)
	live.CredentialAnyDatabase = mc.CredentialAnyDatabase
	// Las claves que se dieron ahora (al menos las de antes; puede traer más).
	live.EnvKeys = api.MachineEnvKeys(env)
	m.persist()
	out := *live
	m.mu.Unlock()

	out.DiskBytes = m.touchDisk(mc.ID)
	// Las carpetas vivas se reconectan como tras un thaw: en segundo plano.
	m.startShares(mc.ID)
	impulso.entregar()
	m.vigilarListo(mc.ID, nil)
	m.olvidarAgente(mc.ID)
	m.conocerAgenteY(mc.ID, func(ag *api.GuestAgent) { m.resyncArranque(mc.ID, ag) })
	if len(bootEnv) > 0 || comandoDe(mc) != nil {
		m.retirarEntornoMMDS(mc.ID, mc.Name, bootEnv)
	}
	keys := ""
	if len(env) > 0 {
		keys = fmt.Sprintf(" with its environment (%s)", strings.Join(api.MachineEnvKeys(env), ", "))
	}
	m.bus.Publish(api.Event{Time: now, Type: api.EvStarted, ID: mc.ID, Name: mc.Name,
		Message: fmt.Sprintf("started from its disk in %d ms%s", out.BootMS, keys)})
	tel.exito(OpStart, DurBoot, out.BootMS)
	return &out, nil
}

// imagenBorrada dice si de la imagen no queda ni la monolítica ni la capa.
// Un nombre inválido no se mira aquí: imageLayer lo rechaza con su error.
func (m *Manager) imagenBorrada(image string) bool {
	if !validName.MatchString(image) {
		return false
	}
	for _, p := range []string{m.imagePath(image), m.layerPath(image)} {
		if _, err := os.Stat(p); err == nil {
			return false
		}
	}
	return true
}

// matarVMMDeFallida mata el VMM que pueda quedarle a una failed y espera a
// que desaparezca. Con el cerrojo de ciclo de vida tomado.
func (m *Manager) matarVMMDeFallida(mc *api.Machine) error {
	pid := m.liveVMs()[mc.ID]
	if pid <= 0 {
		return nil
	}
	log.Printf("%s (%s) is failed but its VMM is still alive (pid %d): killing it first", mc.Name, shortID(mc.ID), pid)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if !waitGone(pid, 5*time.Second) {
		return fmt.Errorf("its old VMM (pid %d) did not die", pid)
	}
	return nil
}

// thawFallida es lo que Thaw pide a una failed antes de cargar su volcado:
// uno completo (volcadoValido), que el VMM que lo volcó ya no esté vivo y que
// su disco no haya cambiado desde el volcado. Una que murió durante el resync
// de un thaw conserva el volcado del que se despertó, y es su RAM entera;
// pero si después de él el invitado llegó a escribir en el overlay, cargar
// aquella memoria sobre este disco es corromperlo (la caché del invitado ya
// no casa con lo que hay debajo), y entonces solo cabe arrancarla en frío.
func (m *Manager) thawFallida(mc *api.Machine) error {
	dir := m.dir(mc.ID)
	if err := volcadoValido(dir, nil); err != nil {
		return fmt.Errorf("machine %q is failed (%s) and has no usable memory dump (%v): "+
			"kling start boots it from its disk", mc.Name, mc.LastErr, err)
	}
	if err := diskSinTocarDesdeVolcado(dir); err != nil {
		return fmt.Errorf("machine %q is failed (%s) and %v: thawing it would corrupt it; "+
			"kling start boots it from its disk", mc.Name, mc.LastErr, err)
	}
	return m.matarVMMDeFallida(mc)
}

// diskSinTocarDesdeVolcado falla si el overlay de dir se modificó después de
// sellar el volcado (marcaOK). Sin sello (volcados anteriores a los sellos)
// se compara con mem.file.
func diskSinTocarDesdeVolcado(dir string) error {
	ref, err := os.Stat(filepath.Join(dir, marcaOK))
	if err != nil {
		if ref, err = os.Stat(filepath.Join(dir, "mem.file")); err != nil {
			return err
		}
	}
	disco, err := os.Stat(filepath.Join(dir, "overlay.ext4"))
	if err != nil {
		return fmt.Errorf("its disk is gone (%v)", err)
	}
	if disco.ModTime().After(ref.ModTime()) {
		return fmt.Errorf("its disk was written after its memory dump (%s, dump %s)",
			disco.ModTime().Format(time.RFC3339), ref.ModTime().Format(time.RFC3339))
	}
	return nil
}
