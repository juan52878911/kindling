package server

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/vz/internal/spec"
)

// vfsFactory es la Factory de kling-hv para estas pruebas: sabe de virtio-fs
// y sus VM guardan las sesiones que reciben.
type vfsFactory struct{ fakeFactory }

func (f *vfsFactory) VirtioFS() bool { return true }

func (f *vfsFactory) Create(s *spec.Spec, bootArgs string, n Network) (VM, error) {
	vm, err := f.fakeFactory.Create(s, bootArgs, n)
	if err != nil {
		return nil, err
	}
	return &vfsVM{fakeVM: vm.(*fakeVM), estado: map[int][]byte{}, sesiones: make(chan int, 4)}, nil
}

type vfsVM struct {
	*fakeVM
	mu       sync.Mutex
	estado   map[int][]byte
	sesiones chan int
}

func (v *vfsVM) AttachShare(tag int, rwc io.ReadWriteCloser) error {
	rwc.Close()
	v.sesiones <- tag
	return nil
}

func (v *vfsVM) ShareState() (map[int][]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.estado, nil
}

func newRigVFS(t *testing.T) (*rig, *vfsFactory) {
	r := newRig(t)
	f := &vfsFactory{}
	r.srv.d.Factory = f
	return r, f
}

// kling-vz no hace virtio-fs: PUT /kling/shares le dice que no con un 501,
// que el núcleo lee como "por FUSE".
func TestKlingSharesSinVirtioFS(t *testing.T) {
	r := newRig(t)
	if code, out := r.call("PUT", "/kling/shares", `{"shares":[{"tag":0}]}`); code != http.StatusNotImplemented {
		t.Fatalf("PUT /kling/shares on kling-vz = %d %s", code, out)
	}
}

// Con kling-hv las carpetas van en el spec de la VM y en su snapshot (con lo
// que guarde su servidor FUSE, que no se queda en memoria), y solo antes de
// arrancar.
func TestKlingSharesSnapshot(t *testing.T) {
	r, f := newRigVFS(t)
	dir := t.TempDir()
	r.configure(dir)
	r.mustFail("PUT", "/kling/shares", `{"shares":[{"tag":0},{"tag":0}]}`, "unique")
	r.mustFail("PUT", "/kling/shares", `{"shares":[{"tag":8}]}`, "between 0 and 7")
	r.must("PUT", "/kling/shares", `{"shares":[{"tag":0,"read_only":true,"state":"AAAA"},{"tag":2}]}`)
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	r.mustFail("PUT", "/kling/shares", `{"shares":[]}`, "InstanceStart")
	f.mu.Lock()
	vs := f.specs[len(f.specs)-1].VirtioFS
	f.mu.Unlock()
	if len(vs) != 2 || vs[0].Tag != 0 || !vs[0].ReadOnly || vs[0].State != nil || vs[1].Tag != 2 {
		t.Fatalf("the VM got %+v", vs)
	}
	r.srv.mu.Lock()
	v := r.srv.vm.(*vfsVM)
	r.srv.mu.Unlock()
	v.mu.Lock()
	v.estado[0], v.estado[2] = []byte("nodos-0"), []byte("nodos-2")
	v.mu.Unlock()

	snap, mem := filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem.file")
	r.must("PATCH", "/vm", `{"state":"Paused"}`)
	r.must("PUT", "/snapshot/create", `{"snapshot_type":"Full","snapshot_path":"`+snap+`","mem_file_path":"`+mem+`"}`)
	got, err := spec.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	if got.KlingVZ != 4 || len(got.VirtioFS) != 2 || string(got.VirtioFS[0].State) != "nodos-0" || string(got.VirtioFS[1].State) != "nodos-2" {
		t.Fatalf("snapshot: kling_vz %d, virtio-fs %+v", got.KlingVZ, got.VirtioFS)
	}
	r.srv.mu.Lock()
	quedo := r.srv.spec.VirtioFS[0].State
	r.srv.mu.Unlock()
	if quedo != nil {
		t.Fatal("the virtio-fs state stayed in the live spec")
	}
}

// POST /kling/share/attach: con Upgrade, a una carpeta que la VM tiene; la
// conexión pasa a la VM tras el 101.
func TestShareAttach(t *testing.T) {
	r, _ := newRigVFS(t)
	r.configure(t.TempDir())
	r.must("PUT", "/kling/shares", `{"shares":[{"tag":1}]}`)
	if code, _ := r.call("POST", "/kling/share/attach", `{"tag":1}`); code != http.StatusUpgradeRequired {
		t.Fatalf("without Upgrade = %d", code)
	}
	ts := httptest.NewServer(r.h)
	defer ts.Close()
	attach := func(tag string) (int, *bufio.Reader, net.Conn) {
		conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest("POST", ts.URL+"/kling/share/attach", strings.NewReader(`{"tag":`+tag+`}`))
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "kling-share/1")
		if err := req.Write(conn); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, br, conn
	}
	// Antes de arrancar no hay VM.
	if code, _, c := attach("1"); code != http.StatusNotImplemented {
		t.Fatalf("before start = %d", code)
	} else {
		c.Close()
	}
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	if code, _, c := attach("3"); code != http.StatusNotImplemented {
		t.Fatalf("a share the VM does not have = %d", code)
	} else {
		c.Close()
	}
	code, _, c := attach("1")
	defer c.Close()
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("attach = %d", code)
	}
	r.srv.mu.Lock()
	v := r.srv.vm.(*vfsVM)
	r.srv.mu.Unlock()
	if tag := <-v.sesiones; tag != 1 {
		t.Fatalf("the VM got the session of share %d", tag)
	}
}
