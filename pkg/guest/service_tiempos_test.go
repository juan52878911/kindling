package guest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Cada línea del log del servicio lleva la hora en que llegó, también si
// llega en trozos; sin ?timestamps=1 se quitan.
func TestLogConTiempos(t *testing.T) {
	p := filepath.Join(t.TempDir(), "service.log")
	reloj := time.Date(2026, 10, 28, 11, 0, 0, 123456000, time.UTC)
	l := &rotatingLog{path: p, max: 1 << 20, ahora: func() time.Time { return reloj }}
	for _, trozo := range []string{"uno\ndo", "s\n", "tres\ncua"} {
		if n, _ := l.Write([]byte(trozo)); n != len(trozo) {
			t.Fatalf("Write devolvió %d de %d", n, len(trozo))
		}
		reloj = reloj.Add(time.Second)
	}
	l.Close()
	b, _ := os.ReadFile(p)
	want := "2026-10-28T11:00:00.123456Z uno\n2026-10-28T11:00:00.123456Z dos\n2026-10-28T11:00:02.123456Z tres\n2026-10-28T11:00:02.123456Z cua"
	if string(b) != want {
		t.Fatalf("log:\n%s\nwant:\n%s", b, want)
	}
	if got := quitarTiempos(string(b)); got != "uno\ndos\ntres\ncua" {
		t.Fatalf("sin tiempos: %q", got)
	}
	// La primera línea de una cola por bytes viene cortada, sin marca.
	if got := quitarTiempos("Z dos\n2026-10-28T11:00:02.123456Z tres\n"); got != "Z dos\ntres\n" {
		t.Fatalf("cortada: %q", got)
	}
}
