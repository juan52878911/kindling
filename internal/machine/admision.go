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
	"sync"
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
// máquina: KLING_MIN_FREE_DISK_MIB, o el de la plataforma (2 GiB en Linux,
// 16 GiB en macOS: ver minDiscoLibrePlataforma).
func minFreeDiskMiB() int64 {
	if v := os.Getenv("KLING_MIN_FREE_DISK_MIB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return minDiscoLibrePlataforma
}

// maxDiskMiB es el tope de RunRequest.DiskMiB: KLING_MAX_DISK_MIB, que solo
// puede bajarlo (entre minOverlayMiB y maxOverlayMiB).
func maxDiskMiB() int {
	if v := os.Getenv("KLING_MAX_DISK_MIB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= minOverlayMiB && n <= maxOverlayMiB {
			return n
		}
	}
	return maxOverlayMiB
}

// checkDiskParaOverlay rechaza un disco escribible (-disk) que no cabe en lo
// que queda libre más el mínimo: es disperso, pero un invitado que lo llene
// dejaría al host sin disco.
func (m *Manager) checkDiskParaOverlay(diskMiB int) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(m.root, &st); err != nil {
		return nil // sin poder medirlo no se bloquea nada
	}
	libre := int64(st.Bavail) * int64(st.Bsize) >> 20
	if necesario := int64(diskMiB) + minFreeDiskMiB(); libre < necesario {
		return &api.StatusError{Code: api.StatusDiskFull, Message: fmt.Sprintf(
			"only %d MiB of disk left under %s: a %d MiB writable disk needs %d MiB free (the disk plus the %d MiB minimum)",
			libre, m.root, diskMiB, necesario, minFreeDiskMiB())}
	}
	return nil
}

// defaultMaxSwapPct es el tope de swap usado (ver evaluarSwap) por encima del
// cual no se admiten máquinas: KLING_MAX_SWAP_PCT, 0 lo apaga.
const defaultMaxSwapPct = 85

func maxSwapPct() int64 {
	if v := os.Getenv("KLING_MAX_SWAP_PCT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 && n <= 100 {
			return n
		}
	}
	return defaultMaxSwapPct
}

// evaluarSwap es la admisión por swap sobre cifras ya leídas (MiB).
//
// macOS no tiene un swap de tamaño fijo: dynamic_pager añade ficheros de
// 1 GiB en el volumen VM mientras haya disco. "Usado sobre total" no dice
// nada (un Mac sano va al 90 % de 4 GiB con 60 GiB libres: crecerá), así que
// la capacidad es lo que YA hay más lo que aún puede crecer sin bajar del
// mínimo de disco libre (libreDisco − minDisco). Con disco de sobra el
// porcentaje es bajo; con el disco en su mínimo es usado/total, que es la
// situación en la que se vieron páginas de la caché del invitado a ceros
// (prototypes/android/docs/sigill.md: swap 8–9 de 9,2 GB con el disco al
// 97–98 %). 507, como el resto de la admisión de memoria.
func evaluarSwap(usado, total, libreDisco, minDisco, maxPct int64) error {
	if maxPct <= 0 || total <= 0 || usado <= 0 {
		return nil
	}
	capacidad := total + max(0, libreDisco-minDisco)
	pct := usado * 100 / capacidad
	if pct < maxPct {
		return nil
	}
	return &api.StatusError{Code: api.StatusInsufficientMemory, Message: fmt.Sprintf(
		"the host swap is %d%% full (%d of %d MiB, and only %d MiB of disk left for it to grow; limit %d%%).\n"+
			"Under this pressure macOS has handed guests pages of zeros (docs/estabilidad.md); another "+
			"microVM would make it worse.\n"+
			"Freeze or remove idle instances (`kling ps`), free disk, or raise the limit with KLING_MAX_SWAP_PCT",
		pct, usado, total, max(0, libreDisco-minDisco), maxPct)}
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

// statfsRaiz es syscall.Statfs, en variable para que los tests simulen un
// disco casi lleno.
var statfsRaiz = syscall.Statfs

// reservarDiscoParaVolcado comprueba Y reserva el disco de un volcado
// (freeze, save): Firecracker escribe un mem.file del tamaño de la RAM de la
// máquina ANTES de que perforarHuecos lo adelgace, y si el disco se acaba a
// medias deja el fichero parcial, la máquina pausada y el resto del host sin
// disco. Medido en el laboratorio con una microVM de 2 GiB y 1,9 GiB libres:
// el disco llegó al 100 % y el daemon de al lado dejó de admitir máquinas.
// Hace falta la RAM entera más el mínimo de siempre, para que el host siga
// admitiendo máquinas después.
//
// Reserva y no solo comprueba, por lo mismo que reserveMemory: N freezes a la
// vez (el planificador congelando para hacer sitio) miraban el mismo disco
// libre y pasaban todos, y juntos no cabían. Lo reservado por los que aún no
// han escrito (volcandoMiB) se suma a lo que pide este. Devuelve la función
// que suelta la reserva, que hay que llamar siempre al terminar de escribir
// (o de fallar).
func (m *Manager) reservarDiscoParaVolcado(memMiB int, que string) (func(), error) {
	var st syscall.Statfs_t
	if err := statfsRaiz(m.root, &st); err != nil {
		return func() {}, nil // sin poder medirlo no se bloquea nada
	}
	libre := int64(st.Bavail) * int64(st.Bsize) >> 20

	m.mu.Lock()
	defer m.mu.Unlock()
	enVuelo := int64(m.volcandoMiB)
	necesario := int64(memMiB) + minFreeDiskMiB()
	if libre >= necesario+enVuelo {
		m.volcandoMiB += memMiB
		var una sync.Once
		return func() {
			una.Do(func() {
				m.mu.Lock()
				m.volcandoMiB = max(0, m.volcandoMiB-memMiB)
				m.mu.Unlock()
			})
		}, nil
	}
	otros := ""
	if enVuelo > 0 {
		otros = fmt.Sprintf(", and other dumps in progress have %d MiB of it reserved", enVuelo)
	}
	if que == "thaw" {
		// Descongelar un diferencial sin reflink: la base se copia entera
		// (diff_volcado.go), y el fichero vive mientras la copia corra.
		return nil, &api.StatusError{Code: api.StatusDiskFull, Message: fmt.Sprintf(
			"only %d MiB of disk left under %s%s: thawing this copy rebuilds its %d MiB of memory from the "+
				"golden snapshot (this filesystem cannot share blocks) and needs %d MiB free (the RAM plus the "+
				"%d MiB minimum).\nRemove warm machines or unused snapshots (`kling ps -a`, `kling snapshots`), "+
				"or lower the minimum with KLING_MIN_FREE_DISK_MIB", libre, m.root, otros, memMiB, necesario, minFreeDiskMiB())}
	}
	return nil, &api.StatusError{Code: api.StatusDiskFull, Message: fmt.Sprintf(
		"only %d MiB of disk left under %s%s: %s dumps the machine's %d MiB of RAM to disk first and "+
			"needs %d MiB free (the RAM plus the %d MiB minimum).\n"+
			"Remove warm machines or unused snapshots (`kling ps -a`, `kling snapshots`), "+
			"or lower the minimum with KLING_MIN_FREE_DISK_MIB", libre, m.root, otros, que, memMiB, necesario, minFreeDiskMiB())}
}

// admitir es la admisión completa, antes de reservar memoria.
func (m *Manager) admitir() error {
	if err := m.checkDisk(); err != nil {
		return err
	}
	return m.admitirMemoria()
}

// admitirMemoria es la admisión sin la del disco: la de quien no crea disco
// nuevo (Thaw).
func (m *Manager) admitirMemoria() error {
	if err := m.checkPresionPlataforma(); err != nil {
		return err
	}
	return checkPressure()
}
