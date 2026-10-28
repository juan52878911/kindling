package server

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/vz/internal/spec"
)

// En el doble, toda VM sabe abrir vsock: DialVsock apunta el puerto.
func (v *fakeVM) DialVsock(_ context.Context, port uint32) (net.Conn, error) {
	v.mu.Lock()
	v.vsock = append(v.vsock, port)
	v.mu.Unlock()
	return nil, errors.New("fake vsock")
}

func (n *fakeNet) SetGuestDialer(port int, dial func(ctx context.Context) (net.Conn, error), soloTCP bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.dial == nil {
		n.dial = map[int]func(ctx context.Context) (net.Conn, error){}
	}
	n.dial[port] = dial
	n.soloTCP = soloTCP
}

func (n *fakeNet) GuestOnlyTCP(int) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.soloTCP
}

func (n *fakeNet) dialer(port int) func(ctx context.Context) (net.Conn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dial[port]
}

// Un arranque con Deps.Vsock y Deps.MTU: el Spec los lleva (viajarán en el
// snapshot), la red se monta con esa MTU, el invitado recibe kling.vsock=1 y
// el puerto del agente se lleva por el vsock de la VM.
func TestArranqueConVsockYMTU(t *testing.T) {
	r := newRig(t)
	r.srv.d.Vsock, r.srv.d.MTU = true, 16384
	dir := t.TempDir()
	r.configure(dir)
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)

	vm, s, args := r.f.last()
	if !s.Vsock || s.MTU != 16384 {
		t.Fatalf("spec sin vsock o MTU: vsock=%v mtu=%d", s.Vsock, s.MTU)
	}
	if !strings.HasSuffix(args, " "+VsockBootParam) {
		t.Fatalf("boot args sin %s: %q", VsockBootParam, args)
	}
	n := r.nets[0]
	if n.cfg.MTU != 16384 {
		t.Fatalf("la red se montó con MTU %d", n.cfg.MTU)
	}
	d := n.dialer(api.GuestPort)
	if d == nil {
		t.Fatal("el puerto del agente no va por vsock")
	}
	_, _ = d(context.Background())
	vm.mu.Lock()
	got := vm.vsock
	vm.mu.Unlock()
	if len(got) != 1 || got[0] != uint32(api.GuestPort) {
		t.Fatalf("DialVsock a %v, quiero [%d]", got, api.GuestPort)
	}

	// El snapshot los guarda, con el formato que un kling-vz anterior rechaza.
	r.must("PATCH", "/vm", `{"state":"Paused"}`)
	snap := filepath.Join(dir, "snap.file")
	r.must("PUT", "/snapshot/create", `{"snapshot_type":"Full","snapshot_path":"`+snap+`","mem_file_path":"`+filepath.Join(dir, "mem")+`"}`)
	b, _ := os.ReadFile(snap)
	for _, w := range []string{`"kling_vz": 3`, `"vsock": true`, `"mtu": 16384`} {
		if !strings.Contains(string(b), w) {
			t.Fatalf("el snapshot no lleva %s:\n%s", w, b)
		}
	}
}

// Un snapshot de antes (sin vsock ni MTU) se restaura como se guardó aunque
// este kling-vz arranque las máquinas nuevas con ellos: el framework exige los
// mismos dispositivos, y su agente solo escucha por TCP.
func TestRestaurarSnapshotSinVsock(t *testing.T) {
	r := newRig(t)
	r.srv.d.Vsock, r.srv.d.MTU = true, 16384
	snap, mem := writeSnapshot(t, t.TempDir())
	r.must("PUT", "/snapshot/load", `{"snapshot_path":"`+snap+`","mem_backend":{"backend_path":"`+mem+`","backend_type":"File"},"resume_vm":true}`)
	_, s, args := r.f.last()
	if s.Vsock || s.MTU != 0 || strings.Contains(args, "kling.vsock") {
		t.Fatalf("restaurado con vsock=%v mtu=%d args=%q", s.Vsock, s.MTU, args)
	}
	if r.nets[0].cfg.MTU != 0 || r.nets[0].dialer(api.GuestPort) != nil {
		t.Fatal("la red de un snapshot sin vsock debe quedar como antes")
	}
}

// Y uno con vsock se restaura con él: la conexión de cada reenvío es nueva,
// así que no hay nada que reconectar tras RestoreState.
func TestRestaurarSnapshotConVsock(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	s := &spec.Spec{
		BootSource:        &spec.BootSource{KernelImagePath: "/k", BootArgs: "console=ttyS0"},
		MachineConfig:     &spec.MachineConfig{VCPUCount: 1, MemSizeMiB: 256},
		Network:           &spec.NetworkInterface{IfaceID: "eth0"},
		MachineIdentifier: "aWQ=",
		Drives:            []spec.Drive{{DriveID: "rootfs", PathOnHost: "/i/min.ext4", IsRootDevice: true, IsReadOnly: true}},
		Vsock:             true,
		MTU:               16384,
	}
	snap, mem := filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem.file")
	if err := s.WriteFile(snap); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mem, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.must("PUT", "/snapshot/load", `{"snapshot_path":"`+snap+`","mem_backend":{"backend_path":"`+mem+`","backend_type":"File"},"resume_vm":true}`)
	_, got, _ := r.f.last()
	if !got.Vsock || got.MTU != 16384 || r.nets[0].cfg.MTU != 16384 || r.nets[0].dialer(api.GuestPort) == nil {
		t.Fatalf("restaurado sin lo del snapshot: vsock=%v mtu=%d", got.Vsock, got.MTU)
	}
}

// Lo que el proceso vio del agente (solo TCP) se guarda en el snapshot y el
// que lo restaura lo recibe: no vuelve a pagar el connect de vsock fallido.
func TestSnapshotRecuerdaAgenteSoloTCP(t *testing.T) {
	r := newRig(t)
	r.srv.d.Vsock = true
	dir := t.TempDir()
	r.configure(dir)
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	r.nets[0].mu.Lock()
	r.nets[0].soloTCP = true // lo que haría vnet al ver contestar solo a TCP
	r.nets[0].mu.Unlock()
	r.must("PATCH", "/vm", `{"state":"Paused"}`)
	snap, mem := filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem")
	r.must("PUT", "/snapshot/create", `{"snapshot_type":"Full","snapshot_path":"`+snap+`","mem_file_path":"`+mem+`"}`)
	if b, _ := os.ReadFile(snap); !strings.Contains(string(b), `"agent_tcp_only": true`) {
		t.Fatalf("el snapshot no recuerda el agente solo TCP:\n%s", b)
	}

	r2 := newRig(t)
	r2.must("PUT", "/snapshot/load", `{"snapshot_path":"`+snap+`","mem_backend":{"backend_path":"`+mem+`","backend_type":"File"},"resume_vm":true}`)
	if r2.nets[0].dialer(api.GuestPort) == nil || !r2.nets[0].GuestOnlyTCP(api.GuestPort) {
		t.Fatal("al restaurar no se pasó soloTCP a la red")
	}
}
