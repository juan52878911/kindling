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
//	GET  /meminfo            MemTotal y MemAvailable del invitado (squeeze en macOS)
//	POST /volume/sync        vacía la caché del invitado a los volúmenes
//	POST /volume/release     desmonta los volúmenes (antes de congelar)
//	POST /volume/acquire     los vuelve a montar (después de restaurar)
//	POST /share/attach       carpeta compartida en vivo (Upgrade: kling-share/1)
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

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/share"
)

// Agent es el estado del agente de invitado de este proceso.
type Agent struct {
	// Env es el entorno para los procesos que lance el invitado: el del
	// proceso más NODE_PATH/PYTHONPATH apuntando a los volúmenes que traen
	// paquetes. Se calcula una vez, después de montar.
	Env []string

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
}

// Caps son las capacidades que anuncia /healthz: las rutas que Register sirve
// en este proceso y las que añade quien lo embebe.
func (a *Agent) Caps() []string {
	caps := []string{api.GuestCapResync, api.GuestCapReady, api.GuestCapHooks, api.GuestCapMemInfo,
		api.GuestCapVolume, api.GuestCapShare, api.GuestCapBootOpt, api.GuestCapService}
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
func New() (*Agent, error) {
	a := &Agent{Reaper: DefaultReaper, Volumes: volumeState}
	go a.Reaper.Run()

	// Best-effort: si nadie usa secretos por MMDS es inocuo, y si se usan es
	// esta ruta la que los hace legibles.
	SetupMMDSRoute()

	if err := a.Volumes.Acquire(); err != nil {
		return nil, err
	}
	// Después de montar, no antes: hay que mirar dentro de los volúmenes para
	// saber cuáles traen paquetes.
	a.Env = LibraryEnv(os.Environ(), a.Volumes.Specs())
	readyState.setEnv(a.Env)
	for _, kv := range a.Env {
		if strings.HasPrefix(kv, "NODE_PATH=") || strings.HasPrefix(kv, "PYTHONPATH=") {
			log.Printf("library: %s", kv)
		}
	}
	return a, nil
}

// Register añade las rutas del agente a mux. /exec solo existe si el kernel la
// encendió: en una microVM de servicio no está registrada siquiera, porque una
// capacidad de ejecutar comandos que solo depende de no ser alcanzable acaba
// siendo alcanzada.
func (a *Agent) Register(mux *http.ServeMux) {
	mux.HandleFunc(api.GuestHealthPath, a.handleHealthz)
	mux.HandleFunc("/dns", DNSHandler)
	// /resync la llama el daemon tras cada restauración: reloj y CSPRNG propios
	// en cada instancia de un mismo snapshot (ver resync.go).
	mux.HandleFunc(api.GuestResyncPath, ResyncHandler())
	// /ready y /hooks: la sonda y los ganchos que declara la imagen. Sin
	// kling.exec: ejecutan lo que la imagen trae, nunca lo que pida quien llama.
	mux.HandleFunc(api.GuestReadyPath, ReadyHandler())
	mux.HandleFunc(api.GuestHooksPath, HooksHandler())
	mux.HandleFunc(api.GuestMemInfoPath, MemInfoHandler())
	// /service: el servicio que declara la imagen (service.go), sin kling.exec:
	// solo lee su estado y su salida.
	mux.HandleFunc(api.GuestServicePath, ServiceHandler())

	// /volume/sync la llama el daemon antes de matar la microVM. Sin esto lo
	// último que se escribió se queda en la caché de páginas del invitado y muere
	// con él: el volumen perdería justo lo más reciente.
	mux.HandleFunc("/volume/sync", func(w http.ResponseWriter, r *http.Request) {
		a.Volumes.Sync()
		w.WriteHeader(http.StatusNoContent)
	})
	// /volume/release DESMONTA antes de congelar un snapshot dorado: la memoria
	// que se vuelca no puede llevar un ext4 montado, porque cada instancia
	// restaurada arrancaría con metadatos viejos sobre un disco que ya divergió.
	mux.HandleFunc("/volume/release", func(w http.ResponseWriter, r *http.Request) {
		a.Volumes.Release()
		w.WriteHeader(http.StatusNoContent)
	})
	// /volume/acquire vuelve a montar tras restaurar. 500 y no un log: sin el
	// volumen, lo que se escriba acabaría en el overlay y se perdería en silencio.
	mux.HandleFunc("/volume/acquire", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Volumes.Acquire(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// /share/attach la abre el daemon para cada carpeta compartida en vivo
	// (docs/compartir.md). No depende de kling.exec: montar una carpeta no da
	// a nadie una forma de ejecutar nada dentro.
	mux.HandleFunc(share.AttachPath, shareState.AttachHandler())

	if ExecEnabled() {
		mux.HandleFunc("/exec", ExecHandler(a.Env))
		mux.HandleFunc("/exec/stream", StreamExecHandler(a.Env))
		mux.HandleFunc("/exec/pty", ShellHandler(a.Env))
		mux.HandleFunc("/files", FilesHandler())
		log.Printf("command execution enabled (%s=1): exec and files are served", execBootParam)
	}
}

// Close desmonta los volúmenes. Hay que llamarlo cuando ya no quede nadie
// escribiendo en ellos: desmontar por debajo de un proceso vivo pierde sus
// escrituras.
func (a *Agent) Close() {
	// El servicio primero: es quien escribe en los volúmenes.
	StopService()
	shareState.Close()
	a.Volumes.Release()
}
