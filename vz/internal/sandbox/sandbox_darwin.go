//go:build darwin

// Package sandbox encierra a kling-vz y kling-hv en su perfil de sandbox
// (sandbox_init_with_parameters). Los dos VMM comparten los parámetros del
// perfil y el reparto de ficheros; cada uno trae su perfil.
package sandbox

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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"unsafe"

	"github.com/juan52878911/kindling/vz/internal/server"
)

// sinBroker es la ruta que el perfil recibe cuando no hay broker: no existe
// ni puede crearse (/dev no admite sockets), así que la regla no abre nada.
const sinBroker = "/dev/null/kling-vz-no-broker"

// MaxFicheros es cuántos ficheros de lectura (L0..) y de escritura (E0..)
// se le pasan al perfil como mucho: el kernel y unos pocos discos, de sobra.
const MaxFicheros = 16

// Real resuelve p a su ruta real y absoluta: el sandbox compara rutas reales,
// y en macOS /tmp es /private/tmp.
func Real(p string) string {
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

// Parametros son los pares nombre/valor del perfil para esta VM (ver la
// cabecera de kling-vz.sb).
func Parametros(root, mdir, broker string, c server.Confinamiento) ([]string, error) {
	if broker == "" {
		broker = sinBroker
	}
	if len(c.Lectura) > MaxFicheros || len(c.Escritura) > MaxFicheros {
		return nil, fmt.Errorf("the VM has %d read-only and %d read-write files; the sandbox profile admits %d of each",
			len(c.Lectura), len(c.Escritura), MaxFicheros)
	}
	pares := []string{"ROOT", root, "MDIR", mdir, "NET", uno(c.ConRed), "BROKER", broker, "GFX", uno(c.Graphics),
		"NL", strconv.Itoa(len(c.Lectura)), "NE", strconv.Itoa(len(c.Escritura))}
	// Solo los que hay: el perfil recorre NL y NE, no los 16 (~1 ms menos de
	// sandbox_init con los 4-5 ficheros de una VM normal).
	for i, f := range c.Lectura {
		pares = append(pares, "L"+strconv.Itoa(i), Real(f))
	}
	for i, f := range c.Escritura {
		pares = append(pares, "E"+strconv.Itoa(i), Real(f))
	}
	return pares, nil
}

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

// Opciones dice cómo se encierra un VMM.
type Opciones struct {
	Nombre string // "kling-vz", "kling-hv"
	Perfil string // el texto del perfil
	Sock   string // el socket de la API (su directorio es el de la máquina)
	Broker string // el socket del broker del daemon, o ""
	Logf   func(string, ...any)
}

// Preparar devuelve cómo encerrar este proceso en su sandbox (o nil si no
// hay que hacerlo) y dónde escribir los ficheros de un snapshot una vez
// encerrado. El daemon pasa su raíz de datos en KLING_VZ_CONFINE_ROOT (una
// variable y no un argumento: un VMM anterior la ignora, mientras que un
// argumento desconocido lo haría salir). KLING_VZ_NO_SANDBOX=1 lo apaga, para
// diagnosticar un perfil que una versión nueva de macOS rompa.
//
// Encerrado, el proceso solo escribe en el directorio de su máquina (y en sus
// discos). Un snapshot pedido fuera de él falla con un error claro: el dorado
// de kling commit lo pide el daemon en el directorio de la máquina y lo
// publica él en snapshots/<nombre>/ (internal/machine/publicar_volcado.go);
// nunca se abre snapshots/ a este proceso.
//
// Las rutas se resuelven antes: el sandbox compara rutas reales.
func Preparar(o Opciones) (func(server.Confinamiento) error, func(string) (string, func() error, error)) {
	root := os.Getenv("KLING_VZ_CONFINE_ROOT")
	if root == "" {
		o.Logf("not confined: KLING_VZ_CONFINE_ROOT is not set (the daemon sets it)")
		return nil, nil
	}
	if os.Getenv("KLING_VZ_NO_SANDBOX") == "1" {
		o.Logf("WARNING: not confined, KLING_VZ_NO_SANDBOX=1")
		return nil, nil
	}
	root, mdir := Real(root), Real(filepath.Dir(o.Sock))
	var confinado bool
	destino := func(p string) (string, func() error, error) {
		if !confinado || Real(filepath.Dir(p)) == mdir {
			return p, nil, nil
		}
		return "", nil, fmt.Errorf("%s is outside this machine's directory (%s): a confined %s only writes there; "+
			"the daemon publishes committed snapshots", p, mdir, o.Nombre)
	}
	confine := func(c server.Confinamiento) error {
		pares, err := Parametros(root, mdir, o.Broker, c)
		if err != nil {
			return err
		}
		if err := Aplicar(o.Perfil, pares); err != nil {
			return err
		}
		confinado = true
		o.Logf("confined: reads %d and writes %d files of this VM besides %s, network out: %v",
			len(c.Lectura), len(c.Escritura), mdir, c.ConRed)
		return nil
	}
	return confine, destino
}
