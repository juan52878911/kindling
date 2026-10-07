package main

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// El primer thaw de una copia en diferencial puede pasar casi todo
// construyendo su memoria (almacén, espejo): `kling thaw` lo dice aparte en
// vez de esconderlo en "other".
func TestWakeNoteDiceLaMemoria(t *testing.T) {
	got := wakeNote(&api.WakePhases{Tier: "frozen", TotalMS: 300, LoadMS: 5, StoreMS: 10, MirrorMS: 200, MemoryMS: 40, WaitMS: 1})
	if !strings.Contains(got, "memory 250.0 (store 10.0, mirror 200.0)") || !strings.Contains(got, "other 1.0") {
		t.Errorf("con memoria: %q", got)
	}
	if got := wakeNote(&api.WakePhases{Tier: "frozen", TotalMS: 100, WaitMS: 2}); strings.Contains(got, "memory") {
		t.Errorf("sin memoria que construir: %q", got)
	}
}
