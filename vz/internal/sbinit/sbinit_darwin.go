//go:build darwin

// Package sbinit encierra al proceso en un perfil de sandbox propio
// (sandbox_init_with_parameters). Es lo único de internal/sandbox que
// necesita kling-credproxy: aparte, para que el proxy no arrastre el servidor
// ni la pila de red de los VMM.
package sbinit

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
	"errors"
	"unsafe"
)

// Aplicar encierra este proceso en profile con esos parámetros (pares
// nombre, valor). No tiene vuelta atrás.
func Aplicar(profile string, pares []string) error {
	params := make([]*C.char, 0, len(pares)+1)
	for _, p := range pares {
		cs := C.CString(p)
		defer C.free(unsafe.Pointer(cs))
		params = append(params, cs)
	}
	params = append(params, nil)

	cp := C.CString(profile)
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
