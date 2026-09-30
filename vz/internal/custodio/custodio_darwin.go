//go:build darwin

// Package custodio es el proceso que deja un snapshot dorado en su sitio.
//
// kling-vz está confinado (cmd/kling-vz/kling-vz.sb) y no puede escribir en
// <raíz>/snapshots: ahí están los dorados de TODAS las máquinas, y el mem.file
// de un dorado no lleva hash (lo que se restaura de él es la memoria de cada
// instancia futura). Un kling-vz tomado por su invitado que pudiera escribir
// ahí reescribiría los dorados de los demás. Pero `kling commit` pide a
// kling-vz que vuelque en snapshots/<nombre>/, un nombre que no se conoce al
// confinarse. Así que kling-vz vuelca en su propio directorio y este proceso,
// que lanza antes de encerrarse y que se encierra en su propio perfil (leer el
// directorio de la máquina, escribir en snapshots/), clona el fichero a su
// destino.
//
// El custodio no se fía de lo que le pide kling-vz, que puede estar tomado.
// Solo:
//   - toma el origen de un fichero regular del directorio de la máquina, sin
//     seguir enlaces;
//   - escribe snap.file o mem.file en un directorio que ya exista justo debajo
//     de snapshots/ (lo crea el daemon), sin seguir enlaces, que no tenga
//     meta.json (un dorado terminado) y en un fichero que NO exista: nunca
//     pisa nada. Un dorado ya hecho no se puede tocar; lo peor que puede hacer
//     un kling-vz tomado es adelantarse al commit en curso de otra máquina, que
//     entonces falla (el daemon borra el directorio), no queda corrupto.
//   - clona (clonefile de APFS: el mismo coste que un rename, y las escrituras
//     posteriores al origen no llegan al clon); si el sistema de ficheros no
//     clona, copia.
package custodio

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Nombres que el custodio acepta como destino dentro de snapshots/<nombre>/.
var destinos = map[string]bool{"snap.file": true, "mem.file": true}

// metaDorado es lo que marca un dorado terminado (lo escribe el daemon).
const metaDorado = "meta.json"

type peticion struct {
	Origen  string `json:"origen"`
	Destino string `json:"destino"`
}

type respuesta struct {
	Error string `json:"error,omitempty"`
}

// Custodio es el lado de kling-vz.
type Custodio struct {
	mu sync.Mutex
	c  net.Conn
	r  *bufio.Reader
}

// Iniciar lanza el custodio: este mismo ejecutable con args (el modo custodio
// de main). Hay que llamarlo antes de encerrar kling-vz.
func Iniciar(args ...string) (*Custodio, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	mio, suyo := os.NewFile(uintptr(fds[0]), "custodio"), os.NewFile(uintptr(fds[1]), "custodio")
	defer suyo.Close()
	syscall.CloseOnExec(fds[0])
	cmd := exec.Command(exe, args...)
	cmd.ExtraFiles = []*os.File{suyo} // fd 3 en el hijo
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		mio.Close()
		return nil, err
	}
	go func() { _ = cmd.Wait() }()
	c, err := net.FileConn(mio)
	mio.Close()
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	return &Custodio{c: c, r: bufio.NewReader(c)}, nil
}

// Publicar pide al custodio que deje origen (un fichero del directorio de la
// máquina) en destino (snapshots/<nombre>/{snap,mem}.file).
func (k *Custodio) Publicar(origen, destino string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	b, _ := json.Marshal(peticion{Origen: origen, Destino: destino})
	if _, err := k.c.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("snapshot keeper: %w", err)
	}
	line, err := k.r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("snapshot keeper: %w", err)
	}
	var r respuesta
	if err := json.Unmarshal(line, &r); err != nil {
		return fmt.Errorf("snapshot keeper: %w", err)
	}
	if r.Error != "" {
		return fmt.Errorf("snapshot keeper: %s", r.Error)
	}
	return nil
}

// Servir es el proceso custodio: atiende las peticiones que llegan por el
// socket fd hasta que se cierra. snaps es <raíz>/snapshots y mdir el
// directorio de la máquina, ambos rutas reales. Devuelve el código de salida.
func Servir(fd int, snaps, mdir string) int {
	c, err := net.FileConn(os.NewFile(uintptr(fd), "custodio"))
	if err != nil {
		return 2
	}
	defer c.Close()
	r := bufio.NewReader(io.LimitReader(c, 1<<30))
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return 0
		}
		var p peticion
		var resp respuesta
		if len(line) > 16<<10 {
			resp.Error = "request too large"
		} else if err := json.Unmarshal(line, &p); err != nil {
			resp.Error = "bad request"
		} else if err := Publicar(snaps, mdir, p.Origen, p.Destino); err != nil {
			resp.Error = err.Error()
		}
		b, _ := json.Marshal(resp)
		if _, err := c.Write(append(b, '\n')); err != nil {
			return 0
		}
	}
}

// fdReader lee de un descriptor sin adueñarse de él (lo cierra quien lo abrió).
type fdReader int

func (f fdReader) Read(p []byte) (int, error) {
	n, err := unix.Read(int(f), p)
	if n == 0 && err == nil && len(p) > 0 {
		return 0, io.EOF
	}
	if n < 0 {
		n = 0
	}
	return n, err
}

// componente dice si s es un único componente de ruta normal.
func componente(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\x00")
}

// Publicar comprueba y hace lo que pide una petición (ver el comentario del
// paquete). Es lo que corre dentro del custodio; exportada para las pruebas.
func Publicar(snaps, mdir, origen, destino string) error {
	if filepath.Clean(origen) != origen || filepath.Dir(origen) != mdir || !componente(filepath.Base(origen)) {
		return fmt.Errorf("source %q is not a file of this machine's directory", origen)
	}
	dirDorado := filepath.Dir(destino)
	nombre, fichero := filepath.Base(dirDorado), filepath.Base(destino)
	if filepath.Clean(destino) != destino || filepath.Dir(dirDorado) != snaps || !componente(nombre) || !destinos[fichero] {
		return fmt.Errorf("destination %q is not snapshots/<name>/snap.file or mem.file", destino)
	}

	src, err := unix.Open(origen, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	defer unix.Close(src)
	var st unix.Stat_t
	if err := unix.Fstat(src, &st); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("source is not a regular file")
	}

	snapsFd, err := unix.Open(snaps, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("snapshots directory: %w", err)
	}
	defer unix.Close(snapsFd)
	dir, err := unix.Openat(snapsFd, nombre, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("snapshot directory %q (the daemon creates it): %w", nombre, err)
	}
	defer unix.Close(dir)
	var ms unix.Stat_t
	if err := unix.Fstatat(dir, metaDorado, &ms, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return fmt.Errorf("snapshot %q is already finished: it can't be written to", nombre)
	} else if !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("snapshot %q: %w", nombre, err)
	}

	// Clonar: falla con EEXIST si el destino existe, que es lo que se quiere.
	err = unix.Fclonefileat(src, dir, fichero, 0)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("%s already exists in snapshot %q: it is not overwritten", fichero, nombre)
	}
	if !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EXDEV) {
		return fmt.Errorf("cloning into snapshot %q: %w", nombre, err)
	}
	// Sin clonefile (otro sistema de ficheros): copia a un fichero nuevo.
	dst, err := unix.Openat(dir, fichero, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s in snapshot %q: %w", fichero, nombre, err)
	}
	fo := os.NewFile(uintptr(dst), fichero)
	_, err = io.Copy(fo, fdReader(src))
	if err == nil {
		err = fo.Sync()
	}
	fo.Close()
	if err != nil {
		_ = unix.Unlinkat(dir, fichero, 0)
		return fmt.Errorf("copying into snapshot %q: %w", nombre, err)
	}
	return nil
}
