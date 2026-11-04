package server

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/vz/internal/spec"
)

// finVM es una fakeVM que sabe decir por qué terminó (como kling-hv).
type finVM struct {
	*fakeVM
	motivo string
}

func (v *finVM) EndReason() string { return v.motivo }

type finFactory struct {
	*fakeFactory
	motivo string
}

func (f *finFactory) Create(s *spec.Spec, bootArgs string, n Network) (VM, error) {
	vm, err := f.fakeFactory.Create(s, bootArgs, n)
	if err != nil {
		return nil, err
	}
	return &finVM{fakeVM: vm.(*fakeVM), motivo: f.motivo}, nil
}

type registro struct {
	mu     sync.Mutex
	lineas []string
}

func (l *registro) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lineas = append(l.lineas, fmt.Sprintf(f, a...))
}

func (l *registro) tiene(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.lineas {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

// rigFin es newRig con una fábrica de finVM y el registro a la vista.
func rigFin(t *testing.T, motivo string) (*rig, *finFactory, *registro) {
	t.Helper()
	r := newRig(t)
	ff := &finFactory{fakeFactory: r.f, motivo: motivo}
	log := &registro{}
	r.srv = New(Deps{
		Factory: ff,
		NewNet: func(c NetConfig) (Network, error) {
			n := &fakeNet{cfg: c, fwd: map[int]string{}}
			r.nets = append(r.nets, n)
			return n, nil
		},
		Footprint: func() (uint64, error) { return 1, nil },
		Logf:      log.logf,
	})
	r.srv.globoEn = nil
	r.h = r.srv.Handler()
	return r, ff, log
}

func esperarFin(t *testing.T, r *rig) {
	t.Helper()
	select {
	case <-r.srv.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("el servidor no vio parar la VM")
	}
}

// Un pause (o resume, o snapshot) tras apagarse el invitado dice por qué se
// paró, y el registro también.
func TestFinMotivoEnErroresYRegistro(t *testing.T) {
	r, _, log := rigFin(t, "the guest powered off")
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	vm, _, _ := r.f.last()
	vm.stopped <- nil
	esperarFin(t, r)
	if !log.tiene("the guest stopped: the guest powered off") {
		t.Fatalf("el registro no dice el motivo: %q", log.lineas)
	}
	r.mustFail("PATCH", "/vm", `{"state":"Paused"}`, "Stopped (the guest powered off)")
	r.mustFail("PATCH", "/vm", `{"state":"Resumed"}`, "Stopped (the guest powered off)")
	r.mustFail("PATCH", "/drives/overlay", `{"drive_id":"overlay","path_on_host":"/x"}`, "has stopped (the guest powered off)")
}

// Con un error y un motivo distintos, el registro y la API dan los dos.
func TestFinErrorYMotivo(t *testing.T) {
	r, _, log := rigFin(t, "vCPU 0: unexpected exit")
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	vm, _, _ := r.f.last()
	vm.stopped <- errors.New("the VMM failed")
	esperarFin(t, r)
	if !log.tiene("the VM stopped with an error: the VMM failed (end reason: vCPU 0: unexpected exit)") {
		t.Fatalf("el registro no dice error y motivo: %q", log.lineas)
	}
	r.mustFail("PATCH", "/vm", `{"state":"Paused"}`, "the VMM failed; vCPU 0: unexpected exit")
}

// El error de Pause del VMM llega tal cual en la respuesta.
func TestPausaFallidaDaElTexto(t *testing.T) {
	r, _, _ := rigFin(t, "")
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	vm, _, _ := r.f.last()
	vm.mu.Lock()
	vm.failNext = "pause"
	vm.mu.Unlock()
	r.mustFail("PATCH", "/vm", `{"state":"Paused"}`, "cannot pause: pause failed")
	// La VM sigue corriendo: el siguiente pause funciona.
	r.must("PATCH", "/vm", `{"state":"Paused"}`)
	vm.mu.Lock()
	vm.failNext = "resume"
	vm.mu.Unlock()
	r.mustFail("PATCH", "/vm", `{"state":"Resumed"}`, "cannot resume: resume failed")
}
