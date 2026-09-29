package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/vz/internal/egress"
	"github.com/juan52878911/kindling/vz/internal/grafo"
	"github.com/juan52878911/kindling/vz/internal/spec"
)

// --- dobles ---

type fakeVM struct {
	mu       sync.Mutex
	calls    []string
	balloon  []int
	stopped  chan error
	failNext string // nombre de la operación que debe fallar

	// Un invitado que quema toda su vCPU mientras no está en pausa: lo que
	// mide el regulador de CPU.
	enMarcha bool
	desde    time.Time
	corrido  time.Duration
}

func (v *fakeVM) marcha(on bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.enMarcha {
		v.corrido += time.Since(v.desde)
	}
	v.enMarcha, v.desde = on, time.Now()
}

// cpu es la CPU que lleva gastada (toda la de cuando no estaba en pausa).
func (v *fakeVM) cpu() time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.enMarcha {
		return v.corrido + time.Since(v.desde)
	}
	return v.corrido
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

func (v *fakeVM) Start() error { v.marcha(true); return v.rec("start") }
func (v *fakeVM) Pause() error {
	err := v.rec("pause")
	if err == nil {
		v.marcha(false)
	}
	return err
}
func (v *fakeVM) Resume() error {
	err := v.rec("resume")
	if err == nil {
		v.marcha(true)
	}
	return err
}
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

// El confinamiento se aplica una vez, al crear la VM, con la red que diga la
// política en ese momento; y después no se puede ampliar el egress a algo que
// el sandbox ya no deja hacer (estrechar sí).
func TestConfinarAlCrearYNoAmpliarElEgress(t *testing.T) {
	r := newRig(t)
	var llamadas []bool
	r.srv.d.Confine = func(conRed, _ bool) error { llamadas = append(llamadas, conRed); return nil }

	r.configure(t.TempDir())
	r.must("PUT", "/kling/network", `{"egress":"none"}`)
	if len(llamadas) != 0 {
		t.Fatal("se confinó antes de crear la VM")
	}
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	if len(llamadas) != 1 || llamadas[0] {
		t.Fatalf("Confine = %v, quería una llamada sin red", llamadas)
	}
	r.mustFail("PUT", "/kling/network", `{"egress":"internet"}`, "can't be enabled")
	r.mustFail("PUT", "/kling/network", `{"egress":"allowlist","allow_domains":["a.com"]}`, "can't be enabled")
	r.must("PUT", "/kling/network", `{"egress":"none"}`)
	if r.srv.d.Policy.Mode() != egress.None {
		t.Fatal("la política cambió pese al rechazo")
	}
}

// Con salida al crear, el sandbox la permite y se puede cambiar de modo.
func TestConfinarConRed(t *testing.T) {
	r := newRig(t)
	var llamadas []bool
	r.srv.d.Confine = func(conRed, _ bool) error { llamadas = append(llamadas, conRed); return nil }
	r.configure(t.TempDir())
	r.must("PUT", "/kling/network", `{"egress":"internet"}`)
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	if len(llamadas) != 1 || !llamadas[0] {
		t.Fatalf("Confine = %v, quería una llamada con red", llamadas)
	}
	r.must("PUT", "/kling/network", `{"egress":"allowlist","allow_domains":["a.com"]}`)
}

// Si no se puede confinar, la VM no se crea: mejor no arrancar que arrancar
// sin la barrera.
func TestSinConfinarNoSeArranca(t *testing.T) {
	r := newRig(t)
	r.srv.d.Confine = func(bool, bool) error { return errors.New("perfil roto") }
	r.configure(t.TempDir())
	r.mustFail("PUT", "/actions", `{"action_type":"InstanceStart"}`, "perfil roto")
	r.f.mu.Lock()
	creadas := len(r.f.vms)
	r.f.mu.Unlock()
	if creadas != 0 {
		t.Fatal("se creó la VM sin confinar")
	}
}

// Con un techo del 50 % y un invitado que quema toda su vCPU, el regulador lo
// deja corriendo más o menos la mitad del tiempo.
func TestReguladorDeCPUDejaElTecho(t *testing.T) {
	r := newRig(t)
	r.srv.cpuPeriodo, r.srv.cpuGracia = 10*time.Millisecond, time.Millisecond
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	vm, _, _ := r.f.last()
	r.srv.d.CPUTime = func() (time.Duration, error) { return vm.cpu(), nil }

	r.must("PUT", "/kling/cpu", `{"pct":50}`)
	inicio, cpu0 := time.Now(), vm.cpu()
	time.Sleep(1500 * time.Millisecond)
	frac := float64(vm.cpu()-cpu0) / float64(time.Since(inicio))
	t.Logf("fracción de CPU con techo 50 %%: %.2f", frac)
	if frac < 0.35 || frac > 0.65 {
		t.Fatalf("fracción de CPU = %.2f, quería ~0.50", frac)
	}

	// Sin techo, vuelve a correr entero.
	r.must("PUT", "/kling/cpu", `{"pct":0}`)
	time.Sleep(50 * time.Millisecond)
	inicio, cpu0 = time.Now(), vm.cpu()
	time.Sleep(300 * time.Millisecond)
	if frac := float64(vm.cpu()-cpu0) / float64(time.Since(inicio)); frac < 0.9 {
		t.Fatalf("sin techo la fracción es %.2f, quería ~1", frac)
	}
}

// Si el núcleo pausa la VM justo mientras el regulador la tiene en pausa, la
// pausa pasa a ser del núcleo: sigue en pausa, el estado es Paused y el
// regulador no la reanuda.
func TestPausaDelNucleoDuranteLaDelRegulador(t *testing.T) {
	r := newRig(t)
	r.srv.cpuPeriodo, r.srv.cpuGracia = 10*time.Millisecond, time.Millisecond
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	vm, _, _ := r.f.last()
	r.srv.d.CPUTime = func() (time.Duration, error) { return vm.cpu(), nil }
	r.must("PUT", "/kling/cpu", `{"pct":10}`) // pausas largas: fácil caer en una

	deadline := time.Now().Add(3 * time.Second)
	for {
		r.srv.mu.Lock()
		reg := r.srv.regulando
		r.srv.mu.Unlock()
		if reg {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("el regulador no llegó a pausar")
		}
		time.Sleep(time.Millisecond)
	}
	r.must("PATCH", "/vm", `{"state":"Paused"}`)
	time.Sleep(300 * time.Millisecond) // más que cualquier pausa del regulador con pct 10 y ventana 10 ms
	if out := r.must("GET", "/", ""); !strings.Contains(out, `"Paused"`) {
		t.Fatalf("estado tras pausar = %s", out)
	}
	vm.mu.Lock()
	enMarcha := vm.enMarcha
	vm.mu.Unlock()
	if enMarcha {
		t.Fatal("el regulador reanudó una VM que el núcleo había pausado")
	}
	r.must("PATCH", "/vm", `{"state":"Resumed"}`)
}

// PUT /kling/credentials: solo en allowlist, fija el juego entero en el proxy y
// desvía sus dominios a la pasarela; uno inválido no toca el anterior.
func TestKlingCredentials(t *testing.T) {
	r := newRig(t)
	const cred = `{"credentials":[{"env":"API_KEY","domain":"API.example.com","placeholder":"kling-cred-abc","secret":"sk-1"}]}`
	r.mustFail("PUT", "/kling/credentials", cred, "no credential proxy")

	gw := netip.MustParseAddr("172.16.0.1")
	if out := r.must("GET", "/kling/info", ""); strings.Contains(out, "credential_kinds") {
		t.Fatalf("info without a proxy = %s", out)
	}
	r.srv.d.Credentials = credproxy.New(credproxy.Options{})
	r.srv.d.CredIP = gw
	// El daemon lo pregunta antes de mandar una credencial Postgres.
	if out := r.must("GET", "/kling/info", ""); !strings.Contains(out, `"credential_kinds":["http","postgres","postgres-upstream","mysql"]`) {
		t.Fatalf("info = %s", out)
	}
	r.mustFail("PUT", "/kling/credentials", cred, "need egress allowlist")
	r.must("PUT", "/kling/network", `{"egress":"allowlist","allow_domains":["other.org"]}`)
	if out := r.must("PUT", "/kling/credentials", cred); !strings.Contains(out, `"domains":["api.example.com"]`) {
		t.Fatalf("PUT /kling/credentials = %s", out)
	}
	if ip, ok := r.srv.d.Policy.CredHost("api.example.com"); !ok || ip != gw {
		t.Fatal("the credential domain is not diverted to the gateway")
	}
	r.mustFail("PUT", "/kling/credentials", `{"credentials":[{"domain":"*.example.com","placeholder":"kling-cred-x","secret":"s"}]}`, "exact host name")
	r.mustFail("PUT", "/kling/credentials", `{"credentials":[{"domain":"b.example.com","placeholder":"nope","secret":"s"}]}`, "invalid placeholder")
	if _, ok := r.srv.d.Policy.CredHost("api.example.com"); !ok {
		t.Fatal("a rejected set must keep the previous one")
	}

	// allow viaja hasta el proxy y se cablea igual que en Linux: casa GET pero
	// no POST sobre la misma ruta.
	const credAllow = `{"credentials":[{"env":"API_KEY","domain":"api.example.com","placeholder":"kling-cred-abc","secret":"sk-1","allow":["GET /v1/balance"]}]}`
	r.must("PUT", "/kling/credentials", credAllow)
	req := httptest.NewRequest(http.MethodPost, "http://api.example.com/v1/balance", nil)
	req.Host = "api.example.com"
	rec := httptest.NewRecorder()
	r.srv.d.Credentials.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST fuera de allow: %d", rec.Code)
	}

	// El tipo y sus campos viajan hasta el proxy: una credencial Postgres lo
	// deja activo en su papel de Postgres (y no la usa el HTTP).
	if r.srv.d.Credentials.PGActivo() {
		t.Fatal("PGActivo without a postgres credential")
	}
	const credPG = `{"credentials":[{"env":"PGPASSWORD","domain":"db.example.com","placeholder":"kling-cred-pg","secret":"pw","kind":"postgres","port":5432,"user":"app","database":"appdb"}]}`
	if out := r.must("PUT", "/kling/credentials", credPG); !strings.Contains(out, `"domains":["db.example.com"]`) {
		t.Fatalf("PUT /kling/credentials (postgres) = %s", out)
	}
	if !r.srv.d.Credentials.PGActivo() {
		t.Fatal("the postgres credential did not reach the proxy")
	}
	r.mustFail("PUT", "/kling/credentials", `{"credentials":[{"env":"PGPASSWORD","domain":"db.example.com","placeholder":"kling-cred-pg","secret":"pw","kind":"postgres"}]}`, "needs -user")
	// Y una MySQL, en su papel de MySQL.
	const credMy = `{"credentials":[{"env":"MYSQL_PWD","domain":"mysql.example.com","placeholder":"kling-cred-my","secret":"pw","kind":"mysql","user":"app","database":"appdb"}]}`
	if out := r.must("PUT", "/kling/credentials", credMy); !strings.Contains(out, `"domains":["mysql.example.com"]`) {
		t.Fatalf("PUT /kling/credentials (mysql) = %s", out)
	}
	if !r.srv.d.Credentials.MySQLActivo() || r.srv.d.Credentials.PGActivo() {
		t.Fatal("the mysql credential did not reach the proxy as mysql")
	}
	r.must("PUT", "/kling/credentials", credPG)

	// La red nace con el proxy, en sus dos papeles.
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	if r.nets[0].cfg.Credentials == nil || r.nets[0].cfg.CredentialsPG == nil {
		t.Fatal("the network was not given the credential proxy")
	}

	// La lista vacía las quita, en cualquier modo.
	r.must("PUT", "/kling/credentials", `{"credentials":[]}`)
	if _, ok := r.srv.d.Policy.CredHost("api.example.com"); ok {
		t.Fatal("an empty set must stop the diversion")
	}
}

// PUT /kling/graph y las credenciales hacia otra máquina: solo con el broker
// del daemon (Deps.Graph). Con él se anuncia graph-link, los nombres de las
// aristas resuelven a la pasarela en cualquier modo, la red recibe las
// aristas, y las credenciales que van todas a otra máquina valen sin
// allowlist; mezcladas con otras, no.
func TestKlingGraph(t *testing.T) {
	r := newRig(t)
	gw := netip.MustParseAddr("172.16.0.1")
	r.srv.d.Credentials = credproxy.New(credproxy.Options{})
	r.srv.d.CredIP = gw
	const aristas = `{"links":[{"host":"api.graph","port":8081}],"hosts":["api.graph","db.graph"]}`
	const credMaq = `{"credentials":[{"env":"PGPASSWORD","domain":"db.graph","placeholder":"kling-cred-pg","secret":"pw","kind":"postgres","port":5432,"user":"app","database":"shop","upstream_tls":"disable","upstream_machine":"0123456789abcdef0123456789abcdef","upstream_owner":"0123456789abcdef"}]}`

	// Sin broker: ni aristas, ni credenciales a otra máquina, ni capacidad.
	r.mustFail("PUT", "/kling/graph", aristas, "no link broker")
	r.mustFail("PUT", "/kling/credentials", credMaq, "link broker")
	if out := r.must("GET", "/kling/info", ""); strings.Contains(out, credproxy.CapGraphLink) {
		t.Fatalf("info without a broker = %s", out)
	}

	r.srv.d.Graph = grafo.NewConDial(func(context.Context) (*net.UnixConn, error) {
		return nil, errors.New("no daemon in this test")
	}, nil, nil)
	if out := r.must("GET", "/kling/info", ""); !strings.Contains(out, `"credential_kinds":["http","postgres","postgres-upstream","mysql","graph-link"]`) {
		t.Fatalf("info = %s", out)
	}
	r.mustFail("PUT", "/kling/graph", `{"links":[{"host":"api.graph","port":8080}],"hosts":["api.graph"]}`, "guest agent")
	r.mustFail("PUT", "/kling/graph", `{"links":[{"host":"api.graph","port":8081}],"hosts":[]}`, "not in hosts")
	r.mustFail("PUT", "/kling/graph", `{"links":[],"hosts":["api.example.com"]}`, "not a graph name")
	r.must("PUT", "/kling/graph", aristas)
	for _, h := range []string{"api.graph", "db.graph"} {
		if ip, _, ok := r.srv.d.Policy.GraphHost(h); !ok || ip != gw {
			t.Fatalf("%s is not answered with the gateway", h)
		}
	}
	if _, esGrafo, ok := r.srv.d.Policy.GraphHost("cache.graph"); !esGrafo || ok {
		t.Fatal("a graph name without an edge must not resolve")
	}
	if !r.srv.d.Graph.Link(8081) || r.srv.d.Graph.Link(5432) {
		t.Fatal("the links did not reach the graph")
	}

	// egress none (lo que tiene la máquina por defecto): las aristas
	// credential sí; una credencial normal junto a ellas, no.
	r.must("PUT", "/kling/credentials", credMaq)
	if !r.srv.d.Credentials.SoloMaquinas() {
		t.Fatal("the machine credential did not reach the proxy")
	}
	const mezcla = `{"credentials":[{"env":"PGPASSWORD","domain":"db.graph","placeholder":"kling-cred-pg","secret":"pw","kind":"postgres","port":5432,"user":"app","database":"shop","upstream_tls":"disable","upstream_machine":"0123456789abcdef0123456789abcdef","upstream_owner":"0123456789abcdef"},` +
		`{"env":"API_KEY","domain":"api.example.com","placeholder":"kling-cred-abc","secret":"sk-1"}]}`
	r.mustFail("PUT", "/kling/credentials", mezcla, "need egress allowlist")

	// La red nace con las aristas.
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	if r.nets[0].cfg.Graph != r.srv.d.Graph {
		t.Fatal("the network was not given the graph")
	}
}
