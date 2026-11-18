package main

import (
	"log"
	"maps"
	"os/exec"
	"slices"
	"syscall"

	"github.com/juan52878911/kindling/pkg/guest"
)

// Las piezas genéricas del invitado —cosechador, grupos de procesos, MMDS,
// volúmenes, /exec, /dns— viven en pkg/guest y las comparte kling-guest. Estos
// alias mantienen los nombres con los que las usa el resto del puente.

var procReaper = guest.DefaultReaper

// fetchMMDS es guest.FetchMMDS; variable para que los tests pongan un almacén
// sin hablar con la dirección de enlace local.
var fetchMMDS = guest.FetchMMDS

// El grupo propio lo pone procReaper.StartTracked (lanzarVigilado).
func matarGrupo(cmd *exec.Cmd) { guest.KillGroup(cmd) }

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
	env, n, err := b.entornoMMDS()
	if err != nil {
		log.Printf("session %s: can't read MMDS, starting without its secrets: %v", id[:8], err)
		return b.env, false
	}
	if n > 0 {
		log.Printf("session %s: %d MMDS secret(s) injected into the environment", id[:8], n)
	}
	return env, n > 0
}

// entornoMMDS es sessionEnv sin logs y con el error de MMDS a la vista: quien
// compara entornos (ensureProxyChild) tiene que distinguir «MMDS sin secretos»
// de «MMDS no contestó», que también devuelve b.env.
//
// Las claves van ORDENADAS: el entorno resultante es estable para un mismo
// almacén, y huellaEnv puede compararlo. Recorrer el map tal cual daba un orden
// distinto en cada lectura y la huella cambiaba sin que cambiara ningún secreto.
func (b *bridge) entornoMMDS() ([]string, int, error) {
	store, err := fetchMMDS()
	if err != nil {
		return b.env, 0, err
	}
	if store == nil {
		return b.env, 0, nil
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
		return b.env, 0, nil
	}

	env := append([]string(nil), b.env...)
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		env = append(env, k+"="+extra[k])
	}
	return env, len(extra), nil
}
