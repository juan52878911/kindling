//go:build darwin

// Package peercred dice si una conexión TCP por loopback la abrió un proceso
// del mismo usuario que kling-vz.
//
// Los reenvíos del agente del invitado escuchan en 127.0.0.1, y en macOS
// cualquier usuario del Mac puede conectar a 127.0.0.1. Detrás del puerto 8080
// del invitado está su agente, con exec y ficheros sin más autenticación: sin
// esto, cualquier cuenta local ejecutaba comandos en un sandbox ajeno sin pasar
// por el daemon. macOS no da las credenciales del otro extremo de un socket TCP
// (LOCAL_PEERCRED solo vale para sockets Unix), así que se busca, entre los
// procesos de NUESTRO usuario, cuál tiene abierto el otro extremo: el socket
// cuyo puerto local es el remoto de la conexión y cuyo puerto remoto es el de
// nuestro listener. Si ninguno lo tiene, no es nuestro.
package peercred

/*
#include <libproc.h>
#include <sys/proc_info.h>
#include <arpa/inet.h>
#include <stdlib.h>

// has_tcp dice si pid tiene un socket TCP con esos puertos (orden de host).
static int has_tcp(int pid, int lport, int fport) {
	int sz = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
	if (sz <= 0) return 0;
	struct proc_fdinfo *fds = malloc(sz);
	if (!fds) return 0;
	sz = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, sz);
	int n = sz / (int)sizeof(struct proc_fdinfo), found = 0;
	for (int i = 0; i < n && !found; i++) {
		if (fds[i].proc_fdtype != PROX_FDTYPE_SOCKET) continue;
		struct socket_fdinfo si;
		if (proc_pidfdinfo(pid, fds[i].proc_fd, PROC_PIDFDSOCKETINFO, &si, sizeof si) != (int)sizeof si) continue;
		if (si.psi.soi_kind != SOCKINFO_TCP) continue;
		struct in_sockinfo *ini = &si.psi.soi_proto.pri_tcp.tcpsi_ini;
		if (ntohs((unsigned short)ini->insi_lport) == lport && ntohs((unsigned short)ini->insi_fport) == fport) found = 1;
	}
	free(fds);
	return found;
}

// owner busca, entre los procesos del usuario uid, el que tiene ese socket.
// Devuelve su pid, o 0.
static int owner(unsigned int uid, int lport, int fport) {
	int n = proc_listpids(PROC_UID_ONLY, uid, NULL, 0);
	if (n <= 0) return 0;
	int *pids = malloc(n + 64 * sizeof(int));
	if (!pids) return 0;
	n = proc_listpids(PROC_UID_ONLY, uid, pids, n + 64 * sizeof(int));
	int cnt = n / (int)sizeof(int), found = 0;
	for (int i = 0; i < cnt && !found; i++) {
		if (pids[i] > 0 && has_tcp(pids[i], lport, fport)) found = pids[i];
	}
	free(pids);
	return found;
}
*/
import "C"

import (
	"net"
	"os"
	"sync"
)

// Checker comprueba conexiones contra el usuario de este proceso. Recuerda los
// últimos procesos que resultaron dueños (normalmente el daemon): mirar
// primero ahí evita recorrer todos los procesos del usuario en cada conexión.
type Checker struct {
	uid int

	mu        sync.Mutex
	recientes []int
}

// New devuelve un Checker para el usuario efectivo de este proceso.
func New() *Checker { return &Checker{uid: os.Geteuid()} }

// Allowed dice si c la abrió un proceso de nuestro usuario.
func (k *Checker) Allowed(c net.Conn) bool {
	local, ok1 := c.LocalAddr().(*net.TCPAddr)
	remoto, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return false
	}
	// El socket del cliente: su puerto local es nuestro remoto, y su puerto
	// remoto es el de nuestro listener.
	lport, fport := C.int(remoto.Port), C.int(local.Port)

	k.mu.Lock()
	recientes := append([]int(nil), k.recientes...)
	k.mu.Unlock()
	for _, pid := range recientes {
		if C.has_tcp(C.int(pid), lport, fport) != 0 {
			return true
		}
	}
	pid := int(C.owner(C.uint(k.uid), lport, fport))
	if pid == 0 {
		return false
	}
	k.mu.Lock()
	k.recientes = append([]int{pid}, quitar(k.recientes, pid)...)
	if len(k.recientes) > 4 {
		k.recientes = k.recientes[:4]
	}
	k.mu.Unlock()
	return true
}

func quitar(s []int, v int) []int {
	out := s[:0:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
