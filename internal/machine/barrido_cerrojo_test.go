package machine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// P9: el volumen (y sus snapshots) se borra sin m.mu: bajo el cerrojo solo
// se aparta a la papelera. Y no queda nada ni en volumes/ ni en la papelera.
func TestBorrarVolumenSinElCerrojo(t *testing.T) {
	m := newTestManager(t)
	escribirVolumen(t, m, "datos", "v1")
	if _, err := m.SnapshotVolume(context.Background(), "datos", "uno"); err != nil {
		t.Fatal(err)
	}
	var borrados, conCerrojo int
	antes := borrarApartado
	borrarApartado = func(p string) error {
		borrados++
		if !m.mu.TryLock() {
			conCerrojo++
		} else {
			m.mu.Unlock()
		}
		return os.RemoveAll(p)
	}
	t.Cleanup(func() { borrarApartado = antes })

	if err := m.RemoveVolume("datos", true); err != nil {
		t.Fatal(err)
	}
	if borrados != 2 || conCerrojo != 0 {
		t.Fatalf("%d borrados, %d con m.mu tomado; quería 2 sin él", borrados, conCerrojo)
	}
	for _, p := range []string{m.volumePath("datos"), m.volSnapsDir("datos")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s sigue: %v", p, err)
		}
	}
	if e, _ := os.ReadDir(filepath.Join(m.root, "machines", papeleraDir)); len(e) != 0 {
		t.Fatalf("la papelera se quedó con %d entradas", len(e))
	}
}

// P9: el escaneo del barrido (ReadDir y dos stat por snapshot) no toma m.mu.
func TestEscaneoDelBarridoSinElCerrojo(t *testing.T) {
	m := newTestManager(t)
	restos := filepath.Join(m.root, "snapshots", "a-medias")
	if err := os.MkdirAll(restos, 0o755); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	hecho := make(chan int)
	go func() {
		_, r := m.escanearBarrido()
		hecho <- len(r)
	}()
	select {
	case n := <-hecho:
		if n != 1 {
			t.Errorf("%d candidatos; quería el commit a medias", n)
		}
	case <-time.After(5 * time.Second):
		t.Error("el escaneo del barrido espera a m.mu")
	}
	m.mu.Unlock()
}

// Medición de P9: cuánto retiene m.mu el barrido de snapshots con 1000
// dorados enteros. "antes", el de siempre (escaneo bajo el cerrojo);
// "ahora", la parte que sigue bajo él.
// go test ./internal/machine -run x -bench BarridoSnapshotsCerrojo
func BenchmarkBarridoSnapshotsCerrojo(b *testing.B) {
	m := &Manager{root: b.TempDir(), reserved: map[string]int{}}
	for i := range 1000 {
		dir := filepath.Join(m.root, "snapshots", fmt.Sprintf("svc-%04d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("{}"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("antes", func(b *testing.B) {
		for b.Loop() {
			m.mu.Lock()
			m.sweepSnapshotLeftovers()
			m.mu.Unlock()
		}
	})
	b.Run("ahora", func(b *testing.B) {
		for b.Loop() {
			c := m.candidatosRestos()
			m.mu.Lock()
			t0 := time.Now()
			m.sweepSnapshotLeftoversDe(c)
			b.ReportMetric(float64(time.Since(t0).Nanoseconds()), "lock_ns")
			m.mu.Unlock()
		}
	})
}
