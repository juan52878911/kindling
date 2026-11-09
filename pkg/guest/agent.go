// Package guest es el agente que corre como PID 1 dentro de cada microVM de
// kindling: monta los volúmenes, recoge procesos huérfanos, prepara la ruta al
// servicio de metadatos (MMDS) y sirve las rutas que el daemon usa para hablar
// con el invitado.
//
// No sabe nada de MCP. El puente MCP (kling-bridge) lo embebe y añade encima
// sus propias rutas; un invitado sin servidor MCP —el de `kling images
// toolchain`, un sandbox de código— lo usa tal cual a través de kling-guest.
//
// Rutas que registra:
//
//	GET  /healthz            "ok": el invitado está en pie; con Accept JSON,
//	                         qué agente es, su versión y sus capacidades
//	GET  /dns?host=...       diagnóstico de la resolución de nombres
//	POST /resync             hora del host y entropía fresca, tras restaurar;
//	                         lanza los ganchos de la imagen (ready.go)
//	GET  /ready              ¿terminó de arrancar según su imagen? (ready.go)
//	POST /hooks              vuelve a lanzar los ganchos tras restaurar
//	GET  /service            el servicio que declara la imagen (service.go)
//	POST /service/stop       lo para con su señal (el daemon, antes de matar)
//	GET  /meminfo            MemTotal y MemAvailable del invitado (squeeze en macOS)
//	POST /volume/sync        vacía la caché del invitado a los volúmenes
//	POST /volume/release     desmonta los volúmenes (antes de congelar)
//	POST /volume/acquire     los vuelve a montar (después de restaurar)
//	POST /share/attach       carpeta compartida en vivo (Upgrade: kling-share/1)
//	POST /machine/start      entorno y comando de la máquina a un agente que
//	                         espera (kling.imgsnap=1, arranque_tardio.go)
//	POST /exec               solo si el kernel arrancó con kling.exec=1
//	POST /exec/stream        ídem, en streaming (sandboxes)
//	POST /exec/pty           ídem, con pseudoterminal: la shell interactiva
//	GET|PUT|DELETE /files    ídem: ficheros dentro de la microVM
package guest

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	share "github.com/juan52878911/kindling/internal/shareproto"
	"github.com/juan52878911/kindling/pkg/api"
)

// Agent es el estado del agente de invitado de este proceso.
type Agent struct {
	// Env es el entorno para los procesos que lance el invitado: el del
	// proceso más NODE_PATH/PYTHONPATH apuntando a los volúmenes que traen
	// paquetes. Se calcula después de montar: en New, o con VolumeWait cuando
	// el host monta los volúmenes (recalcularBiblioteca). Leerlo con Entorno.
	Env   []string
	envMu sync.RWMutex
	// bibUna: con VolumeWait, NODE_PATH/PYTHONPATH se recalculan una vez,
	// al montarse los volúmenes.
	bibUna sync.Once

	// Reaper es el cosechador de huérfanos del proceso. Cualquier hijo que se
	// quiera esperar tiene que arrancarse con Reaper.StartTracked, o el
	// cosechador puede robarle el estado de salida.
	Reaper *Reaper

	// Volumes son los volúmenes que pidió el kernel (kling.volume=...).
	Volumes *Volumes

	// Name y Version son el binario que embebe al agente ("kling-guest",
	// "kling-bridge") y su versión: los devuelve /healthz para que el host
	// sepa qué agente lleva cada imagen sin tener que sondearlo por rutas.
	Name, Version string
	// ExtraCaps son las capacidades que añade quien embebe al agente (el
	// puente: api.GuestCapMCP). Se fijan antes de Register.
	ExtraCaps []string

	// envErr: el host arrancó la máquina con entorno propio (kling.env=1) y
	// no se pudo leer. El servicio no arranca sin él (machine_env.go).
	envErr error
	// machineCmd y cmdErr: el comando de la máquina (kling.cmd=1) o por qué
	// no se pudo leer (machine_cmd.go).
	machineCmd *api.MachineCmd
	cmdErr     error

	// arrancado se cierra cuando el entorno y el comando de la máquina ya
	// están aplicados y el servicio lanzado: hasta entonces las rutas que
	// dependen de ellos esperan (enArranque). nil (un Agent de las pruebas,
	// o el de New) no espera. diferido y espera son lo que FinishBoot tiene
	// pendiente con NewDeferred.
	arrancado chan struct{}
	diferido  bool
	espera    bool
	finUna    sync.Once

	// tardio: kling.imgsnap=1 y este agente puede cumplirlo
	// (arranque_tardio.go). preparado se cierra cuando FinishBoot acaba lo
	// que no es de la máquina; arrancado, con POST /machine/start. inicioUna
	// hace que ese arranque sea uno solo, y que Close lo impida.
	tardio    bool
	preparado chan struct{}
	inicioUna sync.Once
}

// Caps son las capacidades que anuncia /healthz: las rutas que Register sirve
// en este proceso y las que añade quien lo embebe.
func (a *Agent) Caps() []string {
	caps := []string{api.GuestCapResync, api.GuestCapReady, api.GuestCapHooks, api.GuestCapMemInfo,
		api.GuestCapVolume, api.GuestCapShare, api.GuestCapBootOpt, api.GuestCapEnv, api.GuestCapProcs, api.GuestCapCmd,
		api.GuestCapVolumeWait, CapResyncClock, api.GuestCapDropCaches}
	if a.tardio {
		caps = append(caps, api.GuestCapLateStart)
	}
	// "service" solo si la imagen declara uno: el daemon pide pararlo antes
	// de matar la máquina, y sin servicio sería un viaje para nada.
	if ServiceDeclared() {
		caps = append(caps, api.GuestCapService)
	}
	if ExecEnabled() {
		caps = append(caps, api.GuestCapExec)
	}
	return append(caps, a.ExtraCaps...)
}

// handleHealthz contesta "ok" como siempre, o GuestHealth si se pide JSON. El
// texto plano no cambia: hosts anteriores y scripts solo miran que conteste.
func (a *Agent) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Accept"), "application/json") {
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(api.GuestHealth{Status: "ok", Agent: a.Name, Version: a.Version, Caps: a.Caps()})
}

// New prepara el agente: arranca el cosechador, añade la ruta a MMDS y monta los
// volúmenes. Si el kernel pidió un volumen y no se puede montar devuelve error:
// es mejor morir al arrancar —se ve en la consola serie— que dejar que algo
// escriba en un directorio del overlay que desaparece con la máquina.
//
// Lee el entorno y el comando de la máquina de MMDS antes de volver: quien lo
// embebe (el puente) los usa en cuanto lo tiene. NewDeferred deja esa parte
// para después de escuchar.
func New() (*Agent, error) {
	a, err := newAgent(false)
	if err != nil {
		return nil, err
	}
	a.completarArranque()
	return a, nil
}

// NewDeferred es New sin lo que espera a MMDS: la ruta a 169.254.169.254, el
// entorno y el comando de la máquina. Se escucha enseguida y FinishBoot lo
// hace después; mientras tanto /healthz y lo que depende del entorno esperan
// (enArranque). Con un MMDS mudo eran hasta ~12 s sin escuchar.
//
// /healthz espera y no contesta "pendiente" porque el daemon borra el entorno
// de MMDS en cuanto el agente le contesta, sea lo que sea
// (internal/machine retirarEntornoMMDS): contestar antes de leerlo sería
// perderlo. Lo que se gana es no tener que volver a llamar: la respuesta sale
// en el instante en que el entorno está, sin conexiones rechazadas ni pausas
// entre intentos, y las rutas que no lo necesitan (volúmenes, carpetas, DNS)
// ya atienden.
func NewDeferred() (*Agent, error) {
	a, err := newAgent(true)
	if err != nil {
		return nil, err
	}
	a.arrancado = make(chan struct{})
	a.prepararTardio()
	return a, nil
}

func newAgent(diferido bool) (*Agent, error) {
	a := &Agent{Reaper: DefaultReaper, Volumes: volumeState, diferido: diferido}
	go a.Reaper.Run()
	ajustarLimiteMemoria()

	// Best-effort: si nadie usa secretos por MMDS es inocuo, y si se usan es
	// esta ruta la que los hace legibles. Diferido, la pone FinishBoot.
	if !diferido {
		SetupMMDSRoute()
	}

	// Con kling.volwait=1 los monta el host (POST /volume/acquire) cuando
	// toque: volume_wait.go.
	a.espera = volumeWaitWanted()
	if a.espera {
		log.Printf("volumes: left unmounted until the host asks (%s=1)", api.VolumeWaitBootParam)
	} else if err := a.Volumes.Acquire(); err != nil {
		return nil, err
	}
	// Después de montar, no antes: hay que mirar dentro de los volúmenes para
	// saber cuáles traen paquetes.
	a.Env = LibraryEnv(os.Environ(), a.Volumes.Specs())
	return a, nil
}

// completarArranque aplica el entorno y el comando de la máquina.
func (a *Agent) completarArranque() {
	// El entorno de la máquina, encima del de la imagen: antes de contestar a
	// /healthz, que es cuando el daemon lo borra de MMDS.
	// Una sola lectura (con sus reintentos) para los dos: con un MMDS mudo,
	// dos seguidas doblaban la espera antes de /healthz.
	a.envMu.Lock()
	a.applyMachineBoot(MachineEnvRequired(), MachineCmdRequired(), FetchMMDS)
	a.Env = cpuQuirkEnv(a.Env)
	env := append([]string(nil), a.Env...)
	a.envMu.Unlock()
	readyState.setEnv(env)
	logBiblioteca(env)
	if a.espera {
		// Sin los volúmenes montados, LibraryEnv no ve sus paquetes: cuando el
		// host los monte, otra vez (los exec también, no solo el servicio).
		go func() {
			<-a.Volumes.acquiredCh()
			a.recalcularBiblioteca()
		}()
	}
}

// FinishBoot es lo que NewDeferred dejó pendiente —la ruta a MMDS, el
// entorno y el comando de la máquina—, arranca el servicio de la imagen y
// abre las rutas que esperaban. Una vez; con New no hace nada (el servicio lo
// arranca quien llama, con StartService).
func (a *Agent) FinishBoot() {
	if !a.diferido {
		return
	}
	a.finUna.Do(func() {
		SetupMMDSRoute()
		if a.tardio {
			// Ni entorno, ni comando, ni servicio: los trae POST
			// /machine/start (arranque_tardio.go).
			log.Printf("machine start: waiting for the host (%s=1)", api.ImageSnapshotBootParam)
			close(a.preparado)
			return
		}
		a.completarArranque()
		a.StartService()
		close(a.arrancado)
	})
}

// enArranque hace esperar a h hasta que FinishBoot acabe (o el cliente se
// vaya). /healthz contestado es, para el daemon, "ya leyó su entorno"; un
// /exec o un /ready antes correrían sin él.
func (a *Agent) enArranque(h http.HandlerFunc) http.HandlerFunc {
	if a.arrancado == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-a.arrancado:
		case <-r.Context().Done():
			http.Error(w, "the guest agent is still booting", http.StatusServiceUnavailable)
			return
		}
		h(w, r)
	}
}

func logBiblioteca(env []string) {
	for _, kv := range env {
		if strings.HasPrefix(kv, "NODE_PATH=") || strings.HasPrefix(kv, "PYTHONPATH=") {
			log.Printf("library: %s", kv)
		}
	}
}

// Entorno es el entorno de los procesos del invitado (ver Env), en este
// momento.
func (a *Agent) Entorno() []string {
	a.envMu.RLock()
	defer a.envMu.RUnlock()
	return append([]string(nil), a.Env...)
}

// recalcularBiblioteca añade a Env los NODE_PATH/PYTHONPATH de los volúmenes
// ya montados. Una vez: la llaman el aviso del montaje y el arranque del
// servicio, el primero que llegue.
func (a *Agent) recalcularBiblioteca() {
	a.bibUna.Do(func() {
		a.envMu.Lock()
		a.Env = LibraryEnv(a.Env, a.Volumes.Specs())
		env := append([]string(nil), a.Env...)
		a.envMu.Unlock()
		readyState.setEnv(env)
		logBiblioteca(env)
	})
}

// Register añade las rutas del agente a mux. /exec solo existe si el kernel la
// encendió: en una microVM de servicio no está registrada siquiera, porque una
// capacidad de ejecutar comandos que solo depende de no ser alcanzable acaba
// siendo alcanzada.
func (a *Agent) Register(mux *http.ServeMux) {
	mux.HandleFunc(api.GuestHealthPath, a.enPreparado(a.handleHealthz))
	mux.HandleFunc("/dns", DNSHandler)
	// /resync la llama el daemon tras cada restauración: reloj y CSPRNG propios
	// en cada instancia de un mismo snapshot (ver resync.go).
	mux.HandleFunc(api.GuestResyncPath, ResyncHandler())
	// /ready y /hooks: la sonda y los ganchos que declara la imagen. Sin
	// kling.exec: ejecutan lo que la imagen trae, nunca lo que pida quien llama.
	mux.HandleFunc(api.GuestReadyPath, a.enArranque(ReadyHandler()))
	mux.HandleFunc(api.GuestHooksPath, a.enArranque(HooksHandler()))
	mux.HandleFunc(api.GuestMemInfoPath, MemInfoHandler())
	// /drop-caches: el host la pide antes de apretar, congelar o guardar un
	// dorado (dropcaches.go).
	mux.HandleFunc(api.GuestDropCachesPath, DropCachesHandler())
	// /service: el servicio que declara la imagen (service.go), sin kling.exec:
	// solo lee su estado y su salida.
	mux.HandleFunc(api.GuestServicePath, a.enArranque(ServiceHandler()))
	mux.HandleFunc(api.GuestServiceStopPath, a.enArranque(ServiceStopHandler()))
	// /procs: los procesos vivos, para que `kling save` avise de los que el
	// dorado copiaría con sus aleatorios en memoria (procs.go).
	mux.HandleFunc(api.GuestProcsPath, a.enPreparado(ProcsHandler()))
	// /machine/start: solo en una máquina que espera al host.
	if a.tardio {
		mux.HandleFunc(api.GuestStartPath, a.handleStart)
	}

	// /volume/sync la llama el daemon antes de matar la microVM. Sin esto lo
	// último que se escribió se queda en la caché de páginas del invitado y muere
	// con él: el volumen perdería justo lo más reciente.
	mux.HandleFunc("/volume/sync", soloPost(func(w http.ResponseWriter, r *http.Request) {
		a.Volumes.Sync()
		w.WriteHeader(http.StatusNoContent)
	}))
	// /volume/release DESMONTA antes de congelar un snapshot dorado: la memoria
	// que se vuelca no puede llevar un ext4 montado, porque cada instancia
	// restaurada arrancaría con metadatos viejos sobre un disco que ya divergió.
	mux.HandleFunc("/volume/release", soloPost(func(w http.ResponseWriter, r *http.Request) {
		a.Volumes.Release()
		w.WriteHeader(http.StatusNoContent)
	}))
	// /volume/acquire vuelve a montar tras restaurar. 500 y no un log: sin el
	// volumen, lo que se escriba acabaría en el overlay y se perdería en silencio.
	mux.HandleFunc("/volume/acquire", soloPost(func(w http.ResponseWriter, r *http.Request) {
		if err := a.Volumes.Acquire(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// /share/attach la abre el daemon para cada carpeta compartida en vivo
	// (docs/compartir.md). No depende de kling.exec: montar una carpeta no da
	// a nadie una forma de ejecutar nada dentro.
	mux.HandleFunc(share.AttachPath, shareState.AttachHandler())

	if ExecEnabled() {
		// El entorno de cada exec se lee al llegar: con VolumeWait cambia al
		// montarse los volúmenes.
		mux.HandleFunc("/exec", a.enArranque(ExecHandlerFunc(a.Entorno)))
		mux.HandleFunc("/exec/stream", a.enArranque(StreamExecHandlerFunc(a.Entorno)))
		mux.HandleFunc("/exec/pty", a.enArranque(ShellHandlerFunc(a.Entorno)))
		mux.HandleFunc("/files", a.enArranque(FilesHandler()))
		log.Printf("command execution enabled (%s=1): exec and files are served", execBootParam)
	}
}

// soloPost deja pasar solo POST. Las rutas que cambian algo lo exigen: con
// cualquier método, un GET que llegara por otro camino (un cliente que sigue
// una redirección, un enlace) desmontaba o sincronizaba igual.
func soloPost(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

// Close desmonta los volúmenes. Hay que llamarlo cuando ya no quede nadie
// escribiendo en ellos: desmontar por debajo de un proceso vivo pierde sus
// escrituras.
func (a *Agent) Close() {
	// Un FinishBoot a medias acaba antes (y uno sin empezar ya no empieza):
	// si no, lanzaría el servicio después de pararlo, sin volúmenes.
	a.finUna.Do(func() {})
	a.inicioUna.Do(func() {})
	// El servicio primero: es quien escribe en los volúmenes.
	StopService()
	shareState.Close()
	a.Volumes.Release()
}
