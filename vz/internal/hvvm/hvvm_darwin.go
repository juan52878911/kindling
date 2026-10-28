//go:build darwin && arm64

package hvvm

/*
#cgo CFLAGS: -O2 -Wall -Wextra -Wno-unused-parameter -mmacosx-version-min=15.2
#cgo LDFLAGS: -framework Hypervisor
#include <stdlib.h>
#include "khv.h"
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
	"sync"
	"syscall"
	"time"
	"unsafe"

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

func errC(op string) error {
	return fmt.Errorf("%s: %s", op, C.GoString(C.khv_error()))
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
		err = errors.New(C.GoString(C.khv_error()))
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
		return errors.New("the VM has stopped")
	}
	return nil
}

func (m *machine) Resume() error {
	if C.khv_resume(C.KHV_PAUSA_API) != 0 {
		return errors.New("the VM has stopped")
	}
	return nil
}

func (m *machine) Stop() error {
	C.khv_stop()
	return nil
}

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

func (m *machine) Stopped() <-chan error { return m.stopped }

// Footprint es la memoria que ocupa este proceso en el host (phys_footprint):
// aquí vive la VM entera, no hay proceso auxiliar como en vz.
func Footprint() (uint64, error) {
	b := uint64(C.khv_footprint())
	if b == 0 {
		return 0, errors.New("task_info failed")
	}
	return b, nil
}

// CPUTime es la CPU que llevan gastada los hilos de vCPU.
func CPUTime() (time.Duration, error) {
	return time.Duration(C.khv_cpu_ns()), nil
}

// Freeze para (o reanuda) los vCPU para el regulador de CPU: lo que en vz es
// SIGSTOP al auxiliar. El reloj del invitado sigue corriendo, como bajo
// cpu.max; es independiente de la pausa del núcleo (PATCH /vm).
func Freeze(stop bool) error {
	var r C.int
	if stop {
		r = C.khv_pause(C.KHV_PAUSA_FRENO)
	} else {
		r = C.khv_resume(C.KHV_PAUSA_FRENO)
	}
	if r != 0 {
		return errors.New("the VM has stopped")
	}
	return nil
}
