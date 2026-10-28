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
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
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
		return nil, errors.New("kling-hv creates a single VM per process")
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
	cfg.cmdline = cstr(s.BootSource.BootArgs)
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
	cfg.console_fd = C.int(pw.Fd())

	if C.khv_create(&cfg) != 0 {
		return nil, errC("creating the VM")
	}
	f.creada = true
	m := &machine{f: f, stopped: make(chan error, 1), consola: pw, net: netFile}
	go m.esperar()
	return m, nil
}

type machine struct {
	f       *Factory
	stopped chan error
	consola *os.File // extremo de escritura de la tubería de consola
	net     *os.File // se guarda para que no lo cierre el recolector
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
		return errC("booting")
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
		return errC("restoring")
	}
	return nil
}

func (m *machine) SetBalloonTargetMiB(int) error {
	return errors.New("kling-hv has no balloon device")
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
