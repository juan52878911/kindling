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
//	GET  /healthz            "ok": el invitado está en pie
//	GET  /dns?host=...       diagnóstico de la resolución de nombres
//	POST /volume/sync        vacía la caché del invitado a los volúmenes
//	POST /volume/release     desmonta los volúmenes (antes de congelar)
//	POST /volume/acquire     los vuelve a montar (después de restaurar)
//	POST /exec               solo si el kernel arrancó con kling.exec=1
//	POST /exec/stream        ídem, en streaming (sandboxes)
//	GET|PUT|DELETE /files    ídem: ficheros dentro de la microVM
package guest

import (
	"log"
	"net/http"
	"os"
	"strings"
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
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/dns", DNSHandler)

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

	if ExecEnabled() {
		mux.HandleFunc("/exec", ExecHandler(a.Env))
		mux.HandleFunc("/exec/stream", StreamExecHandler(a.Env))
		mux.HandleFunc("/files", FilesHandler())
		log.Printf("command execution enabled (%s=1): exec and files are served", execBootParam)
	}
}

// Close desmonta los volúmenes. Hay que llamarlo cuando ya no quede nadie
// escribiendo en ellos: desmontar por debajo de un proceso vivo pierde sus
// escrituras.
func (a *Agent) Close() {
	a.Volumes.Release()
}
