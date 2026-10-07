package machine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// agenteVolumenes es el agente del invitado con un código fijo por operación
// de volumen (200 si no se dice otro); apunta cada /volume/<op>.
func agenteVolumenes(t *testing.T, codigos map[string]int) (addr string, ops func() []string) {
	t.Helper()
	var mu sync.Mutex
	var vistos []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op, ok := strings.CutPrefix(r.URL.Path, "/volume/")
		if !ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		vistos = append(vistos, op)
		mu.Unlock()
		if c := codigos[op]; c != 0 {
			http.Error(w, "mount: /data: no such device", c)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), vistos...)
	}
}

func ponerVolumenPlantilla(m *Manager, id, addr string) {
	m.mu.Lock()
	viva := m.byID[id]
	viva.Volumes = []api.VolumeAttachment{{Name: "datos", Mount: "/data"}}
	viva.Forwards = map[string]string{"8080": addr}
	m.mu.Unlock()
}

// Commit soltó los volúmenes y no se los pudo devolver: la plantilla no sigue
// en marcha escribiendo en su overlay. Queda fallida y dicho por qué.
func TestCommitSinPoderDevolverLosVolumenesMarcaFallida(t *testing.T) {
	m := newTestManager(t)
	id := "c0aa170000000011"
	falso, muerto := plantillaParaCommit(t, m, id)
	addr, ops := agenteVolumenes(t, map[string]int{"acquire": http.StatusInternalServerError})
	ponerVolumenPlantilla(m, id, addr)
	falso.fallar(http.MethodPatch, "/vm", http.StatusBadRequest, "vcpu busy")

	if _, err := m.Commit(context.Background(), id, "dorado", false); err == nil {
		t.Fatal("Commit con la pausa rota no devolvió error")
	}
	// Tras el acquire fallido, fail mata el VMM, que antes pide un vaciado
	// (sync): lo que venga detrás no cuenta.
	if got := ops(); len(got) < 2 || got[0] != "release" || got[1] != "acquire" {
		t.Fatalf("operaciones de volumen = %v, quería release y acquire", got)
	}
	v := vivaDe(t, m, id)
	if v.State != api.StateFailed || !strings.Contains(v.LastErr, "volumes back") {
		t.Errorf("plantilla %s / %q, quería failed diciendo que perdió sus volúmenes", v.State, v.LastErr)
	}
	if sigueVivo(muerto) {
		t.Error("el VMM de una plantilla sin volúmenes sigue vivo")
	}
}

// Lo mismo en el camino feliz: el dorado vale (se volcó con los volúmenes
// soltados) y el commit acaba bien, pero la plantilla queda fallida.
func TestCommitCompletoSinVolumenesDevueltosConservaElDorado(t *testing.T) {
	if _, err := exec.LookPath("fallocate"); err != nil {
		t.Skip("sin fallocate no se puede perforar el volcado")
	}
	m := newTestManager(t)
	id := "c0aa170000000012"
	falso, muerto := plantillaParaCommit(t, m, id)
	addr, _ := agenteVolumenes(t, map[string]int{"acquire": http.StatusInternalServerError})
	ponerVolumenPlantilla(m, id, addr)
	dir := m.snapDir("dorado")
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			_ = os.WriteFile(filepath.Join(dir, "snap.file"), []byte("estado"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "mem.file"), make([]byte, 1<<20), 0o644)
		}
	})
	if _, err := m.Commit(context.Background(), id, "dorado", false); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		t.Errorf("el dorado no quedó: %v", err)
	}
	if v := vivaDe(t, m, id); v.State != api.StateFailed || !strings.Contains(v.LastErr, "volumes back") {
		t.Errorf("plantilla %s / %q, quería failed diciendo que perdió sus volúmenes", v.State, v.LastErr)
	}
	if sigueVivo(muerto) {
		t.Error("el VMM de una plantilla sin volúmenes sigue vivo")
	}
}

// Un puente que no conoce /volume/release (404) no soltó nada: ni se le pide
// devolverlos ni se da la plantilla por perdida.
func TestCommitConPuenteSinVolumenesNoLaMata(t *testing.T) {
	m := newTestManager(t)
	id := "c0aa170000000013"
	_, muerto := plantillaParaCommit(t, m, id)
	addr, ops := agenteVolumenes(t, map[string]int{"release": http.StatusNotFound, "acquire": http.StatusNotFound})
	ponerVolumenPlantilla(m, id, addr)

	_, err := m.Commit(context.Background(), id, "dorado", false)
	if err == nil || !strings.Contains(err.Error(), "refresh-bridge") {
		t.Fatalf("esperaba el consejo de refresh-bridge, llegó %v", err)
	}
	if got := ops(); strings.Join(got, ",") != "release" {
		t.Errorf("operaciones de volumen = %v, quería solo [release]", got)
	}
	if v := vivaDe(t, m, id); v.State != api.StateRunning || !sigueVivo(muerto) {
		t.Errorf("plantilla %s (vivo=%v), quería running", v.State, sigueVivo(muerto))
	}
}
