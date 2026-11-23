//go:build darwin

package machine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

func plataformaConProc() bool { return true }

// proc_pid_rusage da los despertares y la E/S de disco del proceso: escribir
// y sincronizar con F_FULLFSYNC 4 MiB los hace subir.
func TestCPUProcesoDespertaresYDisco(t *testing.T) {
	antes, ok := cpuProceso(os.Getpid())
	if !ok || !antes.conDespertares || !antes.conDisco {
		t.Fatalf("rusage sin despertares o disco: %+v", antes)
	}
	if antes.despReposo+antes.despInterrupcion == 0 {
		t.Errorf("ningún despertar: %+v", antes)
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "d"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i * 7)
	}
	for range 4 {
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_FULLFSYNC, 0); e != 0 {
		t.Fatal(e)
	}
	despues, _ := cpuProceso(os.Getpid())
	if d := despues.escritos - antes.escritos; d < 4<<20 {
		t.Errorf("escritos subió %d bytes, quería ≥ 4 MiB", d)
	}
}

// La huella y la residente del propio proceso, en MiB.
func TestMemoriaPropia(t *testing.T) {
	h, rss := memoriaPropia(os.Getpid())
	if h <= 0 || rss <= 0 {
		t.Fatalf("huella %d, rss %d", h, rss)
	}
	if h0, _ := memoriaPropia(1 << 30); h0 != 0 {
		t.Fatalf("un PID que no existe tiene huella %d", h0)
	}
}

// vmmQueContesta levanta un VMM de mentira por socket Unix que contesta code
// con fault_message msg a todo.
func vmmQueContesta(t *testing.T, code int, msg string) *fc.Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "k507")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Skipf("sin socket unix: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"fault_message":%q}`, msg)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return fc.New(filepath.Join(dir, "s"))
}

// El 507 de kling-hv (HV_NO_RESOURCES) en InstanceStart o en PUT /snapshot/load
// llega como falta de capacidad (api.IsInsufficientMemory), que el planificador
// trata congelando ociosas; antes se aplanaba a texto y acababa en un 500. El
// texto sigue empezando por "firecracker ", que es como errorDelVMM sabe que
// contestó el VMM. Un 400 no cambia.
func TestEl507DelVMMEsFaltaDeCapacidad(t *testing.T) {
	ctx := context.Background()
	c := vmmQueContesta(t, http.StatusInsufficientStorage, "creating the VM: this Mac has no room for another VM")
	for nombre, err := range map[string]error{
		"InstanceStart": c.Start(ctx),
		"snapshot/load": c.LoadSnapshotRepoint(ctx, "/s/snap.file", "/s/mem.file", true, false, nil),
	} {
		got := sinCapacidadVMM(err)
		if !api.IsInsufficientMemory(got) {
			t.Errorf("%s: un 507 del VMM no es falta de capacidad: %v", nombre, got)
		}
		if !errorDelVMM(got) || !strings.Contains(got.Error(), "no room for another VM") {
			t.Errorf("%s: se perdió el texto del VMM: %q", nombre, got)
		}
	}
	malo := vmmQueContesta(t, http.StatusBadRequest, "bad config").Start(ctx)
	if got := sinCapacidadVMM(malo); got != malo || api.IsInsufficientMemory(got) {
		t.Errorf("un 400 cambió: %v", got)
	}
	if sinCapacidadVMM(nil) != nil {
		t.Error("nil dejó de ser nil")
	}
}

// Con hv, la alta 121 (KLING_HV_MAX_VMS, 120 por defecto) se rechaza en la
// admisión con 507, nombrando el cupo de Hypervisor.framework. Cuentan las
// que tienen VMM (PID) y las que están naciendo; las warm no gastan cupo. Con
// vz no aplica.
func TestCupoHVEnLaAdmision(t *testing.T) {
	antes := backendVMM
	t.Cleanup(func() { backendVMM = antes })
	t.Setenv("KLING_HV_MAX_VMS", "")
	m := &Manager{byID: map[string]*api.Machine{}}
	for i := range 119 {
		id := fmt.Sprintf("viva%012d", i)
		m.byID[id] = &api.Machine{ID: id, State: api.StateRunning, PID: 1000 + i}
	}
	for i := range 30 {
		id := fmt.Sprintf("warm%012d", i)
		m.byID[id] = &api.Machine{ID: id, State: api.StateWarm}
	}

	backendVMM = BackendHV
	// La 120, naciendo (Run aparta su hueco antes de admitir): cabe.
	m.naciendo = 1
	if err := m.checkCupoHV(); err != nil {
		t.Fatalf("la 120 no cabe con 119 vivas y 30 warm: %v", err)
	}
	// Despertar una warm con 119 vivas también cabe: es la 120.
	m.naciendo = 0
	if err := m.checkCupoHV(); err != nil {
		t.Fatalf("despertar la 120 no cabe: %v", err)
	}
	// Con 120 vivas, la 121 no.
	m.byID["viva000000000119"] = &api.Machine{ID: "viva000000000119", State: api.StateRunning, PID: 9999}
	m.naciendo = 1
	err := m.checkCupoHV()
	if !api.IsInsufficientMemory(err) || !strings.Contains(err.Error(), "Hypervisor.framework") {
		t.Fatalf("la 121 en hv: quiero 507 que nombre Hypervisor.framework, got %v", err)
	}
	m.naciendo = 0
	if err := m.checkCupoHV(); !api.IsInsufficientMemory(err) {
		t.Fatalf("despertar la 121 en hv: quiero 507, got %v", err)
	}
	// El cupo se cambia con KLING_HV_MAX_VMS; 0 lo apaga.
	t.Setenv("KLING_HV_MAX_VMS", "200")
	if err := m.checkCupoHV(); err != nil {
		t.Fatalf("con KLING_HV_MAX_VMS=200: %v", err)
	}
	t.Setenv("KLING_HV_MAX_VMS", "0")
	if err := m.checkCupoHV(); err != nil {
		t.Fatalf("con KLING_HV_MAX_VMS=0: %v", err)
	}
	// Con vz no hay cupo propio.
	t.Setenv("KLING_HV_MAX_VMS", "")
	backendVMM = BackendVZ
	if err := m.checkCupoHV(); err != nil {
		t.Fatalf("con vz: %v", err)
	}
}
