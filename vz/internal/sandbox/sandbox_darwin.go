//go:build darwin

// Package sandbox encierra a kling-vz y kling-hv en su perfil de sandbox
// (sandbox_init_with_parameters) y lanza el custodio de snapshots que coloca
// lo que el proceso encerrado no puede escribir (internal/custodio). Los dos
// VMM comparten los parámetros del perfil y el reparto de ficheros; cada uno
// trae su perfil.
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
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/vz/internal/custodio"
	"github.com/juan52878911/kindling/vz/internal/server"
)

// sinBroker es la ruta que el perfil recibe cuando no hay broker: no existe
// ni puede crearse (/dev no admite sockets), así que la regla no abre nada.
const sinBroker = "/dev/null/kling-vz-no-broker"

// MaxFicheros es cuántos ficheros de lectura (L0..) y de escritura (E0..)
// admite el perfil: el kernel y unos pocos discos, de sobra.
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
	pares := []string{"ROOT", root, "MDIR", mdir, "NET", uno(c.ConRed), "LOOP", uno(c.ConRed && c.Loopback),
		"BROKER", broker, "GFX", uno(c.Graphics),
		"FMIN", strconv.Itoa(credproxy.ForwardPortMin), "FMAX", strconv.Itoa(credproxy.ForwardPortMax)}
	for i := 0; i < MaxFicheros; i++ {
		l, e := "", ""
		if i < len(c.Lectura) {
			l = Real(c.Lectura[i])
		}
		if i < len(c.Escritura) {
			e = Real(c.Escritura[i])
		}
		pares = append(pares, "L"+strconv.Itoa(i), l, "E"+strconv.Itoa(i), e)
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

// ServirCustodio es el proceso custodio de snapshots: se encierra y atiende a
// su VMM por el fd 3 hasta que este muere. nombre va en los errores.
func ServirCustodio(nombre, snaps, mdir string) int {
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	if err := Aplicar(perfilCustodio, []string{"SNAPS", snaps, "MDIR", mdir}); err != nil {
		fmt.Fprintf(os.Stderr, "%s: snapshot keeper: %v\n", nombre, err)
		return 1
	}
	return custodio.Servir(3, snaps, mdir)
}

// Opciones dice cómo se encierra un VMM.
type Opciones struct {
	Nombre      string // "kling-vz", "kling-hv": prefijo de los temporales
	Perfil      string // el texto del perfil
	ArgCustodio string // el argumento con el que este binario hace de custodio
	Sock        string // el socket de la API (su directorio es el de la máquina)
	Broker      string // el socket del broker del daemon, o ""
	Logf        func(string, ...any)
}

// Preparar devuelve cómo encerrar este proceso en su sandbox (o nil si no
// hay que hacerlo) y dónde escribir los ficheros de un snapshot una vez
// encerrado. El daemon pasa su raíz de datos en KLING_VZ_CONFINE_ROOT (una
// variable y no un argumento: un VMM anterior la ignora, mientras que un
// argumento desconocido lo haría salir). KLING_VZ_NO_SANDBOX=1 lo apaga, para
// diagnosticar un perfil que una versión nueva de macOS rompa.
//
// Encerrado, el proceso solo escribe en el directorio de su máquina (y en sus
// discos). Un snapshot que el daemon pide fuera de él (el dorado de kling
// commit, en snapshots/<nombre>/) se vuelca a un temporal del directorio y el
// custodio lo deja en su sitio. El custodio se lanza aquí, antes de
// encerrarse; si no arranca, esos snapshots fallan (nunca se abre snapshots/
// a este proceso).
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
	snaps := filepath.Join(root, "snapshots")
	k, err := custodio.Iniciar(o.ArgCustodio, snaps, mdir)
	if err != nil {
		o.Logf("warning: no snapshot keeper process, snapshots outside %s will fail: %v", mdir, err)
	}
	var confinado bool
	destino := func(p string) (string, func() error, error) {
		d := Real(filepath.Dir(p))
		if !confinado || d == mdir {
			return p, nil, nil
		}
		if k == nil {
			return "", nil, fmt.Errorf("%s is outside this machine's directory and there is no snapshot keeper", p)
		}
		dst := filepath.Join(d, filepath.Base(p))
		tmp := filepath.Join(mdir, "."+o.Nombre+"-publish-"+filepath.Base(p))
		return tmp, func() error { return k.Publicar(tmp, dst) }, nil
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
		o.Logf("confined: reads %d and writes %d files of this VM besides %s, network out: %v, loopback: %v",
			len(c.Lectura), len(c.Escritura), mdir, c.ConRed, c.ConRed && c.Loopback)
		return nil
	}
	return confine, destino
}
