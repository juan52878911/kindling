package machine

// Consola serie acotada (M-07 / D-04 / A-01).
//
// El stdout de Firecracker ES la consola serie del invitado (console=ttyS0, ver
// bootArgsBase en manager.go): nada la limita ni la rota, así que un invitado
// hostil que haga `cat /dev/urandom > /dev/ttyS0` llena $KLING_ROOT a la
// velocidad de la UART. Eso hace que gcDisk expulse las warm de OTROS
// inquilinos, que checkDisk rechace máquinas nuevas, y que `kling logs` cargue
// el fichero entero en memoria del daemon y de la CLI. Contradice SECURITY.md
// §4: "una microVM no puede degradar a las demás".
//
// No hay tubería (pipe) de por medio a propósito: Firecracker debe sobrevivir a
// un reinicio del daemon (ver comentario de spawn/spawnJailed), y un pipe cuyo
// lector desaparece bloquea o mata al escritor. Por eso el tope se aplica al
// FICHERO: se abre con O_APPEND y el vigilante lo rota in situ cuando pasa de
// consolaMaxBytes, sin recrearlo ni tocar el descriptor que ya tiene abierto
// Firecracker.

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

// consolaMaxBytes es el tamaño de firecracker.log a partir del cual el
// vigilante lo rota. 16 MiB es generoso para un arranque ruidoso (kernel con
// loglevel alto, un pánico con backtrace) y minúsculo frente a lo que
// `cat /dev/urandom > /dev/ttyS0` produciría sin tope.
const consolaMaxBytes = 16 << 20

// consolaKeepBytes es cuánto se conserva en firecracker.log.1 al rotar: basta
// para diagnosticar algo que falló hace segundos, y es lo bastante pequeño
// para que la propia rotación no sea el próximo DoS de disco.
const consolaKeepBytes = 1 << 20

// abrirConsola abre firecracker.log para un VMM que arranca en dir, sea el
// camino normal (spawn) o con jailer (spawnJailed).
//
// O_APPEND es obligatorio, no cosmético: rotarConsola trunca este mismo
// fichero bajo los pies del proceso mientras sigue escribiendo. Sin O_APPEND,
// Firecracker seguiría escribiendo en la posición que tenía su descriptor
// ANTES del truncado —un hueco de ceros hasta ahí, y por debajo del tamaño real
// del fichero, invisible para quien lo lee después—. Con O_APPEND cada
// escritura va siempre al final real, que tras truncar es la posición 0.
//
// Cada arranque (Run, runFrom, Thaw) empieza con un firecracker.log NUEVO: se
// borra el nombre que hubiera y se crea con O_EXCL|O_NOFOLLOW. Antes era
// O_TRUNC sobre lo que hubiera, y el directorio de la máquina es del VMM: con
// KLING_JAILER=0 el VMM ve el disco del host y puede dejar ahí un enlace a
// cualquier fichero, que el daemon, como root, truncaba. unlink borra el
// enlace, no su destino, y O_EXCL no abre nada que ya exista.
func abrirConsola(dir string) (*os.File, error) {
	ruta := filepath.Join(dir, "firecracker.log")
	for i := 0; ; i++ {
		if err := os.Remove(ruta); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		f, err := os.OpenFile(ruta, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o644)
		if errors.Is(err, os.ErrExist) && i < 3 {
			continue // alguien lo volvió a poner entre medias
		}
		return f, err
	}
}

// abrirConsolaExistente abre la consola de dir sin seguir un enlace (ni
// bloquearse en un FIFO) y exige que sea la que creó abrirConsola: un fichero
// regular del daemon con un solo enlace. Lo que el VMM haya puesto en su
// lugar —un enlace o un hardlink a un fichero del host— no se lee, ni se
// trunca, ni se copia.
func abrirConsolaExistente(ruta string, flag int) (*os.File, error) {
	f, err := os.OpenFile(ruta, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || uint64(st.Nlink) != 1 || int(st.Uid) != os.Geteuid() {
		f.Close()
		return nil, fmt.Errorf("%s is not the console the daemon created (a planted link?): not touching it", ruta)
	}
	return f, nil
}

// rotarConsolas rota, si hace falta, la consola de cada máquina conocida. La
// llama el vigilante (watch, en reconcile.go) en cada vuelta.
//
// Recoge los directorios bajo m.mu y rota fuera del candado: rotarConsola solo
// toca ficheros, no el estado en memoria, y un invitado escribiendo a toda
// velocidad no debe bloquear ps/run/stop mientras el vigilante la revisa.
func (m *Manager) rotarConsolas() {
	m.mu.RLock()
	dirs := make([]string, 0, len(m.byID))
	for id := range m.byID {
		dirs = append(dirs, m.dir(id))
	}
	m.mu.RUnlock()

	for _, dir := range dirs {
		if err := rotarConsola(dir); err != nil {
			log.Printf("watch: couldn't rotate console log at %s: %v", dir, err)
		}
	}
}

// rotarConsola rota firecracker.log en dir si pasa de consolaMaxBytes,
// conservando los últimos consolaKeepBytes en firecracker.log.1 (que
// sobrescribe: no es un historial, es solo el remanente de la rotación
// anterior).
//
// Sin log todavía (máquina recién creada, o sin consola en absoluto) no es un
// error: simplemente no hay nada que rotar.
func rotarConsola(dir string) error {
	path := filepath.Join(dir, "firecracker.log")
	f, err := abrirConsolaExistente(path, os.O_RDWR)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() <= consolaMaxBytes {
		return nil
	}

	cola := int64(consolaKeepBytes)
	if fi.Size() < cola {
		cola = fi.Size()
	}
	if _, err := f.Seek(-cola, io.SeekEnd); err != nil {
		return fmt.Errorf("seeking to the tail before rotating %s: %w", path, err)
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("reading the tail before rotating %s: %w", path, err)
	}
	if err := escribirRotado(dir, buf); err != nil {
		return fmt.Errorf("writing firecracker.log.1: %w", err)
	}
	// Truncar EN SITIO, no borrar+recrear: el mismo descriptor que tiene abierto
	// Firecracker sigue siendo válido, y con O_APPEND (ver abrirConsola) sus
	// próximas escrituras van al nuevo final (0). Borrar y recrear el fichero
	// dejaría a Firecracker escribiendo para siempre en un inodo que nadie lee.
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncating %s after rotation: %w", path, err)
	}
	return nil
}

// escribirRotado deja buf en dir/firecracker.log.1 sin escribir a través de
// lo que haya con ese nombre: un temporal nuevo (O_EXCL) y un rename, que
// reemplaza el nombre aunque sea un enlace plantado por el VMM, en vez de
// seguirlo y escribir la consola del invitado en un fichero del host.
func escribirRotado(dir string, buf []byte) error {
	tmp, err := os.CreateTemp(dir, ".firecracker.log.1-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(buf)
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, "firecracker.log.1"))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// leerCola lee como mucho max bytes del final de path. Se usa desde Logs()
// para no cargar en memoria del daemon más que un tail acotado, sin importar
// cuánto haya crecido la consola entre una rotación y la siguiente. Solo si
// es la consola que creó el daemon (abrirConsolaExistente): si no, Logs
// devolvería a quien lo pide un fichero del host.
func leerCola(path string, max int64) ([]byte, error) {
	f, err := abrirConsolaExistente(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return leerColaDe(f, max)
}

// leerColaDe es leerCola sobre un fichero ya abierto (credaudit.go lo abre sin
// seguir enlaces).
func leerColaDe(f *os.File, max int64) ([]byte, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > max {
		if _, err := f.Seek(-max, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	// io.ReadAll, no ReadFull con el tamaño de Stat: rotarConsola puede truncar
	// el fichero entre el Stat y aquí, y leer "lo que quede" es correcto: sigue
	// siendo una cola válida, solo más corta.
	return io.ReadAll(f)
}
