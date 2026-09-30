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
	"fmt"
	"path/filepath"
	"strconv"
	"unsafe"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/vz/internal/server"
)

//go:embed kling-vz.sb
var perfil string

// sinBroker es la ruta que el perfil recibe cuando no hay broker: no existe
// ni puede crearse (/dev no admite sockets), así que la regla no abre nada.
const sinBroker = "/dev/null/kling-vz-no-broker"

// maxFicheros es cuántos ficheros de lectura (L0..) y de escritura (E0..)
// admite el perfil: el kernel y unos pocos discos, de sobra.
const maxFicheros = 16

// real resuelve p a su ruta real y absoluta: el sandbox compara rutas reales,
// y en macOS /tmp es /private/tmp.
func real(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

func uno(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// parametros son los pares nombre/valor de kling-vz.sb para esta VM.
func parametros(root, mdir, broker string, c server.Confinamiento) ([]string, error) {
	if broker == "" {
		broker = sinBroker
	}
	if len(c.Lectura) > maxFicheros || len(c.Escritura) > maxFicheros {
		return nil, fmt.Errorf("the VM has %d read-only and %d read-write files; the sandbox profile admits %d of each",
			len(c.Lectura), len(c.Escritura), maxFicheros)
	}
	pares := []string{"ROOT", root, "MDIR", mdir, "NET", uno(c.ConRed), "LOOP", uno(c.ConRed && c.Loopback),
		"BROKER", broker, "GFX", uno(c.Graphics),
		"FMIN", strconv.Itoa(credproxy.ForwardPortMin), "FMAX", strconv.Itoa(credproxy.ForwardPortMax)}
	for i := 0; i < maxFicheros; i++ {
		l, e := "", ""
		if i < len(c.Lectura) {
			l = real(c.Lectura[i])
		}
		if i < len(c.Escritura) {
			e = real(c.Escritura[i])
		}
		pares = append(pares, "L"+strconv.Itoa(i), l, "E"+strconv.Itoa(i), e)
	}
	return pares, nil
}

// confinar encierra este proceso en kling-vz.sb con los ficheros y la red de
// su VM. No tiene vuelta atrás. broker es el socket del daemon para las
// aristas ("" si no hay).
func confinar(root, mdir, broker string, c server.Confinamiento) error {
	pares, err := parametros(root, mdir, broker, c)
	if err != nil {
		return err
	}
	return aplicar(perfil, pares)
}

// aplicar encierra este proceso en profile con esos parámetros.
func aplicar(profile string, pares []string) error {
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

// perfilCustodio es el sandbox del custodio de snapshots (internal/custodio):
// leer el directorio de su máquina y escribir bajo snapshots/, nada más; ni
// red, ni lanzar procesos. Las comprobaciones de qué escribe ahí son del
// propio custodio; esto acota lo que haría uno con un fallo.
const perfilCustodio = `(version 1)
(deny default)
(import "system.sb")
(allow file-read-metadata)
(allow file-read* (subpath (param "MDIR")))
(allow file-read* file-write* (subpath (param "SNAPS")))
`

// confinarCustodio encierra el proceso custodio en perfilCustodio.
func confinarCustodio(snaps, mdir string) error {
	return aplicar(perfilCustodio, []string{"SNAPS", snaps, "MDIR", mdir})
}

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
