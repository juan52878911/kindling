package fc

// Conectar a un socket unix cuya ruta no cabe en sun_path.
//
// La ruta de un socket unix tiene un tope: 104 bytes en macOS, 108 en Linux, y
// pasarse da "invalid argument" al conectar. En Linux el socket de cada VMM
// (/var/lib/kindling/machines/<id>/fc.sock) va holgado. En macOS la raíz es
// ~/Library/Application Support/kindling y con un usuario largo, o un
// KLING_ROOT hondo, se pasa. kling-vz se ata a esas rutas con chdir y un nombre
// corto; el cliente no puede hacer lo mismo —chdir es de todo el proceso, y el
// daemon habla con muchos VMM a la vez—, así que conecta a través de un enlace
// simbólico corto: connect() sigue el enlace y el tope se aplica a la ruta que
// se le pasa, no a la resuelta.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// maxSunPath es el tope más estricto de los dos sistemas (macOS). Por debajo,
// se conecta a la ruta tal cual: es el caso de siempre en Linux.
const maxSunPath = 104

// dirEnlaces es dónde van los enlaces cortos: /tmp y no $TMPDIR, que en macOS
// ya mide ~50 bytes y se comería la mitad del margen.
func dirEnlaces() string { return "/tmp/kling-" + strconv.Itoa(os.Getuid()) }

// dialUnix conecta a path, pasando por un enlace corto si no cabe.
func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	if len(path) < maxSunPath {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}
	corto, err := enlaceCorto(dirEnlaces(), path)
	if err != nil {
		return nil, fmt.Errorf("socket path %s is %d bytes (the limit is %d) and no short link could be made: %w",
			path, len(path), maxSunPath, err)
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", corto)
}

// enlaceCorto devuelve un enlace simbólico corto en dir que apunta a destino,
// creándolo si falta. El nombre sale de un hash del destino: el mismo socket
// usa siempre el mismo enlace, y dos sockets distintos nunca comparten uno.
func enlaceCorto(dir, destino string) (string, error) {
	// El directorio es solo nuestro: en /tmp cualquiera puede crear cosas, y un
	// enlace plantado por otro usuario nos haría hablar con SU socket.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is not a private directory of this user", dir)
	}
	h := sha256.Sum256([]byte(destino))
	link := filepath.Join(dir, hex.EncodeToString(h[:8])+".sock")
	if actual, err := os.Readlink(link); err == nil && actual == destino {
		return link, nil
	}
	// Se crea al lado y se renombra encima: dos conexiones a la vez al mismo
	// VMM no pueden ver un enlace a medias ni pisarse con EEXIST.
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	tmp := link + "." + hex.EncodeToString(rnd[:])
	_ = os.Remove(tmp)
	if err := os.Symlink(destino, tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return link, nil
}

// BarrerEnlaces borra los enlaces cortos que apuntan dentro de raiz a un
// directorio que ya no existe: los de máquinas borradas. Sin esto
// /tmp/kling-<uid> acumulaba un enlace por cada máquina que existió. Se mira
// el directorio y no el socket porque una máquina congelada no tiene socket
// (su VMM no corre) y su enlace volverá a servir al descongelarla.
func BarrerEnlaces(raiz string) {
	barrerEnlaces(dirEnlaces(), raiz)
}

func barrerEnlaces(dir, raiz string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	pre := filepath.Clean(raiz) + string(filepath.Separator)
	for _, e := range ents {
		link := filepath.Join(dir, e.Name())
		destino, err := os.Readlink(link)
		if err != nil || !strings.HasPrefix(destino, pre) {
			continue
		}
		if _, err := os.Stat(filepath.Dir(destino)); errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(link)
		}
	}
}
