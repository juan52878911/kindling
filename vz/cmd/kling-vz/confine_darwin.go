//go:build darwin

package main

import (
	_ "embed"

	"github.com/juan52878911/kindling/vz/internal/sandbox"
)

// perfil es el sandbox de kling-vz (ver internal/sandbox).
//
//go:embed kling-vz.sb
var perfil string

// perfilFreno es el sandbox del proceso freno del tope de CPU
// (footprint.ServeFreno): mandar señales y mirar procesos, nada más; ni
// ficheros, ni red, ni lanzar procesos.
const perfilFreno = `(version 1)
(deny default)
(import "system.sb")
(allow signal (target others))
(allow process-info-listpids)
(allow process-info-pidinfo)
(allow process-info-pidfdinfo)
`

// confinarFreno encierra el proceso freno en perfilFreno.
func confinarFreno() error { return sandbox.Aplicar(perfilFreno, nil) }
