// Package spec guarda la configuración de una microVM tal y como la manda el
// núcleo por el API de Firecracker, y la serializa como el JSON del snapshot.
//
// Es la única fuente de verdad para crear la VM: tanto InstanceStart como la
// restauración de un snapshot construyen el dispositivo a partir de un Spec. Por
// eso el JSON del snapshot es este mismo tipo: el framework exige, para
// restaurar, una configuración idéntica a la que se guardó, y la forma más
// barata de garantizarlo es no tener dos caminos que la construyan.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FormatVersion es el valor de "kling_vz" en el JSON del snapshot. Si cambia el
// formato de forma incompatible, se sube y los snapshots viejos se rechazan con
// un error claro en vez de restaurar una máquina distinta de la guardada.
const FormatVersion = 1

// maxSnapshotJSON acota lo que se lee del fichero de snapshot. El fichero viene
// de disco y lo escribe este mismo programa, pero un fichero corrupto o ajeno no
// debe poder hacer que el ayudante reserve memoria sin límite.
const maxSnapshotJSON = 1 << 20

type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type MachineConfig struct {
	VCPUCount  int `json:"vcpu_count"`
	MemSizeMiB int `json:"mem_size_mib"`
}

type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name,omitempty"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

type MMDSConfig struct {
	Version           string   `json:"version,omitempty"`
	IPv4Address       string   `json:"ipv4_address,omitempty"`
	NetworkInterfaces []string `json:"network_interfaces,omitempty"`
}

type Balloon struct {
	AmountMiB             int  `json:"amount_mib"`
	DeflateOnOOM          bool `json:"deflate_on_oom"`
	StatsPollingIntervalS int  `json:"stats_polling_interval_s"`
}

// Spec es todo lo necesario para crear (o recrear) la VM.
type Spec struct {
	KlingVZ           int               `json:"kling_vz"`
	BootSource        *BootSource       `json:"boot_source"`
	MachineConfig     *MachineConfig    `json:"machine_config"`
	Drives            []Drive           `json:"drives"`
	Network           *NetworkInterface `json:"network"`
	MMDSConfig        *MMDSConfig       `json:"mmds_config"`
	Balloon           *Balloon          `json:"balloon"`
	Entropy           bool              `json:"entropy"`
	MachineIdentifier string            `json:"machine_identifier,omitempty"`
}

// Clone devuelve una copia profunda: el servidor guarda el Spec vivo y lo
// muta con los PATCH, y lo que se escribe en un snapshot no debe compartir
// memoria con él.
func (s *Spec) Clone() *Spec {
	b, _ := json.Marshal(s)
	var out Spec
	_ = json.Unmarshal(b, &out)
	return &out
}

// PutDrive añade o sustituye un drive. Sustituir conserva la POSICIÓN: el orden
// de las peticiones es el orden de las letras de disco (vda, vdb...), y
// reconfigurar un drive no debe mover a los demás.
func (s *Spec) PutDrive(d Drive) {
	for i := range s.Drives {
		if s.Drives[i].DriveID == d.DriveID {
			s.Drives[i] = d
			return
		}
	}
	s.Drives = append(s.Drives, d)
}

// PatchDrivePath cambia la ruta registrada de un drive existente.
func (s *Spec) PatchDrivePath(id, path string) error {
	for i := range s.Drives {
		if s.Drives[i].DriveID == id {
			s.Drives[i].PathOnHost = path
			return nil
		}
	}
	return fmt.Errorf("drive %q is not configured", id)
}

// Validate comprueba que haya lo mínimo para crear la VM.
func (s *Spec) Validate() error {
	if s.BootSource == nil || s.BootSource.KernelImagePath == "" {
		return errors.New("boot source is not configured")
	}
	if s.MachineConfig == nil || s.MachineConfig.VCPUCount <= 0 || s.MachineConfig.MemSizeMiB <= 0 {
		return errors.New("machine config is not configured")
	}
	if len(s.Drives) == 0 {
		return errors.New("no drives configured")
	}
	if s.Balloon != nil && s.Balloon.AmountMiB >= s.MachineConfig.MemSizeMiB {
		return fmt.Errorf("balloon amount_mib (%d) must be below mem_size_mib (%d)",
			s.Balloon.AmountMiB, s.MachineConfig.MemSizeMiB)
	}
	return nil
}

// BalloonTargetMiB traduce lo INFLADO (semántica de Firecracker) al objetivo de
// memoria del invitado (semántica de Virtualization.framework).
func (s *Spec) BalloonTargetMiB() int {
	if s.MachineConfig == nil {
		return 0
	}
	amount := 0
	if s.Balloon != nil {
		amount = s.Balloon.AmountMiB
	}
	t := s.MachineConfig.MemSizeMiB - amount
	if t < 1 {
		t = 1
	}
	return t
}

// TranslateBootArgs adapta la línea de comandos de Firecracker al hardware de
// Virtualization.framework. Firecracker expone un 8250 (ttyS0) y no tiene PCI;
// en vz la consola es virtio-console (hvc0) y los virtio van por PCI, así que
// pci=off dejaría al invitado sin discos. Todo lo demás (ip=, kling.*, root=)
// pasa intacto: el invitado debe ver lo mismo que en Linux.
func TranslateBootArgs(args string) string {
	fields := strings.Fields(args)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		switch {
		case f == "pci=off":
			continue
		case f == "console=ttyS0" || strings.HasPrefix(f, "console=ttyS0,"):
			out = append(out, "console=hvc0")
		default:
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// WriteFile escribe el JSON del snapshot de forma atómica (temporal + rename):
// un snapshot a medio escribir que se lea después restauraría con otra
// configuración o no restauraría, y el núcleo no tendría forma de saberlo.
func (s *Spec) WriteFile(path string) error {
	out := s.Clone()
	out.KlingVZ = FormatVersion
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kling-vz-snap-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ReadFile lee y valida el JSON de un snapshot.
func ReadFile(path string) (*Spec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Decode(f)
}

// Decode lee un snapshot de r. Se rechaza lo que no sea de kling-vz: un
// snap.file de Firecracker tiene otro formato, y restaurarlo aquí daría errores
// crípticos del framework en vez de uno que diga qué pasa.
func Decode(r io.Reader) (*Spec, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxSnapshotJSON+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSnapshotJSON {
		return nil, errors.New("snapshot file is too large to be a kling-vz snapshot")
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("snapshot file is not a kling-vz snapshot: %w", err)
	}
	if s.KlingVZ != FormatVersion {
		return nil, fmt.Errorf("unsupported snapshot format kling_vz=%d (this build reads %d)", s.KlingVZ, FormatVersion)
	}
	if s.MachineIdentifier == "" {
		return nil, errors.New("snapshot has no machine_identifier: the framework cannot restore it")
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("snapshot is incomplete: %w", err)
	}
	return &s, nil
}
