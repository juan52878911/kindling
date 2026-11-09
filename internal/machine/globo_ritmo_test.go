package machine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// En reposo el globo reporta cada 10 s: a 1 Hz eran ~86 000 despertares al día
// por máquina que nadie leía.
func TestGloboArrancaConEstadisticasEnReposo(t *testing.T) {
	if balloonStatsPollSec < 10 {
		t.Fatalf("balloonStatsPollSec = %d, quería >= 10", balloonStatsPollSec)
	}
	f := nuevoFcFalso(t)
	m := &Manager{}
	if err := m.configurarGlobo(context.Background(), f.cliente(), "abcdef0123456789", 0); err != nil {
		t.Fatal(err)
	}
	cs := cuerposBalloon(t, f)
	if len(cs) == 0 || cs[len(cs)-1]["stats_polling_interval_s"] != float64(balloonStatsPollSec) {
		t.Fatalf("PUT /balloon = %v, quería stats_polling_interval_s=%d", cs, balloonStatsPollSec)
	}
}

// Las estadísticas de reposo pueden tener 10 s. El invitado, en el falso, tenía
// 100 MiB disponibles hace un intervalo y 900 ahora: squeeze tiene que reclamar
// según el informe que pide al bajar el intervalo, como cuando reportaba cada
// segundo, y dejarlo luego otra vez en reposo.
func TestSqueezeLeeEstadisticasFrescas(t *testing.T) {
	if globoSinEstadisticas {
		t.Skip("en esta plataforma el globo no da estadísticas")
	}
	viejas := &fc.BalloonStats{FreeMemory: 100 << 20, AvailableMemory: 100 << 20, TotalMemory: 2048 << 20, MinorFaults: 10}
	frescas := &fc.BalloonStats{FreeMemory: 300 << 20, AvailableMemory: 900 << 20, TotalMemory: 2048 << 20, MinorFaults: 11}
	g, sock := nuevoGloboFalsoEn(t, viejas)
	g.fresco = frescas

	m := &Manager{byID: map[string]*api.Machine{}, socket: map[string]string{}}
	id := "abcdef0123456789"
	m.byID[id] = &api.Machine{ID: id, Name: "pg", State: api.StateRunning, MemMiB: 2048}
	m.socket[id] = sock
	res, err := m.squeezeLocked(context.Background(), id, id, false, balloonSqueezeMarginMiB, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.GuestFreeMiB != 300 {
		t.Errorf("GuestFreeMiB = %d, quería 300 (el informe fresco)", res.GuestFreeMiB)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if want := 900 - balloonSqueezeMarginMiB; len(g.patches) == 0 || g.patches[0] != want {
		t.Errorf("globo: %v, quería inflar a %d (disponible fresco menos colchón)", g.patches, want)
	}
	if !slices.Equal(g.intervalos, []int{balloonStatsPollSecApreton, balloonStatsPollSec}) {
		t.Errorf("intervalos = %v, quería %d al apretar y %d al acabar", g.intervalos, balloonStatsPollSecApreton, balloonStatsPollSec)
	}
}

// El apretón de antes de volcar, igual: y vuelve al reposo ANTES de volcar,
// porque el intervalo queda grabado en el snapshot que heredan las copias.
func TestApretonAntesDeVolcarLeeEstadisticasFrescas(t *testing.T) {
	if globoSinEstadisticas {
		t.Skip("en esta plataforma el apretón previo al volcado está apagado")
	}
	t.Setenv("KLING_SQUEEZE_BEFORE_DUMP", "")
	g, c := nuevoGloboFalso(t, &fc.BalloonStats{FreeMemory: 50 << 20, AvailableMemory: 50 << 20, TotalMemory: 2048 << 20})
	g.fresco = &fc.BalloonStats{FreeMemory: 300 << 20, AvailableMemory: 900 << 20, TotalMemory: 2048 << 20}
	mc := &api.Machine{ID: "0123456789abcdef", MemMiB: 2048}
	if n, want := apretarAntesDeVolcar(context.Background(), c, mc), 900-balloonSqueezeMarginMiB; n != want {
		t.Fatalf("entregados %d MiB, quería %d", n, want)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.Equal(g.intervalos, []int{balloonStatsPollSecApreton, balloonStatsPollSec}) {
		t.Errorf("intervalos = %v", g.intervalos)
	}
}

// Si el invitado no contesta al informe pedido, se aprieta con lo último que
// reportó, sin esperar más que plazoEstadisticasFrescas.
func TestEstadisticasFrescasNoEsperanDeMas(t *testing.T) {
	if globoSinEstadisticas {
		t.Skip("en esta plataforma el globo no da estadísticas")
	}
	g, c := nuevoGloboFalso(t, &fc.BalloonStats{FreeMemory: 100 << 20, TotalMemory: 1024 << 20})
	inicio := time.Now()
	s, volver, err := estadisticasFrescas(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	volver()
	if d := time.Since(inicio); d > plazoEstadisticasFrescas+500*time.Millisecond {
		t.Errorf("tardó %v", d)
	}
	if s.FreeMemory != 100<<20 {
		t.Errorf("FreeMemory = %d", s.FreeMemory)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.Equal(g.intervalos, []int{balloonStatsPollSecApreton, balloonStatsPollSec}) {
		t.Errorf("intervalos = %v", g.intervalos)
	}
}

// Un dorado o un volcado de antes trae 1 s en el snapshot: al restaurar se deja
// en el de reposo.
func TestRitmoGloboReposo(t *testing.T) {
	if globoSinEstadisticas {
		t.Skip("en esta plataforma el globo no da estadísticas")
	}
	g, c := nuevoGloboFalso(t, &fc.BalloonStats{})
	ritmoGloboReposo(context.Background(), c)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.Equal(g.intervalos, []int{balloonStatsPollSec}) {
		t.Errorf("intervalos = %v", g.intervalos)
	}
}
