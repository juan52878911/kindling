//go:build darwin

// Package vzvm implementa la VM del servidor con Virtualization.framework
// (Code-Hex/vz). Es la única parte del ayudante que necesita un Mac, cgo y el
// entitlement com.apple.security.virtualization.
package vzvm

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"unsafe"

	"github.com/Code-Hex/vz/v3"

	"github.com/juan52878911/kindling/vz/internal/server"
	"github.com/juan52878911/kindling/vz/internal/spec"
)

// Factory crea VMs. Console recibe lo que el invitado escribe en hvc0.
type Factory struct {
	Console io.Writer
	Logf    func(format string, args ...any)
	// OnCreate recibe el descriptor de la tubería de consola que se entrega al
	// framework: es lo que permite reconocer después el proceso auxiliar de
	// Apple que aloja a esta VM (ver footprint).
	OnCreate func(consoleFD uintptr)
	// Window: enseñar la pantalla (si la máquina tiene) en una ventana; ver
	// WindowMode. Title es el título de la ventana.
	Window bool
	Title  string
}

func (f *Factory) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
	}
}

// NewMachineID genera la identidad de la máquina. El framework la exige igual
// al restaurar ("invalid argument" si cambia), por eso viaja en el snapshot.
func (f *Factory) NewMachineID() (string, error) {
	id, err := vz.NewGenericMachineIdentifier()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(id.DataRepresentation()), nil
}

func (f *Factory) Create(s *spec.Spec, bootArgs string, n server.Network) (server.VM, error) {
	boot, err := vz.NewLinuxBootLoader(s.BootSource.KernelImagePath, vz.WithCommandLine(bootArgs))
	if err != nil {
		return nil, fmt.Errorf("kernel %s: %w", s.BootSource.KernelImagePath, err)
	}
	cfg, err := vz.NewVirtualMachineConfiguration(boot, uint(s.MachineConfig.VCPUCount), uint64(s.MachineConfig.MemSizeMiB)<<20)
	if err != nil {
		return nil, err
	}

	raw, err := base64.StdEncoding.DecodeString(s.MachineIdentifier)
	if err != nil {
		return nil, fmt.Errorf("machine identifier: %w", err)
	}
	mid, err := vz.NewGenericMachineIdentifierWithData(raw)
	if err != nil {
		return nil, fmt.Errorf("machine identifier: %w", err)
	}
	platform, err := vz.NewGenericPlatformConfiguration(vz.WithGenericMachineIdentifier(mid))
	if err != nil {
		return nil, err
	}
	cfg.SetPlatformVirtualMachineConfiguration(platform)

	m := &machine{stopped: make(chan error, 1), fin: make(chan struct{}), logf: f.Logf}
	ok := false
	defer func() {
		if !ok {
			// La copia de la consola no llegó a arrancar: el extremo de
			// lectura también es nuestro.
			m.closeConsole()
			if m.outR != nil {
				m.outR.Close()
			}
		}
	}()

	// Consola: la entrada es una tubería que nadie escribe (el núcleo no manda
	// nada por la consola) pero que se mantiene abierta, porque un EOF en la
	// entrada del hvc0 no está definido para el framework. La salida se copia a
	// Console en vez de dársela tal cual para no partir las líneas de
	// diagnóstico del propio ayudante.
	if m.inR, m.inW, err = os.Pipe(); err != nil {
		return nil, err
	}
	if m.outR, m.outW, err = os.Pipe(); err != nil {
		return nil, err
	}
	serial, err := vz.NewFileHandleSerialPortAttachment(m.inR, m.outW)
	if err != nil {
		return nil, err
	}
	console, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serial)
	if err != nil {
		return nil, err
	}
	cfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{console})

	// Un virtio-blk por drive en el orden del Spec: vda, vdb... como Firecracker.
	var disks []vz.StorageDeviceConfiguration
	for _, d := range s.Drives {
		att, err := adjuntoDisco(d)
		if err != nil {
			return nil, fmt.Errorf("drive %s (%s): %w", d.DriveID, d.PathOnHost, err)
		}
		dev, err := vz.NewVirtioBlockDeviceConfiguration(att)
		if err != nil {
			return nil, err
		}
		disks = append(disks, dev)
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration(disks)

	if s.Network != nil {
		if n == nil {
			return nil, errors.New("a network interface is configured but the network is not up")
		}
		att, err := vz.NewFileHandleNetworkDeviceAttachment(n.VMFile())
		if err != nil {
			return nil, fmt.Errorf("network attachment: %w", err)
		}
		// MTU del enlace (macOS 13+): el framework la anuncia al invitado
		// (VIRTIO_NET_F_MTU) y el virtio_net de Linux la adopta solo. La pila
		// de usuario se montó con la misma (vnet.Config.MTU): las dos salen
		// del Spec, que viaja en el snapshot.
		if s.MTU > 0 {
			if err := att.SetMaximumTransmissionUnit(s.MTU); err != nil {
				return nil, fmt.Errorf("network MTU %d: %w", s.MTU, err)
			}
		}
		nic, err := vz.NewVirtioNetworkDeviceConfiguration(att)
		if err != nil {
			return nil, err
		}
		macStr := s.Network.GuestMAC
		if macStr == "" {
			macStr = "06:00:AC:10:00:02"
		}
		hw, err := net.ParseMAC(macStr)
		if err != nil {
			return nil, fmt.Errorf("guest_mac: %w", err)
		}
		mac, err := vz.NewMACAddress(hw)
		if err != nil {
			return nil, err
		}
		nic.SetMACAddress(mac)
		cfg.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{nic})
	}

	if s.Balloon != nil {
		b, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
		if err != nil {
			return nil, err
		}
		cfg.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{b})
	}
	if s.Entropy {
		e, err := vz.NewVirtioEntropyDeviceConfiguration()
		if err != nil {
			return nil, err
		}
		cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{e})
	}

	// Pantalla virtio-gpu (2D): un scanout del tamaño pedido. Sin teclado ni
	// puntero: vz solo los da como USB para Linux, y el kernel de invitado no
	// lleva USB.
	if g := s.Graphics; g != nil {
		scan, err := vz.NewVirtioGraphicsScanoutConfiguration(int64(g.Width), int64(g.Height))
		if err != nil {
			return nil, fmt.Errorf("graphics scanout: %w", err)
		}
		gpu, err := vz.NewVirtioGraphicsDeviceConfiguration()
		if err != nil {
			return nil, fmt.Errorf("graphics device: %w", err)
		}
		gpu.SetScanouts(scan)
		cfg.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{gpu})
	}

	// virtio-vsock: el canal con el agente del invitado (ver DialVsock). Solo
	// si el Spec lo lleva: un snapshot guardado sin él debe restaurarse sin
	// él, el framework exige los mismos dispositivos.
	if s.Vsock {
		vs, err := vz.NewVirtioSocketDeviceConfiguration()
		if err != nil {
			return nil, fmt.Errorf("vsock device: %w", err)
		}
		cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vs})
	}

	if valid, err := cfg.Validate(); !valid || err != nil {
		return nil, fmt.Errorf("invalid VM configuration: %v", err)
	}
	if valid, err := cfg.ValidateSaveRestoreSupport(); (!valid || err != nil) && f.Logf != nil {
		f.Logf("warning: this configuration does not support save/restore: %v", err)
	}
	vm, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return nil, err
	}
	m.vm = vm
	ok = true
	if s.Graphics != nil && f.Window {
		if m.view = showWindow(vm, s.Graphics.Width, s.Graphics.Height, f.Title); m.view != nil {
			f.logf("graphics %dx%d shown in a window", s.Graphics.Width, s.Graphics.Height)
		} else {
			f.logf("warning: cannot open the window (unknown Code-Hex/vz layout)")
		}
	}
	if f.OnCreate != nil {
		f.OnCreate(m.outW.Fd())
	}

	go func() {
		// Lee hasta el fin de fichero (el auxiliar y signal cierran sus
		// extremos de escritura) y suelta el descriptor: antes se quedaba
		// abierto para siempre, uno por VM creada.
		defer m.outR.Close()
		if f.Console != nil {
			_, _ = io.Copy(f.Console, m.outR)
		} else {
			_, _ = io.Copy(io.Discard, m.outR)
		}
	}()
	go m.watch()
	return m, nil
}

type machine struct {
	vm                   *vz.VirtualMachine
	inR, inW, outR, outW *os.File
	stopped              chan error
	// fin se cierra en signal: watch deja de esperar cambios de estado de
	// una VM que ya no los va a tener (p. ej. la de un Start fallido).
	fin  chan struct{}
	once sync.Once
	logf func(string, ...any)
	view unsafe.Pointer // VZVirtualMachineView si hay ventana
}

// Screenshot devuelve la ventana de la VM como PNG (ver capture).
func (m *machine) Screenshot() ([]byte, error) { return capture(m.view) }

// closeConsole cierra nuestros extremos de las tuberías de consola menos el
// de lectura de la salida, que es de la copia (la gorrutina de Create): ella
// lo cierra al ver el fin de fichero, para no perder la cola de la consola.
func (m *machine) closeConsole() {
	for _, f := range []*os.File{m.inR, m.inW, m.outW} {
		if f != nil {
			f.Close()
		}
	}
}

func (m *machine) watch() {
	cambios := m.vm.StateChangedNotify()
	for {
		select {
		case <-m.fin:
			// Ya se avisó por otro camino (Stop de una VM que nunca
			// arrancó, Start fallido): esta gorrutina era lo único que
			// retenía la VM.
			return
		case st, ok := <-cambios:
			if !ok {
				return
			}
			switch st {
			case vz.VirtualMachineStateStopped:
				m.signal(nil)
				return
			case vz.VirtualMachineStateError:
				m.signal(errors.New("Virtualization.framework reported the VM in error state"))
				return
			}
		}
	}
}

func (m *machine) signal(err error) {
	m.once.Do(func() {
		m.stopped <- err
		close(m.fin)
		// Cerrar la escritura deja que la copia de la consola termine.
		m.closeConsole()
	})
}

func (m *machine) Stopped() <-chan error { return m.stopped }

// Start arranca la VM. Si falla, la VM no llega a cambiar de estado y watch
// no se enteraría nunca: se avisa aquí (Stopped con el error), lo que suelta
// la gorrutina y la consola. Quien llama la abandona igualmente.
func (m *machine) Start() error {
	err := m.vm.Start()
	if err != nil && !m.vm.CanStop() {
		m.signal(fmt.Errorf("start: %w", err))
	}
	return err
}
func (m *machine) Pause() error  { return m.vm.Pause() }
func (m *machine) Resume() error { return m.vm.Resume() }

func (m *machine) Stop() error {
	if !m.vm.CanStop() {
		// Recién creada y nunca arrancada: no hay nada que parar, pero quien
		// espere en Stopped debe enterarse.
		m.signal(nil)
		return nil
	}
	return m.vm.Stop()
}

func (m *machine) SaveState(path string) error { return m.vm.SaveMachineStateToPath(path) }

func (m *machine) RestoreState(path string) error { return m.vm.RestoreMachineStateFromURL(path) }

func (m *machine) SetBalloonTargetMiB(mib int) error {
	devs := m.vm.MemoryBalloonDevices()
	if len(devs) == 0 {
		return errors.New("the VM has no balloon device")
	}
	b := vz.AsVirtioTraditionalMemoryBalloonDevice(devs[0])
	if b == nil {
		return errors.New("the balloon device is not a virtio traditional balloon")
	}
	b.SetTargetVirtualMachineMemorySize(uint64(mib) << 20)
	return nil
}

// adjuntoDisco crea el adjunto de un disco con sus modos (modosDisco).
func adjuntoDisco(d spec.Drive) (*vz.DiskImageStorageDeviceAttachment, error) {
	cm, sm, def, err := modosDisco(d, os.Getenv("KLING_VZ_DISK_CACHING"), os.Getenv("KLING_VZ_DISK_SYNC"))
	if err != nil {
		return nil, err
	}
	if def {
		return vz.NewDiskImageStorageDeviceAttachment(d.PathOnHost, d.IsReadOnly)
	}
	return vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(d.PathOnHost, d.IsReadOnly, cm, sm)
}

// modosDisco elige la caché y la sincronización de un disco:
//
//   - De solo lectura (la base, las capas, el dorado): caché "cached". Nadie
//     escribe en ellos y los comparten las máquinas que arrancan de la misma
//     imagen: que el host los guarde en su caché ahorra lecturas a todas.
//   - De escritura: la caché por defecto del framework (automatic).
//   - Sincronización: la del framework (full: cada flush del invitado llega
//     al disco físico) salvo en el overlay desechable de una máquina no
//     persistente (spec.Drive.SyncNone), que va con none. Volúmenes y discos
//     de db nunca vienen marcados.
//
// KLING_VZ_DISK_CACHING=cached|uncached|automatic y
// KLING_VZ_DISK_SYNC=full|fsync|none mandan sobre lo anterior, para todos los
// discos: sirven para comparar modos cuando se investiga una lectura
// corrupta (prototypes/android/docs/sigill.md). def: son los modos por
// defecto del framework (el adjunto de siempre).
func modosDisco(d spec.Drive, envCaching, envSync string) (vz.DiskImageCachingMode, vz.DiskImageSynchronizationMode, bool, error) {
	cm := vz.DiskImageCachingModeAutomatic
	if d.IsReadOnly {
		cm = vz.DiskImageCachingModeCached
	}
	switch envCaching {
	case "":
	case "automatic":
		cm = vz.DiskImageCachingModeAutomatic
	case "cached":
		cm = vz.DiskImageCachingModeCached
	case "uncached":
		cm = vz.DiskImageCachingModeUncached
	default:
		return 0, 0, false, fmt.Errorf("KLING_VZ_DISK_CACHING=%q: use automatic, cached or uncached", envCaching)
	}
	sm := vz.DiskImageSynchronizationModeFull
	if d.SyncNone() {
		sm = vz.DiskImageSynchronizationModeNone
	}
	switch envSync {
	case "":
	case "full":
		sm = vz.DiskImageSynchronizationModeFull
	case "fsync":
		sm = vz.DiskImageSynchronizationModeFsync
	case "none":
		sm = vz.DiskImageSynchronizationModeNone
	default:
		return 0, 0, false, fmt.Errorf("KLING_VZ_DISK_SYNC=%q: use full, fsync or none", envSync)
	}
	def := cm == vz.DiskImageCachingModeAutomatic && sm == vz.DiskImageSynchronizationModeFull
	return cm, sm, def, nil
}
