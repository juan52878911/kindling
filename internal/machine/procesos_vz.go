//go:build darwin

package machine

// Qué VMM vivo es de qué máquina, en macOS. No hay /proc: se lee la tabla de
// procesos del kernel (proc_listpids y la línea de órdenes de cada uno con
// sysctl kern.procargs2), lo mismo que hace ps. Antes se lanzaba ps en cada
// vuelta del vigilante: un fork+exec cada 10 s con el Mac ocioso, que en
// batería es justo lo que no se quiere. liveVMs se sigue llamando fuera del
// candado, igual que en Linux.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// procInfoCallListPIDs, procAllPIDs y procUIDOnly son
	// PROC_INFO_CALL_LISTPIDS, PROC_ALL_PIDS y PROC_UID_ONLY de
	// <sys/proc_info.h> (lo que hace proc_listpids).
	procInfoCallListPIDs = 1
	procAllPIDs          = 1
	procUIDOnly          = 4
	// ctlKern y kernProcArgs2 son CTL_KERN y KERN_PROCARGS2 de <sys/sysctl.h>.
	ctlKern       = 1
	kernProcArgs2 = 49
	// procArgsMax acota lo que se lee de una línea de órdenes; la de un
	// kling-vz cabe de sobra, y lo que no quepa no es un VMM nuestro.
	procArgsMax = 256 << 10
)

// pidsVivos son los PID en los que puede haber un VMM nuestro: los de nuestro
// usuario efectivo, o todos si somos root (proc_listpids). Sin root no se
// puede leer la línea de órdenes de otro usuario, así que mirarlos solo
// costaba (la mitad de las llamadas en un Mac normal).
func pidsVivos() ([]int32, error) {
	tipo, uid := uintptr(procAllPIDs), uintptr(0)
	if e := os.Geteuid(); e != 0 {
		tipo, uid = procUIDOnly, uintptr(e)
	}
	n := 4096
	for range 4 {
		buf := make([]int32, n)
		r, _, e := syscall.Syscall6(syscall.SYS_PROC_INFO, procInfoCallListPIDs, tipo, uid, 0,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*4))
		if e != 0 {
			return nil, e
		}
		got := int(r) / 4
		if got < n { // si llena el buffer puede haber más: se repite con el doble
			out := buf[:0]
			for _, p := range buf[:got] {
				if p > 0 {
					out = append(out, p)
				}
			}
			return out, nil
		}
		n *= 2
	}
	return nil, errors.New("proc_listpids: too many processes")
}

// lineaDeOrdenes es la línea de órdenes de pid (argv unido con espacios),
// como la columna command de ps -ww. buf se reutiliza entre llamadas.
func lineaDeOrdenes(pid int32, buf []byte) (string, error) {
	mib := [3]int32{ctlKern, kernProcArgs2, pid}
	n := uintptr(len(buf))
	_, _, e := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), 3,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0, 0)
	if e != 0 {
		return "", e
	}
	return parsearProcArgs2(buf[:n])
}

// parsearProcArgs2 lee el formato de kern.procargs2: argc (int32), la ruta
// del ejecutable acabada en NUL y relleno de NUL, y después argv[0..argc-1],
// cada uno acabado en NUL (detrás viene el entorno, que no se mira).
func parsearProcArgs2(b []byte) (string, error) {
	if len(b) < 4 {
		return "", errors.New("procargs2: short")
	}
	argc := int(binary.LittleEndian.Uint32(b))
	b = b[4:]
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return "", errors.New("procargs2: no executable path")
	}
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	args := make([]string, 0, argc)
	for ; argc > 0 && len(b) > 0; argc-- {
		j := bytes.IndexByte(b, 0)
		if j < 0 {
			j = len(b)
		}
		args = append(args, string(b[:j]))
		b = b[min(j+1, len(b)):]
	}
	return strings.Join(args, " "), nil
}

// tablaProcesos es pid -> línea de órdenes de los procesos cuya línea se
// puede leer (los de otros usuarios no, si no somos root: tampoco serían
// nuestros VMM).
func tablaProcesos() (map[int]string, error) {
	pids, err := pidsVivos()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, procArgsMax)
	out := make(map[int]string, len(pids))
	for _, p := range pids {
		if l, err := lineaDeOrdenes(p, buf); err == nil && l != "" {
			out[int(p)] = l
		}
	}
	return out, nil
}

// liveVMs devuelve qué microVM posee cada kling-vz vivo, reconociéndolo por el
// socket de su línea de órdenes.
func (m *Manager) liveVMs() map[string]int {
	tabla, err := tablaProcesos()
	if err != nil {
		return map[string]int{}
	}
	return vmmsDeTabla(tabla, filepath.Join(m.root, "machines")+"/")
}

// adopt comprueba que el PID apuntado sigue siendo el VMM de esta máquina: los
// PID se reciclan, así que no basta con que exista.
func (m *Manager) adopt(mc *api.Machine) (string, bool) {
	if mc.PID <= 0 {
		return "", false
	}
	out, err := lineaDeOrdenes(int32(mc.PID), make([]byte, procArgsMax))
	if err != nil {
		return "", false
	}
	sock := m.dir(mc.ID) + "/fc.sock"
	if !strings.Contains(out, sock) {
		return "", false
	}
	if _, err := os.Stat(sock); err != nil {
		return "", false
	}
	return sock, true
}

// socketDe: en macOS no hay jailer y el socket está siempre en el directorio de
// la máquina. No se llama a adopt, que lee la tabla de procesos: reconcile lo
// usa bajo m.mu.
func (m *Manager) socketDe(id string, pid int) string { return m.dir(id) + "/fc.sock" }
