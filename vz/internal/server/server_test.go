package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/vz/internal/egress"
	"github.com/juan52878911/kindling/vz/internal/spec"
)

// --- dobles ---

type fakeVM struct {
	mu       sync.Mutex
	calls    []string
	balloon  []int
	stopped  chan error
	failNext string // nombre de la operación que debe fallar
}

func (v *fakeVM) rec(op string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, op)
	if v.failNext == op {
		v.failNext = ""
		return errors.New(op + " failed")
	}
	return nil
}

func (v *fakeVM) Start() error  { return v.rec("start") }
func (v *fakeVM) Pause() error  { return v.rec("pause") }
func (v *fakeVM) Resume() error { return v.rec("resume") }
func (v *fakeVM) Stop() error {
	err := v.rec("stop")
	select {
	case v.stopped <- nil:
	default:
	}
	return err
}
func (v *fakeVM) SaveState(path string) error {
	if err := v.rec("save " + path); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return errors.New("the framework refuses to overwrite " + path)
	}
	return os.WriteFile(path, []byte("state"), 0o644)
}
func (v *fakeVM) RestoreState(path string) error { return v.rec("restore " + path) }
func (v *fakeVM) SetBalloonTargetMiB(mib int) error {
	v.mu.Lock()
	v.balloon = append(v.balloon, mib)
	v.mu.Unlock()
	return nil
}
func (v *fakeVM) Stopped() <-chan error { return v.stopped }

func (v *fakeVM) ops() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.calls...)
}

type fakeFactory struct {
	mu       sync.Mutex
	vms      []*fakeVM
	specs    []*spec.Spec
	bootArgs []string
	failNext string
}

func (f *fakeFactory) NewMachineID() (string, error) { return "bWFjaGluZS1pZA==", nil }

func (f *fakeFactory) Create(s *spec.Spec, bootArgs string, n Network) (VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s.Network != nil && n == nil {
		return nil, errors.New("network missing")
	}
	vm := &fakeVM{stopped: make(chan error, 1), failNext: f.failNext}
	f.failNext = ""
	f.vms = append(f.vms, vm)
	f.specs = append(f.specs, s)
	f.bootArgs = append(f.bootArgs, bootArgs)
	return vm, nil
}

func (f *fakeFactory) last() (*fakeVM, *spec.Spec, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := len(f.vms) - 1
	return f.vms[i], f.specs[i], f.bootArgs[i]
}

type fakeNet struct {
	cfg    NetConfig
	mu     sync.Mutex
	fwd    map[int]string
	closed bool
}

func (n *fakeNet) VMFile() *os.File { return nil }

// Probe: en el doble, "escucha" lo que tenga reenvío pedido y sea par.
func (n *fakeNet) Probe(_ context.Context, p int) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.fwd[p]
	return ok && p%2 == 0
}
func (n *fakeNet) Forward(p int) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if p < 1 || p > 65535 {
		return "", fmt.Errorf("invalid port %d", p)
	}
	if a, ok := n.fwd[p]; ok {
		return a, nil
	}
	a := fmt.Sprintf("127.0.0.1:%d", 40000+len(n.fwd))
	n.fwd[p] = a
	return a, nil
}
func (n *fakeNet) Close() { n.closed = true }

type rig struct {
	t    *testing.T
	srv  *Server
	h    http.Handler
	f    *fakeFactory
	nets []*fakeNet
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, f: &fakeFactory{}}
	r.srv = New(Deps{
		Factory: r.f,
		NewNet: func(c NetConfig) (Network, error) {
			n := &fakeNet{cfg: c, fwd: map[int]string{}}
			r.nets = append(r.nets, n)
			return n, nil
		},
		Footprint: func() (uint64, error) { return 150<<20 + 1, nil },
		Version:   "9.9.9",
	})
	// Sin reintentos del globo salvo en la prueba que los mira: si no, una
	// gorutina escribe en fakeVM.balloon mientras la prueba lo lee.
	r.srv.globoEn = nil
	r.h = r.srv.Handler()
	return r
}

func (r *rig) call(method, path, body string) (int, string) {
	r.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (r *rig) must(method, path, body string) string {
	r.t.Helper()
	code, out := r.call(method, path, body)
	if code/100 != 2 {
		r.t.Fatalf("%s %s -> %d %s", method, path, code, out)
	}
	return out
}

func (r *rig) mustFail(method, path, body, contains string) {
	r.t.Helper()
	code, out := r.call(method, path, body)
	if code != http.StatusBadRequest {
		r.t.Fatalf("%s %s -> %d %s, want 400", method, path, code, out)
	}
	var f struct {
		FaultMessage string `json:"fault_message"`
	}
	if err := json.Unmarshal([]byte(out), &f); err != nil || f.FaultMessage == "" {
		r.t.Fatalf("%s %s: error body is not a Firecracker fault: %s", method, path, out)
	}
	if !strings.Contains(f.FaultMessage, contains) {
		r.t.Fatalf("%s %s: fault %q does not mention %q", method, path, f.FaultMessage, contains)
	}
}

// configure repite la secuencia de boot() del núcleo (manager.go).
func (r *rig) configure(dir string) {
	r.must("PUT", "/boot-source", `{"kernel_image_path":"/k/vmlinux","boot_args":"console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off"}`)
	r.must("PUT", "/drives/rootfs", `{"drive_id":"rootfs","path_on_host":"/i/min.ext4","is_root_device":true,"is_read_only":true,"rate_limiter":{"bandwidth":{"size":1,"refill_time":1000}}}`)
	r.must("PUT", "/drives/overlay", `{"drive_id":"overlay","path_on_host":"`+dir+`/overlay.ext4","is_root_device":false,"is_read_only":false}`)
	r.must("PUT", "/drives/layer", `{"drive_id":"layer","path_on_host":"/i/x.layer.ext4","is_read_only":true}`)
	r.must("PUT", "/network-interfaces/eth0", `{"iface_id":"eth0","host_dev_name":"tap0","guest_mac":"06:00:AC:10:00:02"}`)
	r.must("PUT", "/mmds/config", `{"version":"V2","ipv4_address":"169.254.169.254","network_interfaces":["eth0"]}`)
	r.must("PUT", "/entropy", `{}`)
	r.must("PUT", "/machine-config", `{"vcpu_count":1,"mem_size_mib":512}`)
	r.must("PUT", "/balloon", `{"amount_mib":256,"deflate_on_oom":true,"stats_polling_interval_s":1}`)
}

// --- tests ---

func TestBootSequence(t *testing.T) {
	r := newRig(t)
	if out := r.must("GET", "/", ""); !strings.Contains(out, `"Not started"`) {
		t.Fatalf("GET / = %s", out)
	}
	r.configure(t.TempDir())
	r.must("PUT", "/kling/network", `{"egress":"internet"}`)
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)

	vm, s, args := r.f.last()
	if args != "console=hvc0 reboot=k panic=1 root=/dev/vda ro ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off" {
		t.Fatalf("boot args not translated: %q", args)
	}
	if len(s.Drives) != 3 || s.Drives[0].DriveID != "rootfs" || s.Drives[1].DriveID != "overlay" || s.Drives[2].DriveID != "layer" {
		t.Fatalf("drives out of order: %+v", s.Drives)
	}
	if s.MachineIdentifier == "" || !s.Entropy || s.Network.GuestMAC != "06:00:AC:10:00:02" {
		t.Fatalf("spec incomplete: %+v", s)
	}
	if got := vm.ops(); len(got) != 1 || got[0] != "start" {
		t.Fatalf("vm ops = %v", got)
	}
	// Techo de memoria: 256 inflados de 512 -> el invitado ve 256.
	if len(vm.balloon) != 1 || vm.balloon[0] != 256 {
		t.Fatalf("balloon targets = %v, want [256]", vm.balloon)
	}
	if n := r.nets[0]; n.cfg.MMDS == nil || n.cfg.MMDSAddr.String() != "169.254.169.254" || n.cfg.Policy == nil {
		t.Fatalf("net config = %+v", n.cfg)
	}
	if r.srv.d.Policy.Mode() != egress.Internet {
		t.Fatal("egress policy not applied")
	}
	if out := r.must("GET", "/", ""); !strings.Contains(out, `"Running"`) {
		t.Fatalf("GET / = %s", out)
	}

	// Tras arrancar, los dispositivos ya no cambian.
	r.mustFail("PUT", "/drives/extra", `{"drive_id":"extra","path_on_host":"/x"}`, "before InstanceStart")
	r.mustFail("PUT", "/machine-config", `{"vcpu_count":2,"mem_size_mib":512}`, "before InstanceStart")
	r.mustFail("PUT", "/actions", `{"action_type":"InstanceStart"}`, "before InstanceStart")
}

func TestErrorsAndUnknownRoutes(t *testing.T) {
	r := newRig(t)
	r.mustFail("GET", "/vm/config", "", "does not implement GET /vm/config")
	r.mustFail("DELETE", "/drives/rootfs", "", "does not implement")
	r.mustFail("PUT", "/actions", `{"action_type":"SendCtrlAltDel"}`, "SendCtrlAltDel")
	r.mustFail("PUT", "/actions", `{"action_type":"InstanceStart"}`, "boot source")
	r.mustFail("PUT", "/drives/a", `{"drive_id":"b","path_on_host":"/x"}`, "does not match")
	r.mustFail("PUT", "/boot-source", `{`, "invalid JSON")
	r.mustFail("PUT", "/boot-source", `{"kernel_image_path":"`+strings.Repeat("a", maxConfigBody)+`"}`, "larger than")
	r.mustFail("PUT", "/mmds/config", `{"version":"V2"}`, "network interface")
	r.must("PUT", "/network-interfaces/eth0", `{"iface_id":"eth0"}`)
	r.mustFail("PUT", "/network-interfaces/eth1", `{"iface_id":"eth1"}`, "single network interface")
	r.mustFail("PUT", "/mmds/config", `{"version":"V1"}`, "only implements MMDS V2")
	r.mustFail("PUT", "/mmds/config", `{"version":"V2","ipv4_address":"10.0.0.1"}`, "link-local")
	r.mustFail("PATCH", "/balloon", `{"amount_mib":1}`, "not configured")
	r.mustFail("GET", "/balloon/statistics", "", "not configured")
	r.mustFail("PATCH", "/vm", `{"state":"Paused"}`, "cannot pause")
	r.mustFail("PATCH", "/vm", `{"state":"Frozen"}`, "invalid state")
	r.mustFail("PUT", "/snapshot/create", `{"snapshot_path":"/a","mem_file_path":"/b"}`, "must be paused")
	r.mustFail("PUT", "/snapshot/create", `{"snapshot_type":"Diff","snapshot_path":"/a","mem_file_path":"/b"}`, "Full")
	r.mustFail("PUT", "/kling/network", `{"egress":"everything"}`, "unknown egress policy")
	r.mustFail("PUT", "/kling/forwards", `{"ports":[8080]}`, "not up yet")
}

func TestCommitSnapshotSequence(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	r.configure(dir)
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	vm, _, _ := r.f.last()

	snap := filepath.Join(dir, "snap.file")
	mem := filepath.Join(dir, "mem.file")
	// Un volcado anterior en la misma ruta: Firecracker lo pisa, el framework no.
	if err := os.WriteFile(mem, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Secuencia de Commit (snapshot.go): pausa, overlay -> copia dorada,
	// snapshot, overlay -> el propio, reanudar.
	r.must("PATCH", "/vm", `{"state":"Paused"}`)
	r.must("PATCH", "/drives/overlay", `{"drive_id":"overlay","path_on_host":"/gold/overlay.ext4"}`)
	r.must("PATCH", "/balloon", `{"amount_mib":128}`)
	r.must("PUT", "/snapshot/create", `{"snapshot_type":"Full","snapshot_path":"`+snap+`","mem_file_path":"`+mem+`"}`)
	r.must("PATCH", "/drives/overlay", `{"drive_id":"overlay","path_on_host":"`+dir+`/overlay.ext4"}`)
	r.must("PATCH", "/vm", `{"state":"Resumed"}`)

	got, err := spec.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	if got.Drives[1].PathOnHost != "/gold/overlay.ext4" {
		t.Fatalf("the snapshot must record the patched overlay, got %s", got.Drives[1].PathOnHost)
	}
	if got.MachineIdentifier == "" || got.Balloon.AmountMiB != 128 || got.MMDSConfig == nil || !got.Entropy {
		t.Fatalf("snapshot incomplete: %+v", got)
	}
	ops := vm.ops()
	want := []string{"start", "pause", "save " + mem, "resume"}
	if strings.Join(ops, "|") != strings.Join(want, "|") {
		t.Fatalf("ops = %v, want %v", ops, want)
	}
	if vm.balloon[len(vm.balloon)-1] != 384 {
		t.Fatalf("PATCH /balloon 128 must set the target to 512-128, got %v", vm.balloon)
	}
}

func writeSnapshot(t *testing.T, dir string) (snap, mem string) {
	t.Helper()
	s := &spec.Spec{
		BootSource:        &spec.BootSource{KernelImagePath: "/k", BootArgs: "console=ttyS0 pci=off"},
		MachineConfig:     &spec.MachineConfig{VCPUCount: 1, MemSizeMiB: 256},
		Network:           &spec.NetworkInterface{IfaceID: "eth0", GuestMAC: "06:00:AC:10:00:02"},
		MMDSConfig:        &spec.MMDSConfig{Version: "V2", IPv4Address: "169.254.169.254"},
		Balloon:           &spec.Balloon{AmountMiB: 0},
		Entropy:           true,
		MachineIdentifier: "aWQ=",
		Drives: []spec.Drive{
			{DriveID: "rootfs", PathOnHost: "/i/min.ext4", IsRootDevice: true, IsReadOnly: true},
			{DriveID: "overlay", PathOnHost: "/gold/overlay.ext4"},
		},
	}
	snap, mem = filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem.file")
	if err := s.WriteFile(snap); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mem, []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	return snap, mem
}

// Secuencia de runFrom: cargar sin reanudar, reapuntar el overlay, reanudar.
func TestDeferredRestore(t *testing.T) {
	r := newRig(t)
	snap, mem := writeSnapshot(t, t.TempDir())
	r.must("PUT", "/kling/network", `{"egress":"none"}`)
	r.must("PUT", "/snapshot/load", `{"snapshot_path":"`+snap+`","mem_backend":{"backend_path":"`+mem+`","backend_type":"File"},"resume_vm":false}`)
	if len(r.f.vms) != 0 {
		t.Fatal("resume_vm=false must not create the VM yet")
	}
	if len(r.nets) != 1 {
		t.Fatal("the network must be up after load so forwards work")
	}
	out := r.must("PUT", "/kling/forwards", `{"ports":[8080, 9000, 8080]}`)
	var fw struct {
		Forwards map[string]string `json:"forwards"`
	}
	if err := json.Unmarshal([]byte(out), &fw); err != nil || len(fw.Forwards) != 2 || fw.Forwards["8080"] == "" {
		t.Fatalf("forwards = %s", out)
	}
	again := r.must("PUT", "/kling/forwards", `{"ports":[8080]}`)
	if !strings.Contains(again, fw.Forwards["8080"]) {
		t.Fatalf("repeating a port must return the same address: %s vs %s", again, out)
	}
	r.must("PATCH", "/drives/overlay", `{"drive_id":"overlay","path_on_host":"/m/1/overlay.ext4"}`)
	r.must("PATCH", "/vm", `{"state":"Resumed"}`)

	vm, s, args := r.f.last()
	if s.Drives[1].PathOnHost != "/m/1/overlay.ext4" {
		t.Fatalf("the VM was created with %s, not the patched overlay", s.Drives[1].PathOnHost)
	}
	if s.MachineIdentifier != "aWQ=" {
		t.Fatal("the machine identifier must come from the snapshot")
	}
	if args != "console=hvc0" {
		t.Fatalf("boot args = %q", args)
	}
	if ops := vm.ops(); strings.Join(ops, "|") != "restore "+mem+"|resume" {
		t.Fatalf("ops = %v", ops)
	}
	if len(r.nets) != 1 {
		t.Fatal("resuming must reuse the network created at load")
	}
	r.mustFail("PUT", "/snapshot/load", `{"snapshot_path":"`+snap+`","mem_backend":{"backend_path":"`+mem+`"}}`, "fresh")
}

// Secuencia de Thaw: cargar y reanudar de una vez.
func TestLoadAndResume(t *testing.T) {
	r := newRig(t)
	snap, mem := writeSnapshot(t, t.TempDir())
	r.must("PUT", "/snapshot/load", `{"snapshot_path":"`+snap+`","mem_backend":{"backend_path":"`+mem+`","backend_type":"File"},"resume_vm":true}`)
	vm, _, _ := r.f.last()
	if ops := vm.ops(); strings.Join(ops, "|") != "restore "+mem+"|resume" {
		t.Fatalf("ops = %v", ops)
	}
	if out := r.must("GET", "/", ""); !strings.Contains(out, "Running") {
		t.Fatalf("state = %s", out)
	}
}

func TestLoadRejects(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	snap, mem := writeSnapshot(t, dir)
	r.mustFail("PUT", "/snapshot/load", `{"snapshot_path":"`+snap+`","mem_backend":{"backend_path":"`+mem+`","backend_type":"Uffd"}}`, "File memory backend")
	r.mustFail("PUT", "/snapshot/load", `{"snapshot_path":"`+snap+`","mem_backend":{"backend_path":"`+dir+`/nope"}}`, "state file")
	bad := filepath.Join(dir, "fc.snap")
	_ = os.WriteFile(bad, []byte{0x00, 0x01}, 0o644)
	r.mustFail("PUT", "/snapshot/load", `{"snapshot_path":"`+bad+`","mem_backend":{"backend_path":"`+mem+`"}}`, "not a kling-vz snapshot")
}

func TestFailedRestoreCanRetry(t *testing.T) {
	r := newRig(t)
	snap, mem := writeSnapshot(t, t.TempDir())
	r.f.failNext = "restore " + mem
	r.mustFail("PUT", "/snapshot/load", `{"snapshot_path":"`+snap+`","mem_backend":{"backend_path":"`+mem+`"},"resume_vm":true}`, "restoring the machine state")
	first, _, _ := r.f.last()
	if ops := first.ops(); ops[len(ops)-1] != "stop" {
		t.Fatalf("the failed VM must be stopped, ops %v", ops)
	}
	// La VM abandonada avisa de su parada, pero eso no termina el proceso.
	select {
	case <-r.srv.Done():
		t.Fatal("an abandoned VM must not end the helper")
	case <-time.After(50 * time.Millisecond):
	}
	r.must("PATCH", "/vm", `{"state":"Resumed"}`)
	if len(r.f.vms) != 2 {
		t.Fatalf("retry must create a new VM (%d created)", len(r.f.vms))
	}
}

// El objetivo del globo fijado al arrancar se pierde en el framework (el
// driver del invitado aún no existe): se tiene que repetir en los primeros
// segundos, con el valor vigente, y dejar de hacerlo si ya no hay techo.
func TestBalloonReappliedAfterBoot(t *testing.T) {
	r := newRig(t)
	r.srv.globoEn = []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 200 * time.Millisecond}
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	vm, _, _ := r.f.last()
	objetivos := func() []int {
		vm.mu.Lock()
		defer vm.mu.Unlock()
		return append([]int(nil), vm.balloon...)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(objetivos()) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := objetivos(); len(got) < 3 || got[1] != 256 || got[2] != 256 {
		t.Fatalf("balloon targets = %v, want 256 re-applied", got)
	}
	// Desinflado del todo antes del último reintento: ya no se toca.
	r.must("PATCH", "/balloon", `{"amount_mib":0}`)
	n := len(objetivos())
	time.Sleep(300 * time.Millisecond)
	if got := objetivos(); len(got) != n {
		t.Fatalf("re-applied after the ceiling was lifted: %v", got)
	}
}

func TestProbe(t *testing.T) {
	r := newRig(t)
	r.mustFail("GET", "/kling/probe?port=8080", "", "not up")
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	r.must("PUT", "/kling/forwards", `{"ports":[8080,9001]}`)
	if out := r.must("GET", "/kling/probe?port=8080", ""); !strings.Contains(out, `"open":true`) {
		t.Fatalf("probe 8080 = %s", out)
	}
	if out := r.must("GET", "/kling/probe?port=9001", ""); !strings.Contains(out, `"open":false`) {
		t.Fatalf("probe 9001 = %s", out)
	}
	r.mustFail("GET", "/kling/probe?port=x", "", "invalid port")
}

func TestBalloonStatsAndFootprint(t *testing.T) {
	r := newRig(t)
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	r.must("PATCH", "/balloon", `{"amount_mib":100}`)
	var st map[string]int
	_ = json.Unmarshal([]byte(r.must("GET", "/balloon/statistics", "")), &st)
	if st["target_mib"] != 100 || st["actual_mib"] != 100 || st["free_memory"] != 0 {
		t.Fatalf("stats = %v", st)
	}
	r.mustFail("PATCH", "/balloon", `{"amount_mib":512}`, "below mem_size_mib")
	if out := r.must("GET", "/kling/stats", ""); !strings.Contains(out, `"footprint_mib":151`) {
		t.Fatalf("stats = %s (must round up)", out)
	}
	if out := r.must("GET", "/kling/info", ""); !strings.Contains(out, `"backend":"vz"`) || !strings.Contains(out, "9.9.9") {
		t.Fatalf("info = %s", out)
	}
}

func TestMMDSStoreAPI(t *testing.T) {
	r := newRig(t)
	r.must("PUT", "/mmds", `{"env":{"A":"1"}}`)
	r.must("PATCH", "/mmds", `{"env":{"B":"2"}}`)
	if out := r.must("GET", "/mmds", ""); !strings.Contains(out, `"A":"1"`) || !strings.Contains(out, `"B":"2"`) {
		t.Fatalf("store = %s", out)
	}
	r.mustFail("PUT", "/mmds", `{"x":"`+strings.Repeat("a", maxMMDSBody)+`"}`, "larger than")
}

func TestGuestStopEndsHelper(t *testing.T) {
	r := newRig(t)
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	vm, _, _ := r.f.last()
	vm.stopped <- nil
	select {
	case err := <-r.srv.Done():
		if err != nil {
			t.Fatalf("clean guest stop reported %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not fire after the guest stopped")
	}
	r.mustFail("PATCH", "/drives/overlay", `{"path_on_host":"/x"}`, "stopped")
}

func TestShutdown(t *testing.T) {
	r := newRig(t)
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	vm, _, _ := r.f.last()
	r.srv.Shutdown()
	if ops := vm.ops(); ops[len(ops)-1] != "stop" {
		t.Fatalf("Shutdown must stop the VM: %v", ops)
	}
	if !r.nets[0].closed {
		t.Fatal("Shutdown must close the network")
	}
}
