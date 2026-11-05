package machine

// Snapshots por imagen: `kling run -image X` restaura en vez de arrancar en
// frío (docs/snapshot-imagen.md).
//
// La primera vez que se arranca X con unos parámetros, el daemon arranca
// APARTE, en segundo plano, una máquina de preparación: la misma imagen,
// kernel, vCPU, memoria y disco, pero sin nada de ninguna máquina —ni
// entorno, ni comando, ni volúmenes, ni carpetas, ni red hacia fuera— y con
// kling.imgsnap=1, que le dice a su agente que no lance el servicio de la
// imagen y espere (pkg/guest arranque_tardio.go). En cuanto el agente lo
// anuncia (api.GuestCapLateStart) y no hay ningún otro proceso vivo, se
// congela como un dorado más, con una marca (marcaImagen) que lo aparta de
// `kling snapshots`, y la máquina de preparación se borra.
//
// Las siguientes `run` con los mismos parámetros lo restauran como cualquier
// copia de un dorado —disco CoW propio, red propia, resync de reloj, CSPRNG y
// machine-id— y después hacen lo que hace un arranque en frío: POST
// /machine/start con el entorno y el comando de ESTA máquina, y el agente
// lanza el servicio. Para todo lo demás (kling ps, el GC, kling start, -e) la
// máquina es una arrancada en frío: From vacío, Machine.ImageSnapshot dice de
// dónde salió.
//
// Lo que garantiza que nada de una máquina pase a otra es que el volcado
// sale de una máquina que no es de nadie y que no ha hecho nada: lo propio
// de cada máquina llega siempre DESPUÉS de restaurar (TestSnapshotImagen*).
//
// La clave (claveImagen) es todo lo que cambia el estado congelado: la imagen
// (y la identidad de sus ficheros: otra importación con el mismo nombre es
// otra clave), el kernel, el VMM y la versión de kling, la arquitectura, las
// vCPU, la memoria y su techo, el disco, el caudal de red, la ejecución y la
// pila IPv6 de la receta. Cambiar cualquiera da otro nombre; el viejo deja de
// usarse y el barrido lo borra en cuanto nadie lo usa.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// prefijoImagen es el del nombre de los snapshots por imagen. Lo que los
	// reconoce es la marca, no el nombre (como los de fork): un `kling save`
	// con ese nombre es un dorado normal.
	prefijoImagen = "kling-img-"
	// prefijoPrepImagen, el de las máquinas de preparación.
	prefijoPrepImagen = "kling-imgprep-"
	// marcaImagen es el fichero que marca un snapshot como snapshot por
	// imagen. Lleva su clave (JSON) y su mtime es su último uso (LRU).
	marcaImagen = "image-of"
	// versionClaveImagen sube cuando cambia lo que el agente o el daemon
	// hacen al preparar o al entregar (el formato del volcado, GuestStart):
	// los de antes dejan de valer solos.
	versionClaveImagen = 1
	// plazoPrepImagen acota la preparación entera.
	plazoPrepImagen = 2 * time.Minute
	// reintentoPrepImagen: tras un fallo que puede ser del momento (memoria,
	// disco, un host cargado), cuándo se vuelve a intentar con esa clave.
	reintentoPrepImagen = 10 * time.Minute
)

// claveImagen es lo que distingue un snapshot por imagen de otro.
type claveImagen struct {
	V         int           `json:"v"`
	Image     string        `json:"image"`
	Base      huellaFichero `json:"base"`
	Layer     huellaFichero `json:"layer,omitzero"`
	Kernel    string        `json:"kernel_sha256"`
	VMM       string        `json:"vmm"`
	Backend   string        `json:"backend"`
	Kling     string        `json:"kling"`
	MacOS     string        `json:"macos,omitempty"`
	Arch      string        `json:"arch"`
	VCPUs     int           `json:"vcpus"`
	MemMiB    int           `json:"mem_mib"`
	MemMaxMiB int           `json:"mem_max_mib,omitempty"`
	DiskMiB   int           `json:"disk_mib"`
	NetMiBps  int64         `json:"net_bps"`
	AllowExec bool          `json:"allow_exec,omitempty"`
	IPv6      bool          `json:"ipv6,omitempty"`
}

// huellaFichero identifica el contenido de un fichero de imagen sin leerlo:
// una importación nueva es otro fichero (renombrado encima: otro inodo) o, si
// se escribe en el sitio, otra fecha y quizá otro tamaño.
type huellaFichero struct {
	Ino uint64 `json:"ino"`
	Tam int64  `json:"size"`
	Mod int64  `json:"mtime_ns"`
}

func huellaDeFichero(ruta string) (huellaFichero, error) {
	fi, err := os.Stat(ruta)
	if err != nil {
		return huellaFichero{}, err
	}
	h := huellaFichero{Tam: fi.Size(), Mod: fi.ModTime().UnixNano()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		h.Ino = uint64(st.Ino)
	}
	return h, nil
}

// nombre es el del snapshot de esta clave.
func (c claveImagen) nombre() string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return prefijoImagen + hex.EncodeToString(h[:8])
}

// snapshotsImagenActivos dice si el daemon usa snapshots por imagen, y si no,
// por qué. Solo donde restaurar es barato: con kling-vz restaurar carga la
// RAM entera (más lento que arrancar una imagen pequeña); con kling-hv y
// Firecracker se mapea el volcado y se comparte.
func snapshotsImagenActivos() (bool, string) {
	if v := os.Getenv("KLING_IMAGE_SNAPSHOTS"); v == "0" || strings.EqualFold(v, "off") {
		return false, "KLING_IMAGE_SNAPSHOTS=0"
	}
	if !restauraBarato() {
		return false, "the " + backendVMM + " backend loads a snapshot's whole memory on restore: not cheaper than a cold boot"
	}
	return true, ""
}

// restauraBarato es restaurarComparteMemoria (una constante en Linux); las
// pruebas la cambian.
var restauraBarato = func() bool { return restaurarComparteMemoria }

// topeSnapshotsImagen es lo que pueden ocupar entre todos: KLING_IMAGE_SNAPSHOT_MIB,
// o 1 GiB en macOS (poco disco) y 4 GiB en Linux.
func topeSnapshotsImagen() int64 {
	if v, err := strconv.Atoi(os.Getenv("KLING_IMAGE_SNAPSHOT_MIB")); err == nil && v >= 0 {
		return int64(v) << 20
	}
	if runtime.GOOS == "darwin" {
		return 1 << 30
	}
	return 4 << 30
}

// asentarPrepImagen es cuánto se deja asentarse al agente de la máquina de
// preparación antes de congelarla: justo al escuchar, el runtime de Go aún
// está tocando páginas, y cada copia las acaba escribiendo (más memoria
// propia por copia: medido 16,8 frente a ~4 MiB en kling-hv).
func asentarPrepImagen() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("KLING_IMAGE_SNAPSHOT_SETTLE")); err == nil && d >= 0 {
		return d
	}
	return 2 * time.Second
}

// estadoImagen es el estado en memoria de los snapshots por imagen.
type estadoImagen struct {
	mu sync.Mutex
	// preparando: claves con una preparación en marcha.
	preparando map[string]bool
	// no: claves que no se intentan hasta hasta (con el motivo, para el log
	// y para `kling image snapshots`).
	no map[string]noImagen
	// uno: una preparación a la vez en todo el daemon; las demás esperan a
	// la próxima `run` (arrancan en frío mientras tanto).
	uno chan struct{}
	// prep: nombres de las máquinas de preparación en marcha (boot les pone
	// kling.imgsnap=1). Por nombre y no por etiqueta: una etiqueta la pone
	// cualquiera.
	prep map[string]bool
	// marcas: la marca que commit escribe antes del meta.json de cada
	// snapshot por imagen en curso.
	marcas map[string][]byte
}

type noImagen struct {
	motivo string
	hasta  time.Time
}

func (e *estadoImagen) iniciar() {
	if e.preparando == nil {
		e.preparando, e.no, e.prep, e.marcas = map[string]bool{}, map[string]noImagen{}, map[string]bool{}, map[string][]byte{}
		e.uno = make(chan struct{}, 1)
	}
}

// esPrepImagen dice si la máquina id es una de preparación (con m.mu
// tomado o no: solo lee byID bajo su propio candado).
func (m *Manager) esPrepImagen(id string) bool {
	m.mu.RLock()
	mc := m.byID[id]
	var nombre string
	if mc != nil {
		nombre = mc.Name
	}
	m.mu.RUnlock()
	if nombre == "" {
		return false
	}
	m.img.mu.Lock()
	defer m.img.mu.Unlock()
	return m.img.prep[nombre]
}

func (m *Manager) esPrepImagenNombre(nombre string) bool {
	m.img.mu.Lock()
	defer m.img.mu.Unlock()
	return m.img.prep[nombre]
}

// imgSnapBootArg es el parámetro del kernel de la máquina de preparación.
func imgSnapBootArg(prep bool) string {
	if !prep {
		return ""
	}
	return " " + api.ImageSnapshotBootParam + "=1"
}

// esSnapshotImagen dice si dir es un snapshot por imagen.
func esSnapshotImagen(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, marcaImagen))
	return err == nil
}

// imagenDeSnapshotImagen es la imagen de la marca de dir ("?" si no se lee).
func imagenDeSnapshotImagen(dir string) string {
	var c claveImagen
	b, err := os.ReadFile(filepath.Join(dir, marcaImagen))
	if err != nil || json.Unmarshal(b, &c) != nil || c.Image == "" {
		return "?"
	}
	return c.Image
}

// marcaImagenPendiente es la marca que commit tiene que escribir en el
// snapshot name, si es uno por imagen en curso.
func (m *Manager) marcaImagenPendiente(name string) ([]byte, bool) {
	m.img.mu.Lock()
	defer m.img.mu.Unlock()
	b, ok := m.img.marcas[name]
	return b, ok
}

// motivoSinSnapshotImagen dice por qué req no puede salir de un snapshot por
// imagen ("" si puede). Fuera, todo lo que se decide en el arranque y no se
// puede dar a una máquina ya restaurada: discos (volúmenes, carpetas de
// copia) y lo que el agente espera desde la línea del kernel (carpetas vivas,
// volúmenes sin montar).
func (m *Manager) motivoSinSnapshotImagen(req api.RunRequest) string {
	if ok, motivo := snapshotsImagenActivos(); !ok {
		return motivo
	}
	switch {
	case req.Fresh:
		return "fresh"
	case req.From != "":
		return "from a template"
	case len(req.VolumeSet()) > 0:
		return "it has volumes"
	case len(req.Shares) > 0 || req.ShareWait > 0:
		return "it has shared folders"
	case req.VolumeWait:
		return "volume_wait"
	case m.esPrepImagenNombre(req.Name):
		return "it is an image snapshot's own preparation"
	}
	return ""
}

// claveImagenDe es la clave de req, ya normalizada por run (vCPU, memoria y
// disco por defecto puestos).
func (m *Manager) claveImagenDe(req api.RunRequest) (claveImagen, error) {
	base, layer, err := m.imageLayer(req.Image)
	if err != nil {
		return claveImagen{}, err
	}
	c := claveImagen{V: versionClaveImagen, Image: req.Image, Backend: backendVMM, Arch: runtime.GOARCH,
		VCPUs: req.VCPUs, MemMiB: req.MemMiB, MemMaxMiB: req.MemMaxMiB, DiskMiB: req.DiskMiB,
		NetMiBps: caudalRed(req.NetMiBps, m.netMiBps), AllowExec: req.AllowExec, IPv6: m.ipv6DeReceta(req.Image)}
	if c.DiskMiB == 0 {
		c.DiskMiB = defaultOverlayMiB
	}
	if c.Base, err = huellaDeFichero(base); err != nil {
		return claveImagen{}, err
	}
	if layer != "" {
		if c.Layer, err = huellaDeFichero(layer); err != nil {
			return claveImagen{}, err
		}
	}
	if c.Kernel, err = m.kernelHash(); err != nil {
		return claveImagen{}, err
	}
	o := m.origenActual()
	c.VMM, c.Kling, c.MacOS = o.vmm, o.kling, o.macOS
	return c, nil
}

// snapshotImagenListo dice si el snapshot de la clave existe, es uno por
// imagen y su marca dice esa misma clave.
func (m *Manager) snapshotImagenListo(c claveImagen) bool {
	b, err := os.ReadFile(filepath.Join(m.snapDir(c.nombre()), marcaImagen))
	if err != nil {
		return false
	}
	var grabada claveImagen
	if json.Unmarshal(b, &grabada) != nil || grabada != c {
		return false
	}
	_, err = os.Stat(filepath.Join(m.snapDir(c.nombre()), "meta.json"))
	return err == nil
}

// arranqueImagen es lo que runFrom necesita para restaurar de un snapshot por
// imagen en lugar de un dorado: lo propio de la máquina, que llega DESPUÉS de
// restaurar, y lo que run ya decidió como en un arranque en frío.
type arranqueImagen struct {
	env     map[string]string
	comando *api.MachineCmd
	cpuFijo bool
	// id lo rellena runFrom al publicarla: si la restauración falla después,
	// quien llama la borra antes de arrancar en frío. danado: el snapshot no
	// pasó la comprobación de obsoleto, firma o integridad.
	id     string
	danado bool
}

// fallaDelSnapshot dice si un fallo de runDesdeImagen es del snapshot (y hay
// que tirarlo) y no de la petición o del host en ese momento (cuota,
// memoria): uno publicado ya pasó todo eso, así que lo que falló fue
// restaurar, resincronizar o entregar.
func (ai *arranqueImagen) fallaDelSnapshot() bool { return ai.danado || ai.id != "" }

// descartarSnapshotImagen tira el snapshot de c tras un fallo suyo, y no se
// prepara otro hasta pasado reintentoPrepImagen: si es el VMM el que no
// restaura, cada run pagaría una preparación.
func (m *Manager) descartarSnapshotImagen(c claveImagen, causa error) {
	nombre := c.nombre()
	m.img.mu.Lock()
	m.img.iniciar()
	m.img.no[nombre] = noImagen{motivo: "its last restore failed: " + causa.Error(), hasta: time.Now().Add(reintentoPrepImagen)}
	m.img.mu.Unlock()
	if err := m.removeSnapshot(nombre, false); err != nil {
		log.Printf("image snapshot %s: couldn't remove it after a failed restore: %v", nombre, err)
	}
}

// runDesdeImagen restaura req (ya normalizada por run) del snapshot por
// imagen c. Si falla, la máquina a medias ya no existe y quien llama arranca
// en frío.
func (m *Manager) runDesdeImagen(ctx context.Context, req api.RunRequest, c claveImagen, ai *arranqueImagen) (*api.Machine, error) {
	req.From = c.nombre()
	mc, err := m.runFromCon(ctx, req, ai)
	if err != nil && ai.id != "" {
		if _, ok := m.Get(ai.id); ok {
			if rerr := m.Remove(ai.id); rerr != nil {
				log.Printf("image snapshot %s: couldn't remove the failed restore %s: %v", req.From, shortID(ai.id), rerr)
			}
		}
	}
	return mc, err
}

// entregarMaquina es el POST /machine/start de una copia: su entorno y su
// comando. Un fallo es un fallo de la copia (sin él el servicio no arranca).
func (m *Manager) entregarMaquina(ctx context.Context, id string, ai *arranqueImagen) error {
	m.mu.RLock()
	mc := m.byID[id]
	var addr string
	if mc != nil && mc.Reachable() {
		addr = mc.Addr(api.GuestPort)
	}
	m.mu.RUnlock()
	if addr == "" {
		return errors.New("the machine has no address to reach its guest agent")
	}
	b, err := json.Marshal(api.GuestStart{Env: ai.env, Cmd: ai.comando})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, plazoPeticionAgente)
	defer cancel()
	var ultimo error
	// Como el resync: justo tras reanudar, la primera conexión puede fallar.
	for i := 0; i < 5 && ctx.Err() == nil; i++ {
		if i > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+api.GuestStartPath, strings.NewReader(string(b)))
		if err != nil {
			return err
		}
		r.Header.Set("Content-Type", "application/json")
		resp, err := listoClient.Do(r)
		if err != nil {
			ultimo = err
			continue
		}
		cuerpo, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		// Un 409 es una copia cuyo agente ya arrancó: en el volcado había una
		// máquina arrancada. No se reintenta y la copia no vale.
		return fmt.Errorf("guest agent answered %d to %s: %s", resp.StatusCode, api.GuestStartPath, strings.TrimSpace(string(cuerpo)))
	}
	if ultimo == nil {
		ultimo = ctx.Err()
	}
	return fmt.Errorf("handing the machine to its guest agent: %w", ultimo)
}

// tocarSnapshotImagen apunta el último uso (el mtime de la marca).
func (m *Manager) tocarSnapshotImagen(name string) {
	ahora := time.Now()
	_ = os.Chtimes(filepath.Join(m.snapDir(name), marcaImagen), ahora, ahora)
}

// tras un arranque en frío de req que podía haber salido de un snapshot por
// imagen y no lo tenía: se prepara en segundo plano, una vez por clave.
func (m *Manager) quizaPrepararImagen(req api.RunRequest, c claveImagen) {
	nombre := c.nombre()
	m.img.mu.Lock()
	m.img.iniciar()
	if m.img.preparando[nombre] {
		m.img.mu.Unlock()
		return
	}
	if no, ok := m.img.no[nombre]; ok && time.Now().Before(no.hasta) {
		m.img.mu.Unlock()
		return
	}
	select {
	case m.img.uno <- struct{}{}:
	default:
		m.img.mu.Unlock()
		return // otra en marcha: la próxima run lo intentará
	}
	m.img.preparando[nombre] = true
	m.img.mu.Unlock()

	go func() {
		defer func() {
			m.img.mu.Lock()
			delete(m.img.preparando, nombre)
			m.img.mu.Unlock()
			<-m.img.uno
		}()
		ctx, cancel := context.WithTimeout(context.Background(), plazoPrepImagen)
		defer cancel()
		go func() {
			select {
			case <-m.quit:
				cancel()
			case <-ctx.Done():
			}
		}()
		t0 := time.Now()
		err := m.prepararImagen(ctx, req, c)
		if err != nil {
			hasta := time.Now().Add(reintentoPrepImagen)
			var inc *errImagenIncompatible
			if errors.As(err, &inc) {
				// No va a cambiar mientras no cambie la imagen (otra clave).
				hasta = time.Now().Add(100 * 365 * 24 * time.Hour)
			}
			m.img.mu.Lock()
			m.img.no[nombre] = noImagen{motivo: err.Error(), hasta: hasta}
			m.img.mu.Unlock()
			log.Printf("image snapshot of %s not saved (it keeps booting cold): %v", req.Image, err)
			return
		}
		log.Printf("image snapshot %s of %s saved in %s: next runs with the same parameters restore it",
			nombre, req.Image, time.Since(t0).Round(time.Millisecond))
	}()
}

// errImagenIncompatible: la imagen no puede tener snapshot por imagen (su
// agente no sabe esperar, o hay procesos vivos antes del servicio).
type errImagenIncompatible struct{ motivo string }

func (e *errImagenIncompatible) Error() string { return e.motivo }

// prepararImagen arranca la máquina de preparación de c, espera a que su
// agente esté esperando sin nada más vivo, la congela como snapshot por
// imagen y la borra.
func (m *Manager) prepararImagen(ctx context.Context, req api.RunRequest, c claveImagen) error {
	nombre := c.nombre()
	marca, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// Sitio, antes de arrancar nada: los que nadie usa, del más viejo al más
	// nuevo, hasta que quepa uno más del tamaño de la memoria (lo peor).
	tope := topeSnapshotsImagen()
	estimado := int64(max(c.MemMiB, c.MemMaxMiB)) << 20
	if estimado > tope {
		return &errImagenIncompatible{motivo: fmt.Sprintf("it would need up to %d MiB and KLING_IMAGE_SNAPSHOT_MIB allows %d", estimado>>20, tope>>20)}
	}
	m.podarSnapshotsImagen(tope - estimado)

	prep := prefijoPrepImagen + newID()[:8]
	m.img.mu.Lock()
	m.img.prep[prep] = true
	m.img.marcas[nombre] = marca
	m.img.mu.Unlock()
	defer func() {
		m.img.mu.Lock()
		delete(m.img.prep, prep)
		delete(m.img.marcas, nombre)
		m.img.mu.Unlock()
	}()
	mc, err := m.run(ctx, peticionPrepImagen(prep, req, c))
	if err != nil {
		return fmt.Errorf("booting its preparation machine: %w", err)
	}
	id := mc.ID
	defer func() {
		if err := m.Remove(id); err != nil {
			log.Printf("image snapshot %s: couldn't remove its preparation machine %s: %v", nombre, prep, err)
		}
	}()

	ag, err := m.esperarAgentePrep(ctx, id)
	if err != nil {
		return err
	}
	if !ag.Has(api.GuestCapLateStart) || ag.Has(api.GuestCapMCP) {
		return &errImagenIncompatible{motivo: fmt.Sprintf("its guest agent (%s %s) does not wait for the host before starting the service "+
			"(an older agent, a bridge, its own /entrypoint or init); rebuild the image with a current kling-guest to get it", ag.Agent, ag.Version)}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(asentarPrepImagen()):
	}
	// Nada vivo además del agente: un proceso en el volcado despertaría
	// igual en todas las copias.
	procs, err := m.procesosPrep(ctx, id)
	if err != nil {
		return err
	}
	if procs.Count > 0 {
		return &errImagenIncompatible{motivo: fmt.Sprintf("%d process(es) (%s) run before the image's service starts",
			procs.Count, strings.Join(procs.Names, ", "))}
	}
	if _, err := m.commit(ctx, id, nombre, false, comprobarPrepImagen, false, false, false); err != nil {
		return fmt.Errorf("saving it: %w", err)
	}
	m.podarSnapshotsImagen(tope)
	return nil
}

// peticionPrepImagen es la petición de la máquina de preparación prep para
// la clave c, sacada de req (la run que no lo encontró): lo que cambia el
// estado congelado —lo que fija la clave— y NADA de la máquina que lo pidió:
// ni su nombre, ni etiquetas, ni entorno, ni comando, ni red, ni discos, ni
// TTL. Se construye campo a campo a propósito: un campo nuevo de RunRequest
// no llega aquí sin que alguien lo decida (TestSnapshotImagenPrep...).
func peticionPrepImagen(prep string, req api.RunRequest, c claveImagen) api.RunRequest {
	return api.RunRequest{
		Name: prep, Image: c.Image, VCPUs: c.VCPUs, MemMiB: c.MemMiB, MemMaxMiB: c.MemMaxMiB,
		DiskMiB: req.DiskMiB, NetMiBps: req.NetMiBps, AllowExec: c.AllowExec, Egress: "none",
		CPUPct: req.CPUPct, Labels: map[string]string{labelPrepImagen: c.nombre()},
	}
}

// labelPrepImagen: la etiqueta de las máquinas de preparación, para verlas
// en `kling ps` (lo que las reconoce es el nombre: estadoImagen.prep).
const labelPrepImagen = "kling.image-snapshot"

// comprobarPrepImagen se repite con el cerrojo de la máquina tomado, antes
// de pausarla: nada de lo que puede tener una máquina de alguien.
func comprobarPrepImagen(mc *api.Machine) error {
	switch {
	case mc.HasSecrets:
		return errors.New("it received session secrets")
	case len(mc.EnvKeys) > 0 || len(mc.Entrypoint) > 0 || len(mc.Cmd) > 0:
		return errors.New("it has a machine environment or command")
	case len(mc.Volumes) > 0 || len(mc.Shares) > 0:
		return errors.New("it has volumes or shared folders")
	case len(mc.CredentialDomains) > 0:
		return errors.New("it has credentials")
	}
	return nil
}

// esperarAgentePrep espera a que el agente de la máquina de preparación diga
// qué es (conocerAgente lo apunta en la máquina).
func (m *Manager) esperarAgentePrep(ctx context.Context, id string) (*api.GuestAgent, error) {
	for {
		if ag := m.agenteDe(id); ag != nil {
			return ag, nil
		}
		mc, ok := m.Get(id)
		if !ok || mc.State != api.StateRunning {
			return nil, errors.New("its preparation machine stopped before its guest agent answered")
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for its guest agent: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// procesosPrep es GET /procs del agente de la máquina de preparación.
func (m *Manager) procesosPrep(ctx context.Context, id string) (api.GuestProcs, error) {
	var p api.GuestProcs
	mc, ok := m.Get(id)
	if !ok || !mc.Reachable() {
		return p, errors.New("its preparation machine is gone")
	}
	ctx, cancel := context.WithTimeout(ctx, plazoProcesosVivos)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+mc.Addr(api.GuestPort)+api.GuestProcsPath, nil)
	if err != nil {
		return p, err
	}
	resp, err := listoClient.Do(r)
	if err != nil {
		return p, fmt.Errorf("asking its guest agent for its processes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p, fmt.Errorf("asking its guest agent for its processes: %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&p); err != nil {
		return p, fmt.Errorf("asking its guest agent for its processes: %w", err)
	}
	return p, nil
}

// snapshotImagenEnDisco es un snapshot por imagen tal como está en disco.
type snapshotImagenEnDisco struct {
	nombre string
	clave  claveImagen
	uso    time.Time
	creado time.Time
	bytes  int64
}

// listarSnapshotsImagen recorre los snapshots con la marca.
func (m *Manager) listarSnapshotsImagen() []snapshotImagenEnDisco {
	base := filepath.Join(m.root, "snapshots")
	ents, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []snapshotImagenEnDisco
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(base, e.Name())
		ruta := filepath.Join(dir, marcaImagen)
		fi, err := os.Stat(ruta)
		if err != nil {
			continue
		}
		s := snapshotImagenEnDisco{nombre: e.Name(), uso: fi.ModTime(), bytes: diskUsage(dir)}
		if b, err := os.ReadFile(ruta); err == nil {
			_ = json.Unmarshal(b, &s.clave)
		}
		if mf, err := os.Stat(filepath.Join(dir, "meta.json")); err == nil {
			s.creado = mf.ModTime()
		}
		out = append(out, s)
	}
	return out
}

// motivoObsoletoImagen dice por qué s ya no se usará nunca ("" si vale): su
// clave no es la que daría hoy una run con sus mismos parámetros.
func (m *Manager) motivoObsoletoImagen(s snapshotImagenEnDisco) string {
	if s.clave.V != versionClaveImagen {
		return "made by another version of kindling"
	}
	if s.creado.IsZero() {
		return "incomplete"
	}
	req := api.RunRequest{Image: s.clave.Image, VCPUs: s.clave.VCPUs, MemMiB: s.clave.MemMiB, MemMaxMiB: s.clave.MemMaxMiB,
		DiskMiB: s.clave.DiskMiB, AllowExec: s.clave.AllowExec}
	hoy, err := m.claveImagenDe(req)
	if err != nil {
		return "its image is gone"
	}
	// El caudal no se guarda como petición: se compara aparte.
	hoy.NetMiBps = s.clave.NetMiBps
	switch {
	case hoy.Base != s.clave.Base || hoy.Layer != s.clave.Layer || hoy.IPv6 != s.clave.IPv6:
		return "its image changed"
	case hoy.Kernel != s.clave.Kernel:
		return "the kernel changed"
	case hoy != s.clave:
		return "kindling or its VMM changed"
	}
	return ""
}

// podarSnapshotsImagen borra los que ya no valen y, del menos usado al más,
// los que hagan falta para que el total no pase de tope. Nunca uno con
// instancias vivas o en uso (removeSnapshot se niega). Devuelve lo liberado.
func (m *Manager) podarSnapshotsImagen(tope int64) int64 {
	todos := m.listarSnapshotsImagen()
	var liberado, total int64
	var vivos []snapshotImagenEnDisco
	for _, s := range todos {
		if motivo := m.motivoObsoletoImagen(s); motivo != "" {
			if err := m.removeSnapshot(s.nombre, false); err == nil {
				log.Printf("image snapshot %s of %s removed: %s", s.nombre, s.clave.Image, motivo)
				liberado += s.bytes
				continue
			}
		}
		vivos = append(vivos, s)
		total += s.bytes
	}
	sort.Slice(vivos, func(i, j int) bool { return vivos[i].uso.Before(vivos[j].uso) })
	for _, s := range vivos {
		if total <= tope {
			break
		}
		if err := m.removeSnapshot(s.nombre, false); err != nil {
			continue
		}
		log.Printf("image snapshot %s of %s removed: over the %d MiB cap (least recently used)", s.nombre, s.clave.Image, tope>>20)
		total -= s.bytes
		liberado += s.bytes
	}
	return liberado
}

// borrarSnapshotsImagenDe borra los de la imagen image (kling image rm). Un
// error es que alguno sigue en uso.
func (m *Manager) borrarSnapshotsImagenDe(image string) error {
	for _, s := range m.listarSnapshotsImagen() {
		if s.clave.Image != image {
			continue
		}
		if err := m.removeSnapshot(s.nombre, false); err != nil {
			return fmt.Errorf("its image snapshot %s: %w", s.nombre, err)
		}
	}
	return nil
}

// SnapshotsImagen es GET /image-snapshots.
func (m *Manager) SnapshotsImagen() []api.ImageSnapshot {
	vivas := map[string]int{}
	m.mu.RLock()
	for _, mc := range m.byID {
		if mc.ImageSnapshot != "" && mc.State == api.StateRunning {
			vivas[mc.ImageSnapshot]++
		}
	}
	m.mu.RUnlock()
	todos := m.listarSnapshotsImagen()
	out := make([]api.ImageSnapshot, 0, len(todos))
	for _, s := range todos {
		out = append(out, api.ImageSnapshot{
			Name: s.nombre, Image: s.clave.Image, VCPUs: s.clave.VCPUs, MemMiB: s.clave.MemMiB,
			MemMaxMiB: s.clave.MemMaxMiB, DiskMiB: s.clave.DiskMiB, AllowExec: s.clave.AllowExec,
			CreatedAt: s.creado, LastUsed: s.uso, DiskBytes: s.bytes, Instances: vivas[s.nombre],
			Stale: m.motivoObsoletoImagen(s),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.After(out[j].LastUsed) })
	return out
}

// PodarSnapshotsImagen es DELETE /image-snapshots: todos los que nadie usa.
func (m *Manager) PodarSnapshotsImagen() (int, int64) {
	n := 0
	var bytes int64
	for _, s := range m.listarSnapshotsImagen() {
		if err := m.removeSnapshot(s.nombre, false); err == nil {
			n++
			bytes += s.bytes
		}
	}
	return n, bytes
}

// InfoSnapshotsImagen es el resumen de GET /info.
func (m *Manager) InfoSnapshotsImagen() *api.ImageSnapshotsInfo {
	ok, motivo := snapshotsImagenActivos()
	out := &api.ImageSnapshotsInfo{Enabled: ok, Reason: motivo, CapBytes: topeSnapshotsImagen()}
	for _, s := range m.listarSnapshotsImagen() {
		out.Count++
		out.DiskBytes += s.bytes
	}
	return out
}
