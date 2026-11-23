package machine

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// vmmConTiempos es un VMM falso que contesta GET /kling/info con cuerpo (vacío
// = la ruta no existe, como Firecracker) y cuenta las peticiones.
func vmmConTiempos(t *testing.T, cuerpo string) (*fc.Client, *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ktiempos")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "vmm.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aquí: %v", err)
	}
	var pedidas atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pedidas.Add(1)
		if r.URL.Path != "/kling/info" || cuerpo == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cuerpo))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	return fc.New(sock), &pedidas
}

// Lo que contesta kling-hv tras una restauración (vz/internal/server).
const infoRestauracion = `{"backend":"hv","version":"x","timings":{"kind":"restore","net_ms":0.4,
"sandbox_ms":1.2,"create_ms":3.1,"restore_ms":2.5,"resume_ms":0.3,"total_ms":7.6,
"vmm":{"kind":"restore","process_ms":4.2,"phases_ms":{"gic":0.8},"wait_ms":0.1,
"since_release_ms":{"agent":9.5},"total_ms":12.3}}}`

// K-03: el desglose del VMM se cuelga del último despertar de la máquina, sin
// tocar el que ya se devolvió, y solo si es de una restauración y sigue siendo
// el último.
func TestTiemposVMMDelDespertar(t *testing.T) {
	m := newTestManager(t)
	id := newID()
	mc := m.addForTest(id)
	fases := &api.WakePhases{Tier: "frozen", LoadMS: 9, TotalMS: 20}
	m.mu.Lock()
	mc.Wake = fases
	m.mu.Unlock()

	c, _ := vmmConTiempos(t, infoRestauracion)
	if !m.anotarTiemposDespertar(context.Background(), c, id, fases) {
		t.Fatal("no anotó el desglose del VMM")
	}
	if fases.VMM != nil {
		t.Fatal("tocó el desglose ya devuelto a quien despertó")
	}
	got, _ := m.Get(id)
	v := got.Wake.VMM
	if v == nil || v.Kind != "restore" || v.RestoreMS != 2.5 || v.SandboxMS != 1.2 || got.Wake.LoadMS != 9 {
		t.Fatalf("Wake = %+v, VMM = %+v", got.Wake, v)
	}
	if v.Inside == nil || v.Inside.ProcessMS != 4.2 || v.Inside.PhasesMS["gic"] != 0.8 || v.Inside.SinceReleaseMS["agent"] != 9.5 {
		t.Fatalf("lo de dentro del VMM: %+v", v.Inside)
	}

	// Un despertar posterior ya es otro: el desglose viejo no lo pisa.
	nuevo := &api.WakePhases{Tier: "frozen"}
	m.mu.Lock()
	m.byID[id].Wake = nuevo
	m.mu.Unlock()
	if m.anotarTiemposDespertar(context.Background(), c, id, fases) {
		t.Fatal("anotó sobre un despertar posterior")
	}

	// Un arranque en frío no describe un despertar; un VMM sin timings
	// (anterior, o Firecracker con su 404) no deja nada.
	frio, _ := vmmConTiempos(t, `{"backend":"vz","timings":{"kind":"boot","total_ms":5}}`)
	viejo, _ := vmmConTiempos(t, `{"backend":"vz"}`)
	sinRuta, _ := vmmConTiempos(t, "")
	for nombre, c := range map[string]*fc.Client{"boot": frio, "sin timings": viejo, "404": sinRuta} {
		if m.anotarTiemposDespertar(context.Background(), c, id, nuevo) {
			t.Fatalf("%s: anotó algo", nombre)
		}
	}
	if got, _ := m.Get(id); got.Wake.VMM != nil {
		t.Fatalf("quedó un desglose: %+v", got.Wake.VMM)
	}
}

func TestTiemposVMMDeLaCreacion(t *testing.T) {
	m := newTestManager(t)
	id := newID()
	mc := m.addForTest(id)
	creacion := &api.CreatePhases{LoadMS: 3, TotalMS: 40}
	m.mu.Lock()
	mc.Create = creacion
	m.mu.Unlock()
	c, _ := vmmConTiempos(t, infoRestauracion)
	if !m.anotarTiemposCreacion(context.Background(), c, id, creacion) {
		t.Fatal("no anotó el desglose del VMM")
	}
	got, _ := m.Get(id)
	if got.Create.VMM == nil || got.Create.VMM.CreateMS != 3.1 || got.Create.LoadMS != 3 || creacion.VMM != nil {
		t.Fatalf("Create = %+v", got.Create)
	}
}

// En segundo plano, y solo donde el VMM los da: en Linux (Firecracker) no se
// le pide nada.
func TestTiemposVMMEnSegundoPlano(t *testing.T) {
	m := newTestManager(t)
	id := newID()
	mc := m.addForTest(id)
	fases := &api.WakePhases{Tier: "frozen"}
	m.mu.Lock()
	mc.Wake = fases
	m.mu.Unlock()
	c, pedidas := vmmConTiempos(t, infoRestauracion)
	m.tiemposVMMTrasDespertar(c, id, fases)
	if !vmmDaTiempos {
		time.Sleep(50 * time.Millisecond)
		if n := pedidas.Load(); n != 0 {
			t.Fatalf("pidió %d veces /kling/info a un VMM que no lo tiene", n)
		}
		return
	}
	plazo := time.Now().Add(5 * time.Second)
	for time.Now().Before(plazo) {
		if got, _ := m.Get(id); got.Wake.VMM != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("el desglose del VMM no llegó")
}
