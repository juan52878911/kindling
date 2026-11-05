package guest

// kling.imgsnap=1: el agente espera a que el host le dé lo de la máquina.
//
// Es la máquina de preparación de un snapshot por imagen (pkg/api
// guest_imagen.go, docs/snapshot-imagen.md). El agente arranca como siempre
// —monta lo suyo, pone la ruta a MMDS, escucha— pero no lee el entorno ni el
// comando de la máquina, y no lanza el servicio de la imagen: el host la
// congela así, con nada de ninguna máquina dentro. Cada copia restaurada de
// ese volcado recibe, ya resincronizada (reloj, CSPRNG, machine-id), POST
// /machine/start con SU entorno y SU comando, y en ese momento el agente hace
// lo que habría hecho en un arranque en frío: ponerlos encima de los de la
// imagen y lanzar el servicio. Sus procesos nacen después del resync, así que
// siembran sus aleatorios del kernel de esa copia y no del de la plantilla
// (el problema de los dorados con el servicio vivo, issue #175).
//
// Solo si no hay nada vivo que no controle el agente: un /entrypoint propio
// pudo lanzar cosas antes de cederle el PID 1, y con el init de la imagen
// como PID 1 (ServiceSpec.Init) el servicio ya corre. Entonces el parámetro se
// ignora (con un aviso) y el agente arranca como siempre, sin anunciar
// api.GuestCapLateStart: el host lo ve y no congela esa imagen.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// imageSnapshotWanted dice si el host pidió kling.imgsnap=1.
func imageSnapshotWanted() bool {
	return cmdlineParams().values[api.ImageSnapshotBootParam] == "1"
}

// entornoTardio es lo que motivoSinTardio mira del invitado. Variables para
// las pruebas: fuera de una microVM el agente no es el PID 1.
var (
	esPID1         = func() bool { return os.Getpid() == 1 }
	leerEntrypoint = func() ([]byte, error) {
		f, err := os.Open(entrypointPath)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, maxEntrypointGenerado+1))
	}
	servicioDeclarado = func() (*Service, error) { return LoadService(api.GuestServiceSpec) }
)

// motivoSinTardio dice por qué este agente NO puede esperar al host sin
// arrancar nada ("" si puede). Ante la duda, no: el coste de equivocarse es
// un volcado con procesos de otra máquina dentro, y el de abstenerse, un
// arranque en frío.
func motivoSinTardio() string {
	if !esPID1() {
		return "the guest agent is not PID 1 (the image's init runs before it)"
	}
	b, err := leerEntrypoint()
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "cannot read " + entrypointPath + ": " + err.Error()
	default:
		if _, ok := PlainEntrypoint(b); !ok {
			return entrypointPath + " is the image's own (it may start things before the agent)"
		}
	}
	svc, err := servicioDeclarado()
	if err != nil {
		return "cannot read the image's service: " + err.Error()
	}
	if svc != nil && svc.spec.Init {
		return "the image's init is PID 1"
	}
	// El host no los combina nunca con kling.imgsnap; si llegan, no se
	// adivina qué quería.
	switch {
	case MachineEnvRequired(), MachineCmdRequired():
		return "the kernel also asks for a machine environment or command"
	case volumeWaitWanted(), shareWaitWanted() > 0, len(volumeState.Specs()) > 0:
		return "the kernel also asks for volumes or shared folders"
	}
	return ""
}

// prepararTardio decide, en NewDeferred, si este agente espera al host.
func (a *Agent) prepararTardio() {
	if !imageSnapshotWanted() {
		return
	}
	if motivo := motivoSinTardio(); motivo != "" {
		log.Printf("warning: ignoring %s=1: %s; booting as usual", api.ImageSnapshotBootParam, motivo)
		return
	}
	a.tardio = true
	a.preparado = make(chan struct{})
}

// enPreparado es enArranque para lo que el host necesita antes de darle la
// máquina a un agente que espera: /healthz (qué es y que espera) y /procs
// (que no hay nada vivo). Sin kling.imgsnap es lo mismo que enArranque.
func (a *Agent) enPreparado(h http.HandlerFunc) http.HandlerFunc {
	if !a.tardio {
		return a.enArranque(h)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-a.preparado:
		case <-r.Context().Done():
			http.Error(w, "the guest agent is still booting", http.StatusServiceUnavailable)
			return
		}
		h(w, r)
	}
}

// handleStart sirve POST /machine/start (api.GuestStart). Una vez.
func (a *Agent) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, api.GuestStartMaxBody+1))
	if err != nil {
		http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > api.GuestStartMaxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req api.GuestStart
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	env, err := validarEntornoTardio(req.Env)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Cmd != nil {
		if err := api.CheckMachineCmd(*req.Cmd); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	// Hasta que FinishBoot haya puesto lo suyo (la ruta a MMDS): el host no
	// llama antes de ver /healthz, pero un cliente con prisa no debe colarse.
	select {
	case <-a.preparado:
	case <-r.Context().Done():
		http.Error(w, "the guest agent is still booting", http.StatusServiceUnavailable)
		return
	}
	iniciado := false
	a.inicioUna.Do(func() {
		iniciado = true
		a.iniciarTardio(env, req.Cmd)
	})
	if !iniciado {
		http.Error(w, "this machine was already started", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validarEntornoTardio pasa el entorno por las mismas reglas que el daemon
// aplica a RunRequest.Env (el agente no se fía de quien le llama).
func validarEntornoTardio(env map[string]string) (map[string]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	claves := make([]string, 0, len(env))
	for k := range env {
		claves = append(claves, k)
	}
	sort.Strings(claves)
	kv := make([]string, 0, len(env))
	for _, k := range claves {
		if k == "" || strings.Contains(k, "=") {
			return nil, fmt.Errorf("invalid environment entry %q", k)
		}
		kv = append(kv, k+"="+env[k])
	}
	return api.MachineEnvMap(kv)
}

// iniciarTardio es lo que completarArranque y StartService hacen en un
// arranque en frío, con el entorno y el comando que mandó el host en vez de
// los de MMDS. Abre las rutas que esperaban (enArranque).
func (a *Agent) iniciarTardio(env map[string]string, cmd *api.MachineCmd) {
	a.envMu.Lock()
	if len(env) > 0 {
		a.Env = overlayEnv(a.Env, env)
		log.Printf("machine env: %d variable(s) from the host: %s", len(env), strings.Join(api.MachineEnvKeys(env), " "))
	}
	a.machineCmd = cmd
	a.Env = cpuQuirkEnv(a.Env)
	e := append([]string(nil), a.Env...)
	a.envMu.Unlock()
	readyState.setEnv(e)
	logBiblioteca(e)
	log.Printf("machine start: the host handed over this machine; starting the image's service")
	a.StartService()
	close(a.arrancado)
}
