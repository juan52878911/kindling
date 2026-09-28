package main

import (
	"log"
	"os/exec"
	"syscall"

	"github.com/juan52878911/kindling/pkg/guest"
)

// Las piezas genéricas del invitado —cosechador, grupos de procesos, MMDS,
// volúmenes, /exec, /dns— viven en pkg/guest y las comparte kling-guest. Estos
// alias mantienen los nombres con los que las usa el resto del puente.

var procReaper = guest.DefaultReaper

func enSuPropioGrupo(cmd *exec.Cmd) { guest.OwnGroup(cmd) }
func matarGrupo(cmd *exec.Cmd)      { guest.KillGroup(cmd) }

func waitFor(cmd *exec.Cmd, exitCh chan syscall.WaitStatus) error {
	return guest.WaitFor(cmd, exitCh)
}

// sessionEnv construye el entorno del proceso de una sesión: el entorno base del
// puente MÁS los secretos comunes de MMDS. Si no hay MMDS o no hay entrada, devuelve
// el entorno base tal cual, así que el comportamiento sin secretos queda intacto.
//
// El segundo valor dice si se inyectó ALGO: la adopción del hijo caliente lo usa
// como veto, porque el caliente se lanzó con el entorno base y el entorno de un
// proceso no se puede cambiar después de exec (ver warm.go).
//
// CAMPO "sessions" RETIRADO. Por seguridad, ya NO se usan secretos por sesión del
// almacén MMDS. El id de sesión se acuña AQUÍ (en el bridge, root, dentro del
// invitado) al lanzar el hijo, y como el bridge y el servidor MCP corren como root
// y leen el almacén completo, una sesión podría leer los secretos de las otras.
//
// ALTERNATIVAS SEGURAS:
//   - Usar `kling template credential`: inyecta secretos en la plantilla, que todas
//     las sesiones ven (seguro si la máquina es efímera por sesión).
//   - VM efímera por sesión: cada sesión en su propia microVM aislada.
//   - Proxy de credenciales: `kling machine credential` (el proxy es fuera del invitado).
//
// El almacén sigue ACEPTANDO "sessions" para atrás-compatibilidad, pero se ignora.
// Ver pkg/guest/mmds.go para más detalles.
//
// Se lee MMDS EN CADA sesión, no una vez al arrancar: el store puede cambiar entre
// sesiones (un secreto se inyecta después del boot, en la microVM ya viva), y una
// caché lo dejaría sin ver justo lo recién inyectado.
func (b *bridge) sessionEnv(id string) ([]string, bool) {
	store, err := guest.FetchMMDS()
	if err != nil {
		log.Printf("session %s: can't read MMDS, starting without its secrets: %v", id[:8], err)
		return b.env, false
	}
	if store == nil {
		return b.env, false
	}

	// Se parte del entorno base y se AÑADEN/PISAN las claves de MMDS comunes.
	// append sobre una copia para no mutar b.env, que comparten todas las sesiones.
	extra := make(map[string]string)
	for k, v := range store.Env { // comunes a todas las sesiones
		extra[k] = v
	}

	// El campo "sessions" se ignora desde aquí para adelante (retirado por seguridad).
	// Si el almacén lo trae, se avisa UNA VEZ.
	if len(store.Sessions) > 0 {
		b.warnedSessions.Do(func() {
			log.Printf("mmds: field 'sessions' is deprecated (no longer safe); " +
				"use 'kling template credential' or 'kling machine credential' instead")
		})
	}

	if len(extra) == 0 {
		return b.env, false
	}

	env := append([]string(nil), b.env...)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	log.Printf("session %s: %d MMDS secret(s) injected into the environment", id[:8], len(extra))
	return env, true
}
