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
		return nil, fmt.Errorf("machine %q doesn't exist", id)
	}
	if !arrancable(live) {
		return nil, fmt.Errorf("only a stopped machine can be started (it is %s)", live.State)
	}
	var vols []resolvedVolume
	if len(live.Volumes) > 0 {
		var err error
		vols, err = comprobarVolumenes(m, api.RunRequest{Volumes: live.Volumes}, m.volumeUsersLocked())
		if err != nil {
			return nil, err
		}
	}
	live.State = api.StateCreated
	m.persist()
	return vols, nil
}

// arrancable: parada, o created sin VMM. Lo segundo es un arranque que no
// terminó porque el daemon murió en medio (el cerrojo, que es de memoria, ya
// no lo tiene nadie): con el cerrojo de ciclo de vida tomado, una created no
// la está arrancando nadie más.
func arrancable(mc *api.Machine) bool {
	return mc.State == api.StateStopped || (mc.State == api.StateCreated && mc.PID == 0)
}

// Start arranca en frío una máquina parada sobre su propio disco. env es el
// entorno (KEY=valor) con el que se arranca: tiene que traer todas las claves
// de su EnvKeys.
func (m *Manager) Start(ctx context.Context, ref string, envKV []string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	defer m.lock(mc.ID)()
	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	if cur.State == api.StateRunning {
		return cur, nil
	}
	if !arrancable(cur) {
		hint := ""
		if cur.State == api.StateWarm || cur.State == api.StatePaused {
			hint = " (kling thaw wakes it with its memory)"
		}
		return nil, fmt.Errorf("only a stopped machine can be started (it is %s)%s", cur.State, hint)
	}
	mc = cur
	// Lo que no necesita nada del host, primero: un entorno incompleto no
	// tiene que esperar admisión ni montar red para decirse.
	env, err := entornoParaArrancar(mc, envKV)
	if err != nil {
		return nil, err
	}
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
	if _, err := os.Stat(m.KernelPath()); err != nil {
		return nil, fmt.Errorf("missing kernel at %s", m.KernelPath())
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

	vols, err := m.reclamarParada(mc.ID)
	if err != nil {
		return nil, err
	}

	// devolver deja la máquina parada otra vez, con el motivo: un arranque
	// que falla no la convierte en failed (la recogería el GC), y se puede
	// reintentar con lo que faltaba.
	var netcfg *knet.Net
	var pid int
	devolver := func(err error) (*api.Machine, error) {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			waitGone(pid, 5*time.Second)
		}
		if netcfg != nil {
			m.desmontarRed(netcfg, mc.ID)
		}
		m.releaseCPU(mc.ID)
		m.mu.Lock()
		if live := m.byID[mc.ID]; live != nil {
			now := time.Now()
			live.State = api.StateStopped
			live.PID = 0
			live.LastErr = err.Error()
			live.StoppedAt = &now
			delete(m.socket, mc.ID)
			m.persist()
		}
		m.mu.Unlock()
		return nil, err
	}

	// Su overlay se revisa antes de montarlo: es ext4 SIN journal
	// (createOverlay) y overlay-init lo monta sin fsck. Un stop de una pausada,
	// o una que murió con el anfitrión, lo deja sucio, y arrancar encima sin
	// mirar puede corromper lo que guarda (una base de datos en su disco). Lo
	// que e2fsck -p no arregla solo no se monta: la máquina sigue parada y se
	// dice. En el almacén de copia al escribir es un enlace, y e2fsck lo sigue.
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

	start := time.Now()
	m.persistirYa()
	pid, err = m.boot(ctx, mc.ID, mc.VCPUs, mc.MemMiB, mc.MemMaxMiB, src, layer, overlay, netcfg,
		append(vols, copies...), mc.AllowExec, m.ipv6DeReceta(mc.Image), env)
	if err != nil {
		return devolver(err)
	}
	if mc.CPUPct <= 0 {
		mc.CPUPct = techoDelDaemon(mc.VCPUs)
	}
	impulso := m.nuevoImpulso(mc.ID, mc.CPUPct, mc.VCPUs, mc.CPUPctFixed)
	defer impulso.fin()
	if warn := m.limitCPU(mc.ID, pid, impulso.tope); warn != "" {
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
		return nil, fmt.Errorf("machine %q was removed while it was being started", mc.Name)
	}
	now := time.Now()
	live.State = api.StateRunning
	live.PID = pid
	live.StartedAt = &now
	live.StoppedAt = nil
	live.FrozenAt = nil
	live.FailedAt = nil
	live.LastErr = ""
	live.Hold = ""
	live.BootMS = time.Since(start).Milliseconds()
	live.TTLAt = &now
	live.CPUPct = mc.CPUPct
	live.IP, live.NetIndex = netcfg.NSIP, netcfg.Index
	live.MemShared = false
	live.DiffBase = ""
	live.Forwards = nil
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
	m.conocerAgente(mc.ID)
	if len(env) > 0 {
		m.retirarEntornoMMDS(mc.ID, mc.Name, env)
	}
	keys := ""
	if len(env) > 0 {
		keys = fmt.Sprintf(" with its environment (%s)", strings.Join(api.MachineEnvKeys(env), ", "))
	}
	m.bus.Publish(api.Event{Time: now, Type: api.EvStarted, ID: mc.ID, Name: mc.Name,
		Message: fmt.Sprintf("started from its disk in %d ms%s", out.BootMS, keys)})
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
