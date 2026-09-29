//go:build darwin

package main

/*
#include <stdlib.h>
#include <stdint.h>

// Exportada por libsystem_sandbox; marcada como obsoleta pero es la que usan
// Chromium y el resto para encerrarse a sí mismos con un perfil propio.
int sandbox_init_with_parameters(const char *profile, uint64_t flags, const char *const parameters[], char **errorbuf);
void sandbox_free_error(char *errorbuf);
*/
import "C"

import (
	_ "embed"
	"errors"
	"unsafe"
)

//go:embed kling-vz.sb
var perfil string

// sinBroker es la ruta que el perfil recibe cuando no hay broker: no existe
// ni puede crearse (/dev no admite sockets), así que la regla no abre nada.
const sinBroker = "/dev/null/kling-vz-no-broker"

// confinar encierra este proceso en kling-vz.sb. No tiene vuelta atrás.
// broker es el socket del daemon para las aristas ("" si no hay).
func confinar(root, mdir, broker string, conRed, gfx bool) error {
	red := "0"
	if conRed {
		red = "1"
	}
	if broker == "" {
		broker = sinBroker
	}
	grafica := "0"
	if gfx {
		grafica = "1"
	}
	pares := []string{"ROOT", root, "MDIR", mdir, "NET", red, "BROKER", broker, "GFX", grafica}
	params := make([]*C.char, 0, len(pares)+1)
	for _, p := range pares {
		cs := C.CString(p)
		defer C.free(unsafe.Pointer(cs))
		params = append(params, cs)
	}
	params = append(params, nil)

	cp := C.CString(perfil)
	defer C.free(unsafe.Pointer(cp))
	var errbuf *C.char
	if C.sandbox_init_with_parameters(cp, 0, &params[0], &errbuf) != 0 {
		msg := "sandbox_init failed"
		if errbuf != nil {
			msg = C.GoString(errbuf)
			C.sandbox_free_error(errbuf)
		}
		return errors.New(msg)
	}
	return nil
}

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
func confinarFreno() error {
	cp := C.CString(perfilFreno)
	defer C.free(unsafe.Pointer(cp))
	params := []*C.char{nil}
	var errbuf *C.char
	if C.sandbox_init_with_parameters(cp, 0, &params[0], &errbuf) != 0 {
		msg := "sandbox_init failed"
		if errbuf != nil {
			msg = C.GoString(errbuf)
			C.sandbox_free_error(errbuf)
		}
		return errors.New(msg)
	}
	return nil
}
