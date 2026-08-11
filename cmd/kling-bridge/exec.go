package main

// Ejecutar un comando DENTRO de la microVM.
//
// Existe para una sola cosa: poblar un volumen. Instalar paquetes es ejecutar
// código de terceros, y hacerlo en el anfitrión —aunque sea en un chroot— es
// justo lo que kindling existe para evitar. Dentro de una microVM desechable, si
// un paquete trae sorpresas, se lleva por delante una máquina que se destruye
// dos segundos después.
//
// SOLO SE ACTIVA si el kernel lo pide (kling.exec=1). Una microVM de servicio no
// tiene esta ruta en absoluto: no está desactivada, no está registrada. Eso
// importa porque el gateway reenvía peticiones a los invitados, y una capacidad
// de ejecutar comandos que solo depende de no ser alcanzable es una capacidad
// que tarde o temprano alguien alcanza.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// execBootParam enciende la ruta /exec. Va en la línea de comandos del kernel,
// que solo escribe el anfitrión: el invitado no puede concedérsela a sí mismo.
const execBootParam = "kling.exec"

type execRequest struct {
	Cmd []string `json:"cmd"`
	Dir string   `json:"dir,omitempty"`
}

type execResponse struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

// execEnabled dice si el kernel encendió la ejecución de comandos.
func execEnabled() bool {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	for _, tok := range strings.Fields(string(b)) {
		if tok == execBootParam+"=1" {
			return true
		}
	}
	return false
}

// handleExec corre un comando y devuelve su salida y su código.
//
// La salida va JUNTA (stdout y stderr entrelazados) y completa al final, no en
// streaming. Es una decisión: quien llama es el daemon poblando un volumen, no
// una persona mirando una consola, y entrelazadas se lee la causa de un fallo en
// el orden en que ocurrió. Un `npm install` que peta escribe el motivo en stderr
// entre líneas de progreso de stdout, y separarlas lo vuelve ilegible.
//
// Un código de salida distinto de cero NO es un error HTTP: la petición se
// atendió perfectamente y la respuesta es "el comando falló, aquí tienes por
// qué". Devolver 500 obligaría a quien llama a distinguir "no pude ejecutarlo"
// de "lo ejecuté y falló", que son cosas muy distintas.
func (b *bridge) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "usa POST", http.StatusMethodNotAllowed)
		return
	}
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("cuerpo inválido: %v", err), http.StatusBadRequest)
		return
	}
	if len(req.Cmd) == 0 {
		http.Error(w, "falta cmd", http.StatusBadRequest)
		return
	}

	cmd := exec.CommandContext(r.Context(), req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = req.Dir
	// El mismo entorno que reciben los servidores MCP, con NODE_PATH y
	// PYTHONPATH ya apuntando a los volúmenes: instalar en un volumen y luego
	// no encontrarlo desde el mismo sitio sería desconcertante.
	cmd.Env = b.env
	out, err := cmd.CombinedOutput()

	resp := execResponse{Output: string(out)}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			resp.ExitCode = ee.ExitCode()
		} else {
			// Ni siquiera se pudo lanzar: no existe el binario, o no hay
			// permisos. Eso sí es un fallo de la petición, y el mensaje tiene
			// que decirlo porque no habrá salida que lo explique.
			resp.ExitCode = -1
			resp.Output += fmt.Sprintf("\nno pude ejecutar %q: %v", req.Cmd[0], err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
