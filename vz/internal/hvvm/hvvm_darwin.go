//go:build darwin && arm64

package hvvm

/*
#cgo CFLAGS: -O2 -Wall -Wextra -Wno-unused-parameter -mmacosx-version-min=15.2
#cgo LDFLAGS: -framework Hypervisor
#include <stdlib.h>
#include "khv.h"

// De devices.c y net.c (khv.h es de vm.c).
uint64_t uart_perdidos(void);
void vnet_parar(void);
*/
import "C"

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/juan52878911/kindling/vz/internal/footprint"
	"github.com/juan52878911/kindling/vz/internal/server"
	"github.com/juan52878911/kindling/vz/internal/spec"
)

// guestMAC es la MAC por defecto (knet.GuestMAC), la que manda el núcleo.
var guestMAC = net.HardwareAddr{0x06, 0x00, 0xac, 0x10, 0x00, 0x02}

// Factory crea LA VM de este proceso: Hypervisor.framework solo deja una por
// proceso, y el C guarda su estado en globales.
type Factory struct {
	// Console recibe la consola del invitado (ttyS0) y los diagnósticos del C.
	Console io.Writer
	Logf    func(format string, args ...any)
	// RAMDir es dónde se crea (y se borra en seguida) el fichero disperso que
	// respalda la RAM de un arranque en frío: el directorio de la máquina.
	RAMDir string
	// Inservible se llama cuando este proceso ya no puede alojar una VM: un
	// Create, un arranque o una restauración que fallaron después de crear la
	// VM de Hypervisor.framework, que es una por proceso y no se puede
	// rehacer. El servidor, como con kling-vz, intentaría otra en el mismo
	// proceso; kling-hv tiene que salir para que el núcleo lance uno nuevo.
	Inservible func(error)

	mu     sync.Mutex
	creada bool
}

// NewMachineID: kling-hv no lo necesita (Virtualization.framework sí), pero el
// snapshot lo lleva para que el JSON sea el mismo que el de kling-vz.
func (f *Factory) NewMachineID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// ultimoError es khv_error copiado con su cerrojo: lo escriben también los
// hilos de vCPU (un fallo que termina la VM) mientras Go lo lee.
func ultimoError() string {
	var b [512]C.char
	C.khv_error_copia(&b[0], C.size_t(len(b)))
	return C.GoString(&b[0])
}

func errC(op string) error {
	return fmt.Errorf("%s: %s", op, ultimoError())
}

// FinMotivo dice por qué terminó la VM de este proceso, en una línea para el
// log ("" mientras sigue): el invitado se apagó o se reinició, la paró el
// host, o el fallo que la terminó (un vCPU que salió con una excepción que
// no se atiende, un remapeo de la RAM imposible...), con el texto que dio
// vm.c al terminarla y no el último de khv_error, que el siguiente fallo
// pisa. Vale desde cualquier goroutine y en cualquier momento.
func FinMotivo() string {
	var b [256]C.char
	m := C.khv_fin(&b[0], C.size_t(len(b)))
	texto := C.GoString(&b[0])
	switch m {
	case C.KHV_FIN_APAGADO:
		return "the guest powered off"
	case C.KHV_FIN_REINICIO:
		return "the guest rebooted (SYSTEM_RESET)"
	case C.KHV_FIN_PARADA:
		return "stopped by the host"
	case C.KHV_FIN_FALLO:
		if texto == "" {
			texto = ultimoError()
		}
		return "failed: " + texto
	}
	return ""
}

// errPausa es el error de un khv_pause o khv_resume que devolvió -1: si la
// VM ya terminó, eso y por qué (FinMotivo); si no, el texto de vm.c (un vCPU
// que no llegó a pausarse a tiempo, por ejemplo), y la VM sigue corriendo.
// Siempre con texto: el servidor lo registra tal cual.
func errPausa(op string) error {
	if por := FinMotivo(); por != "" {
		return fmt.Errorf("%s: the VM has stopped: %s", op, por)
	}
	if e := ultimoError(); e != "" {
		return fmt.Errorf("%s: %s", op, e)
	}
	return fmt.Errorf("%s failed", op)
}

// Create configura la VM: el GIC y los dispositivos, sin RAM todavía (la pone
// Start, o RestoreState con el fichero del snapshot). La línea de comandos es
// la de Firecracker tal cual (consola ttyS0, sin PCI): kling-hv tiene el mismo
// hardware, así que el bootArgs traducido para vz no sirve.
func (f *Factory) Create(s *spec.Spec, _ string, n server.Network) (server.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creada {
		err := errors.New("kling-hv creates a single VM per process")
		f.inservible(err)
		return nil, err
	}
	if s.BootSource == nil || s.MachineConfig == nil {
		return nil, errors.New("boot source and machine config are required")
	}
	if len(s.Drives) > int(C.KHV_MAX_DISKS) {
		return nil, fmt.Errorf("kling-hv supports at most %d drives", int(C.KHV_MAX_DISKS))
	}
	if s.Graphics != nil {
		return nil, errors.New("kling-hv has no display device")
	}

	// La consola va por una tubería: el C escribe líneas enteras y aquí se
	// copian a Console, que las serializa con los diagnósticos.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	go func() {
		_, _ = io.Copy(f.Console, pr)
	}()

	var cfg C.struct_khv_config
	cfg.ncpus = C.int(s.MachineConfig.VCPUCount)
	cfg.mem_size = C.uint64_t(uint64(s.MachineConfig.MemSizeMiB) << 20)
	var libres []unsafe.Pointer
	cstr := func(v string) *C.char {
		p := C.CString(v)
		libres = append(libres, unsafe.Pointer(p))
		return p
	}
	defer func() {
		for _, p := range libres {
			C.free(p)
		}
	}()
	cfg.kernel = cstr(s.BootSource.KernelImagePath)
	args := s.BootSource.BootArgs
	if s.Vsock {
		// El mismo parámetro que añade el servidor a la línea traducida de vz:
		// el agente escucha entonces también por AF_VSOCK.
		args += " " + server.VsockBootParam
	}
	cfg.cmdline = cstr(args)
	cfg.ram_dir = cstr(f.RAMDir)
	cfg.ndisks = C.int(len(s.Drives))
	for i, d := range s.Drives {
		cfg.disks[i].path = cstr(d.PathOnHost)
		if d.IsReadOnly {
			cfg.disks[i].ro = 1
		}
		// Qué hace un FLUSH del invitado (spec.Drive.SyncMode): el overlay
		// desechable (kling_disposable) va sin fsync, como el
		// synchronizationMode none de vz; un volumen, lo que pidió.
		switch d.SyncMode() {
		case spec.SyncModeBarrier:
			cfg.disks[i].sinc = C.KHV_SINC_BARRIER
		case spec.SyncModeNone:
			cfg.disks[i].sinc = C.KHV_SINC_NONE
		default:
			cfg.disks[i].sinc = C.KHV_SINC_FULL
		}
	}
	cfg.net_fd = -1
	mac := guestMAC
	var netFile *os.File
	if s.Network != nil && n != nil {
		netFile = n.VMFile()
		if netFile == nil {
			return nil, errors.New("the network has no VM socket")
		}
		cfg.net_fd = C.int(netFile.Fd())
		if s.Network.GuestMAC != "" {
			m, err := net.ParseMAC(s.Network.GuestMAC)
			if err != nil || len(m) != 6 {
				return nil, fmt.Errorf("guest_mac %q is not a MAC address", s.Network.GuestMAC)
			}
			mac = m
		}
		// La MTU del enlace (la del Spec, que es la del snapshot al
		// restaurar): se anuncia al invitado, como hace vz con el adjunto.
		cfg.mtu = C.int(s.MTU)
	}
	for i := 0; i < 6; i++ {
		cfg.mac[i] = C.uint8_t(mac[i])
	}
	if s.Entropy {
		cfg.rng = 1
	}
	if s.Vsock {
		cfg.vsock = 1
	}
	if s.Balloon != nil {
		cfg.balloon = 1
		if s.Balloon.DeflateOnOOM {
			cfg.balloon = 2
		}
	}
	cfg.console_fd = C.int(pw.Fd())

	if C.khv_create(&cfg) != 0 {
		err := errC("creating the VM")
		if C.khv_vm_tocada() {
			f.creada = true
			f.inservible(err)
		}
		return nil, err
	}
	f.creada = true
	m := &machine{f: f, stopped: make(chan error, 1), consola: pw, net: netFile, vsock: s.Vsock,
		memMiB: s.MachineConfig.MemSizeMiB}
	go m.esperar()
	return m, nil
}

func (f *Factory) inservible(err error) {
	if f.Inservible != nil {
		f.Inservible(err)
	}
}

type machine struct {
	f       *Factory
	stopped chan error
	consola *os.File // extremo de escritura de la tubería de consola
	net     *os.File // se guarda para que no lo cierre el recolector
	vsock   bool
	memMiB  int
}

func (m *machine) esperar() {
	code := C.khv_wait()
	var err error
	if code != 0 {
		err = errors.New(strings.TrimPrefix(FinMotivo(), "failed: "))
	}
	// La VM ya no corre: el hilo de recepción de la red sobra (ver Stop).
	C.vnet_parar()
	if n := ConsoleDropped(); n > 0 && m.f.Logf != nil {
		m.f.Logf("the console reader stalled: %d bytes of guest console were dropped", n)
	}
	m.stopped <- err
}

func (m *machine) Start() error {
	if C.khv_boot() != 0 {
		err := errC("booting")
		m.f.inservible(err)
		return err
	}
	return nil
}

func (m *machine) Pause() error {
	if C.khv_pause(C.KHV_PAUSA_API) != 0 {
		return errPausa("pausing")
	}
	return nil
}

func (m *machine) Resume() error {
	if C.khv_resume(C.KHV_PAUSA_API) != 0 {
		return errPausa("resuming")
	}
	return nil
}

func (m *machine) Stop() error {
	C.khv_stop()
	// El servidor cierra la red justo después (Server.Shutdown): antes, el
	// hilo de recepción del C tiene que haber soltado su fd.
	C.vnet_parar()
	return nil
}

// ConsoleDropped son los bytes de la consola del invitado que se tiraron
// porque su lector no daba abasto: la UART no bloquea nunca al vCPU (ver
// devices.c).
func ConsoleDropped() uint64 { return uint64(C.uart_perdidos()) }

// SaveState escribe el fichero de memoria del snapshot (RAM + estado) en path.
func (m *machine) SaveState(path string) error {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	if C.khv_snapshot(p) != 0 {
		return errC("snapshot")
	}
	return nil
}

// RestoreState mapea el fichero de memoria de un snapshot como RAM (sin
// copiarlo) y carga el estado; la VM queda en pausa hasta Resume.
func (m *machine) RestoreState(path string) error {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	if C.khv_restore(p) != 0 {
		err := errC("restoring")
		m.f.inservible(err)
		return err
	}
	return nil
}

// DialVsock abre una conexión virtio-vsock al puerto port del invitado (ver
// vsock.c): el canal con el agente, como en kling-vz. Cada llamada es una
// conexión nueva; tras restaurar, el dispositivo manda TRANSPORT_RESET y las
// que el invitado creía vivas mueren, pero su listener sigue.
func (m *machine) DialVsock(ctx context.Context, port uint32) (net.Conn, error) {
	if !m.vsock {
		return nil, errors.New("the VM has no vsock device")
	}
	plazo := 2 * time.Second
	if d, ok := ctx.Deadline(); ok {
		plazo = time.Until(d)
	}
	if plazo <= 0 {
		return nil, context.DeadlineExceeded
	}
	ms := plazo.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	fd := int(C.khv_vsock_dial(C.uint32_t(port), C.int(ms)))
	if fd < 0 {
		err := syscall.Errno(-fd)
		if err == syscall.ETIMEDOUT && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("vsock connect to port %d: %w", port, err)
	}
	f := os.NewFile(uintptr(fd), "vsock")
	defer f.Close()
	return net.FileConn(f)
}

// SetBalloonTargetMiB deja al invitado con mib MiB: el globo (balloon.c) se
// infla hasta la diferencia con su RAM, como en kling-vz.
func (m *machine) SetBalloonTargetMiB(mib int) error {
	if mib < 0 || mib > m.memMiB {
		mib = m.memMiB
	}
	if C.khv_balloon(C.uint32_t((m.memMiB-mib)*256)) != 0 {
		return errors.New("the VM has no balloon device")
	}
	return nil
}

// BalloonStats son las estadísticas de memoria que da el driver del globo
// del invitado (VIRTIO_BALLOON_F_STATS_VQ), en bytes, y el globo actual en
// MiB; ok=false si la VM no tiene globo, está en pausa o el driver no
// negoció la cola (un invitado restaurado de antes de que existiera). Es lo
// que sirve GET /balloon/statistics (server.balloonStatser).
func (m *machine) BalloonStats() (free, available, total uint64, actualMiB int, ok bool) {
	var v [C.KHV_BALLOON_NSTATS]C.uint64_t
	var actual C.uint32_t
	// Las últimas que dio el driver, pidiéndole otras para la próxima vez;
	// si tienen más de un segundo (o no hay, recién restaurado) se esperan
	// hasta 100 ms.
	if C.khv_balloon_stats(&v[0], &actual, 100) != 0 {
		return 0, 0, 0, 0, false
	}
	// Etiquetas de virtio-balloon: 4 MEMFREE, 5 MEMTOT, 6 AVAIL.
	return uint64(v[4]), uint64(v[6]), uint64(v[5]), int(actual) / 256, true
}

func (m *machine) Stopped() <-chan error { return m.stopped }

// BalloonCounters son las cuentas del globo de esta VM: páginas del host (16
// KiB) que tiene soltadas ahora, las devueltas por el informe de páginas
// libres (acumulado), los tramos soltados, lo que costaron y los que fallaron
// (sin terminar la VM: las páginas se quedan sucias).
type BalloonCounters struct {
	ReleasedPages, ReportedPages, Runs, Failed uint64
	Spent                                      time.Duration
}

// Balloon da las cuentas del globo (balloon.c, khv_balloon_cuentas).
func Balloon() BalloonCounters {
	var v [C.KHV_BALLOON_NCUENTAS]C.uint64_t
	C.khv_balloon_cuentas(&v[0])
	return BalloonCounters{ReleasedPages: uint64(v[0]), ReportedPages: uint64(v[1]), Runs: uint64(v[2]),
		Spent: time.Duration(v[3]), Failed: uint64(v[4])}
}

// EndReason dice por qué terminó la VM (FinMotivo): "" mientras corre; tras
// recibir de Stopped, siempre el motivo.
func (m *machine) EndReason() string { return FinMotivo() }

// VersionVolcado es la versión del estado que escribe (y la más nueva que
// lee) este kling-hv en el pie de mem.file: la cabecera KLNGHV02 de vm.c.
const VersionVolcado = C.KHV_SNAP_VERSION

// MaxVCPUs es el tope de vCPU de una máquina de kling-hv en este Mac: lo que
// deja Hypervisor.framework (64 en el M4, y MAX_CPUS de khv.h como mucho) sin
// pasar de los núcleos del host. Más vCPU que núcleos no corre más y deja al
// invitado esperando a hilos que el host no puede atender a la vez (un
// spinlock del kernel invitado gira mientras su dueño no tiene núcleo).
func MaxVCPUs() int {
	return min(int(C.khv_max_vcpus()), runtime.NumCPU())
}

// Footprint es la memoria que ocupa este proceso en el host (phys_footprint):
// aquí vive la VM entera, no hay proceso auxiliar como en vz.
func Footprint() (uint64, error) {
	b := uint64(C.khv_footprint())
	if b == 0 {
		return 0, errors.New("task_info failed")
	}
	return b, nil
}

// Avisos son los contadores de avisos del invitado (notify) e interrupciones
// hacia él de la red y los discos, acumulados desde que arrancó el proceso:
// tx, rx, irq de red, notify de disco, irq de disco.
func Avisos() [5]uint64 {
	var v [C.KHV_N_MAX]C.uint64_t
	C.khv_contadores(&v[0])
	var out [5]uint64
	for i := range out {
		out[i] = uint64(v[i])
	}
	return out
}

// cpuMu serializa las lecturas (el C decide su fuente en la primera) y
// cpuEquiv lleva la cuenta en tiempo de núcleo P.
var (
	cpuMu    sync.Mutex
	cpuEquiv footprint.Contador
)

// CPUTime es la CPU que llevan gastada los hilos de vCPU, con el tiempo en
// núcleos de eficiencia cobrado a lo que vale en P, como el de kling-vz
// (footprint/coste.go): sin eso, un vCPU que el planificador manda a los E
// gasta el techo de CPU varias veces más deprisa por el mismo trabajo.
func CPUTime() (time.Duration, error) {
	cpuMu.Lock()
	defer cpuMu.Unlock()
	var v [4]C.uint64_t
	C.khv_cpu_lectura(&v[0])
	return cpuEquiv.Suma(time.Duration(v[0]), time.Duration(v[1]), uint64(v[2]), uint64(v[3])), nil
}

// Freeze para (o reanuda) los vCPU para el regulador de CPU: lo que en vz es
// SIGSTOP al auxiliar. El reloj del invitado sigue corriendo, como bajo
// cpu.max; es independiente de la pausa del núcleo (PATCH /vm), que sí lo
// para: tras ella el invitado sigue donde estaba y su reloj de pared queda
// atrasado lo que duró (lo arregla el /resync del agente).
func Freeze(stop bool) error {
	var r C.int
	if stop {
		r = C.khv_pause(C.KHV_PAUSA_FRENO)
	} else {
		r = C.khv_resume(C.KHV_PAUSA_FRENO)
	}
	if r != 0 {
		if stop {
			return errPausa("throttling (stopping the vCPUs)")
		}
		return errPausa("throttling (releasing the vCPUs)")
	}
	return nil
}
