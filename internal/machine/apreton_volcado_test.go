package machine

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// globoFalso es un Firecracker que solo sabe de su globo: contesta las
// estadísticas que se le den y, al inflarlo, el "invitado" entrega al instante.
type globoFalso struct {
	mu      sync.Mutex
	stats   *fc.BalloonStats // nil: sin globo (404)
	patches []int
}

func (g *globoFalso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.URL.Path == "/balloon/statistics":
		if g.stats == nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"fault_message": "Balloon device not configured"})
			return
		}
		_ = json.NewEncoder(w).Encode(g.stats)
	case r.Method == http.MethodPatch && r.URL.Path == "/balloon":
		var p struct {
			Amount int `json:"amount_mib"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &p)
		g.patches = append(g.patches, p.Amount)
		if g.stats != nil {
			g.stats.ActualMiB, g.stats.TargetMiB = p.Amount, p.Amount
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func nuevoGloboFalso(t *testing.T, stats *fc.BalloonStats) (*globoFalso, *fc.Client) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kglobo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aquí: %v", err)
	}
	g := &globoFalso{stats: stats}
	srv := &http.Server{Handler: g}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	return g, fc.New(sock)
}

// Antes de volcar, el invitado entrega lo que tiene disponible (libre más
// caché) menos el colchón, y el globo vuelve a la línea base para que el
// volcado no lo lleve inflado.
func TestApretonAntesDeVolcarEntregaYDesinfla(t *testing.T) {
	if globoSinEstadisticas {
		t.Skip("en esta plataforma el apretón previo al volcado está apagado")
	}
	t.Setenv("KLING_SQUEEZE_BEFORE_DUMP", "")
	g, c := nuevoGloboFalso(t, &fc.BalloonStats{FreeMemory: 300 << 20, AvailableMemory: 900 << 20, TotalMemory: 2048 << 20})
	mc := &api.Machine{ID: "0123456789abcdef", MemMiB: 2048}
	n := apretarAntesDeVolcar(context.Background(), c, mc)
	want := 900 - balloonSqueezeMarginMiB
	if n != want {
		t.Fatalf("entregados %d MiB, esperaba %d (disponible menos colchón)", n, want)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.patches) != 2 || g.patches[0] != want || g.patches[1] != 0 {
		t.Fatalf("globo: %v, esperaba inflar a %d y volver a 0", g.patches, want)
	}
}

// Con techo (mem_max) la línea base del globo es la diferencia, no 0: así
// las copias del dorado no ven el techo entero.
func TestApretonAntesDeVolcarRespetaElTecho(t *testing.T) {
	if globoSinEstadisticas {
		t.Skip("en esta plataforma el apretón previo al volcado está apagado")
	}
	t.Setenv("KLING_SQUEEZE_BEFORE_DUMP", "")
	g, c := nuevoGloboFalso(t, &fc.BalloonStats{ActualMiB: 512, FreeMemory: 400 << 20, AvailableMemory: 400 << 20})
	mc := &api.Machine{ID: "0123456789abcdef", MemMiB: 1024, MemMaxMiB: 1536}
	if n := apretarAntesDeVolcar(context.Background(), c, mc); n != 400-balloonSqueezeMarginMiB {
		t.Fatalf("entregados %d MiB", n)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.patches) != 2 || g.patches[1] != 512 {
		t.Fatalf("globo: %v, esperaba volver a la línea base 512", g.patches)
	}
}

// Sin globo (imagen anterior a él), sin nada que reclamar, o apagado por
// entorno: el volcado de siempre, sin tocar el globo.
func TestApretonAntesDeVolcarSinGloboNoHaceNada(t *testing.T) {
	if globoSinEstadisticas {
		t.Skip("en esta plataforma el apretón previo al volcado está apagado")
	}
	mc := &api.Machine{ID: "0123456789abcdef", MemMiB: 1024}
	t.Setenv("KLING_SQUEEZE_BEFORE_DUMP", "")
	g, c := nuevoGloboFalso(t, nil)
	if n := apretarAntesDeVolcar(context.Background(), c, mc); n != 0 {
		t.Fatalf("sin globo entregó %d MiB", n)
	}
	g2, c2 := nuevoGloboFalso(t, &fc.BalloonStats{FreeMemory: 32 << 20, AvailableMemory: 64 << 20})
	if n := apretarAntesDeVolcar(context.Background(), c2, mc); n != 0 {
		t.Fatalf("con menos que el colchón entregó %d MiB", n)
	}
	t.Setenv("KLING_SQUEEZE_BEFORE_DUMP", "0")
	g3, c3 := nuevoGloboFalso(t, &fc.BalloonStats{FreeMemory: 800 << 20, AvailableMemory: 800 << 20})
	if n := apretarAntesDeVolcar(context.Background(), c3, mc); n != 0 {
		t.Fatalf("apagado y entregó %d MiB", n)
	}
	for i, g := range []*globoFalso{g, g2, g3} {
		g.mu.Lock()
		if len(g.patches) != 0 {
			t.Fatalf("caso %d: el globo se tocó: %v", i, g.patches)
		}
		g.mu.Unlock()
	}
}

// Una copia recién restaurada puede traer un actual_mib obsoleto (el inflado
// de un apretón anterior, ya desinflado): sumarle lo disponible pasaba del
// total y el VMM contestaba 400. El objetivo se acota al total del invitado.
func TestApretonAntesDeVolcarAcotaUnActualObsoleto(t *testing.T) {
	if globoSinEstadisticas {
		t.Skip("en esta plataforma el apretón previo al volcado está apagado")
	}
	t.Setenv("KLING_SQUEEZE_BEFORE_DUMP", "")
	g, c := nuevoGloboFalso(t, &fc.BalloonStats{ActualMiB: 305, FreeMemory: 355 << 20, AvailableMemory: 433 << 20, TotalMemory: 481 << 20})
	mc := &api.Machine{ID: "0123456789abcdef", MemMiB: 512}
	apretarAntesDeVolcar(context.Background(), c, mc)
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.patches) != 2 || g.patches[0] != 481-balloonSqueezeMarginMiB || g.patches[1] != 0 {
		t.Fatalf("globo: %v, esperaba inflar a %d (total menos colchón) y volver a 0", g.patches, 481-balloonSqueezeMarginMiB)
	}
}
