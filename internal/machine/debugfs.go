package machine

import "errors"

// ErrNoDebugfs dice que no hay debugfs en este host. Va aparte porque debugfs
// solo sirve para INSPECCIONAR imágenes (¿lleva agente?, ¿entiende capas?,
// images cat): en un Mac sin e2fsprogs de Homebrew arrancar, congelar,
// descongelar y exec funcionan igual, y quien pregunta por diagnóstico puede
// seguir sin la respuesta en vez de negarse a arrancar.
var ErrNoDebugfs = errors.New("cannot find debugfs (comes with e2fsprogs)")

// debugfsBin localiza debugfs, que en Debian vive en /sbin y no siempre está en
// el PATH de un servicio de systemd, y en macOS en el prefijo keg-only de
// Homebrew.
func debugfsBin() string {
	return buscarE2fs("debugfs")
}
