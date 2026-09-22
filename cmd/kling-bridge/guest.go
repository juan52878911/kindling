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
// puente MÁS los secretos de MMDS que le correspondan (los comunes "env" y los de
// "sessions[<id>]"). Si no hay MMDS o no hay entrada, devuelve el entorno base tal
// cual, así que el comportamiento sin secretos queda intacto.
//
// El segundo valor dice si se inyectó ALGO: la adopción del hijo caliente lo usa
// como veto, porque el caliente se lanzó con el entorno base y el entorno de un
// proceso no se puede cambiar después de exec (ver warm.go).
//
// Se lee MMDS EN CADA sesión, no una vez al arrancar: el store puede cambiar entre
// sesiones (un secreto se inyecta después del boot, en la microVM ya viva), y una
// caché lo dejaría sin ver justo lo recién inyectado.
func (b *bridge) sessionEnv(id string) ([]string, bool) {
	store := guest.FetchMMDS()
	if store == nil {
		return b.env, false
	}

	// Se parte del entorno base y se AÑADEN/PISAN las claves de MMDS. append sobre
	// una copia para no mutar b.env, que comparten todas las sesiones.
	extra := make(map[string]string)
	for k, v := range store.Env { // comunes a todas las sesiones
		extra[k] = v
	}
	for k, v := range store.Sessions[id] { // los de ESTA sesión pisan a los comunes
		extra[k] = v
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
