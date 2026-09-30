package daemon

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
)

// modoSocket: el acceso al daemon equivale a root en este host.
const modoSocket = 0o660

// trasEscucharSocket, si no es nil, se llama con el socket ya creado y antes
// de ponerlo en su sitio. Solo para los tests.
var trasEscucharSocket func(final string)

// escucharSocket crea el socket del daemon en ruta con modoSocket y, si ceder,
// de uid:gid, sin seguir enlaces y sin ventana en la que otro pueda conectar.
//
// Antes se escuchaba en ruta y luego se hacía os.Chmod y os.Chown sobre el
// nombre. Los dos siguen enlaces: quien pudiera escribir en el directorio del
// socket y cambiarlo por un enlace entre medias se hacía dar cualquier fichero
// del host (el daemon es root). Y hasta el Chmod, el socket tenía los permisos
// del umask.
//
// Ahora el socket nace en un directorio privado (0700, del daemon) dentro del
// de destino: ahí nadie más puede cambiar el nombre, así que chmod y chown
// actúan sobre el socket y nada más. Luego se renombra a ruta, que en el mismo
// sistema de ficheros es atómico y reemplaza lo que haya en ruta (un enlace
// incluido) sin seguirlo.
func escucharSocket(ruta string, ceder bool, uid, gid int) (net.Listener, error) {
	dir := filepath.Dir(ruta)
	tmp, err := os.MkdirTemp(dir, ".ks")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	nuevo := filepath.Join(tmp, "s")
	// El limite de sun_path son 104 bytes en macOS y 108 en Linux, y pasarse da
	// "bind: invalid argument", que no menciona ni la longitud ni el socket.
	// El nombre provisional es un poco más largo que el final: cuenta ese.
	if n := len(nuevo); n >= 104 {
		return nil, fmt.Errorf("socket path is too long: the daemon creates it as %s (%d bytes) and a unix socket allows about 104; use a shorter -socket", nuevo, n)
	}
	ln, err := net.Listen("unix", nuevo)
	if err != nil {
		return nil, err
	}
	// Tras el Rename, el nombre original ya no existe: que Close no intente
	// borrarlo (Listen de server.go borra ruta al salir).
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	fallo := func(err error) (net.Listener, error) {
		ln.Close()
		os.Remove(nuevo)
		return nil, err
	}
	if err := os.Chmod(nuevo, modoSocket); err != nil {
		return fallo(err)
	}
	if ceder {
		if err := os.Lchown(nuevo, uid, gid); err != nil {
			log.Printf("warning: couldn't hand off the socket to uid %d: %v", uid, err)
		} else {
			log.Printf("socket handed off to uid %d gid %d", uid, gid)
		}
	}
	if trasEscucharSocket != nil {
		trasEscucharSocket(ruta)
	}
	if err := os.Rename(nuevo, ruta); err != nil {
		return fallo(err)
	}
	avisarDirectorioSocket(dir)
	return ln, nil
}

// avisarDirectorioSocket avisa si otros pueden escribir en el directorio del
// socket: podrían borrarlo y poner el suyo, y el CLI le hablaría a un daemon
// falso.
func avisarDirectorioSocket(dir string) {
	fi, err := os.Stat(dir)
	if err != nil {
		return
	}
	if p := fi.Mode(); p.Perm()&0o022 != 0 && p&os.ModeSticky == 0 {
		log.Printf("SECURITY WARNING: the socket's directory %s is writable by group or others (%v): "+
			"anyone who can write there can replace the socket; use a directory like /run/kling", dir, p.Perm())
	}
}
