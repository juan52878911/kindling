package machine

// Admisión: decir que no ANTES de arrancar, cuando el host no va a poder.
//
// La cuenta de memoria (memory.go) mira MemAvailable, y eso tiene un punto
// ciego que las pruebas de estrés dejaron a la vista: kindling sobreasigna a
// propósito —una microVM no toca toda la RAM que declara—, así que con el host
// saturado MemAvailable sigue alto mientras el sistema ya está pagando por
// reclamar memoria. El síntoma era un invitado que no llegaba a escuchar en dos
// minutos y un rechazo por plazo agotado en vez de uno claro.
//
// La señal que sí ve eso es PSI (/proc/pressure/memory): el porcentaje del
// tiempo en que alguna tarea estuvo parada esperando memoria. Y el disco tiene
// su propia admisión, porque cada máquina crea un overlay y cada congelación un
// mem.file del tamaño de su RAM.

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
)

// defaultMaxMemPressure es el tope de PSI "some avg10" por encima del cual no se
// admiten máquinas nuevas. 20 % significa que en los últimos 10 s alguna tarea
// pasó dos de cada diez segundos esperando memoria: el host ya está pagando por
// cada página, y una microVM más solo lo empeora.
const defaultMaxMemPressure = 20.0

// maxMemPressure lee KLING_MAX_MEM_PRESSURE; "0" apaga la comprobación.
func maxMemPressure() float64 {
	if v := os.Getenv("KLING_MAX_MEM_PRESSURE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return defaultMaxMemPressure
}

// memPressure devuelve "some avg10" de /proc/pressure/memory, o -1 si el kernel
// no lo expone (PSI desactivado, o no es Linux).
func memPressure() float64 {
	f, err := os.Open("/proc/pressure/memory")
	if err != nil {
		return -1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		campos := strings.Fields(sc.Text())
		if len(campos) == 0 || campos[0] != "some" {
			continue
		}
		for _, c := range campos[1:] {
			if v, ok := strings.CutPrefix(c, "avg10="); ok {
				if x, err := strconv.ParseFloat(v, 64); err == nil {
					return x
				}
			}
		}
	}
	return -1
}

// checkPressure rechaza con 507 si el host está bajo presión de memoria. 507 y
// no otro código a propósito: es falta de memoria de verdad, y quien escala
// (el planificador) responde a un 507 congelando instancias ociosas, que es
// justo lo que alivia la presión.
func checkPressure() error {
	tope := maxMemPressure()
	if tope <= 0 {
		return nil
	}
	p := memPressure()
	if p < 0 || p <= tope {
		return nil
	}
	return &api.StatusError{Code: api.StatusInsufficientMemory, Message: fmt.Sprintf(
		"the host is under memory pressure: tasks spent %.0f%% of the last 10 s waiting for memory (limit %.0f%%).\n"+
			"It still reports free memory because microVMs don't touch all they declare, but it is already "+
			"reclaiming pages; another one would make it worse.\n"+
			"Freeze idle instances (`kling ps`), or raise the limit with KLING_MAX_MEM_PRESSURE", p, tope)}
}

// minFreeDiskMiB es el disco libre mínimo bajo $KLING_ROOT para admitir una
// máquina: KLING_MIN_FREE_DISK_MIB, o 2 GiB.
func minFreeDiskMiB() int64 {
	if v := os.Getenv("KLING_MIN_FREE_DISK_MIB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return 2048
}

// checkDisk rechaza si queda poco disco. 503 y NO 507: el planificador contesta
// a un 507 congelando, y congelar ESCRIBE un mem.file del tamaño de la RAM, que
// es lo último que necesita un disco lleno.
func (m *Manager) checkDisk() error {
	tope := minFreeDiskMiB()
	if tope <= 0 {
		return nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(m.root, &st); err != nil {
		return nil // sin poder medirlo no se bloquea nada
	}
	libre := int64(st.Bavail) * int64(st.Bsize) >> 20
	if libre >= tope {
		return nil
	}
	return &api.StatusError{Code: api.StatusDiskFull, Message: fmt.Sprintf(
		"only %d MiB of disk left under %s (the minimum to start a machine is %d MiB).\n"+
			"Remove warm machines or unused snapshots (`kling ps -a`, `kling snapshots`), "+
			"or lower the minimum with KLING_MIN_FREE_DISK_MIB", libre, m.root, tope)}
}

// admitir es la admisión completa, antes de reservar memoria.
func (m *Manager) admitir() error {
	if err := m.checkDisk(); err != nil {
		return err
	}
	if err := checkPresionPlataforma(); err != nil {
		return err
	}
	return checkPressure()
}
