package guest

import (
	"testing"
	"time"
)

// Con kling.volwait el servicio no arranca hasta el primer montaje, y un
// segundo montaje no lo vuelve a lanzar.
func TestStartAfterVolumes(t *testing.T) {
	v := &Volumes{}
	lanzado := make(chan struct{}, 2)
	startAfterVolumes(v, func() { lanzado <- struct{}{} })
	select {
	case <-lanzado:
		t.Fatal("started before the volumes were mounted")
	case <-time.After(50 * time.Millisecond):
	}
	v.mu.Lock()
	v.noteAcquiredLocked()
	v.noteAcquiredLocked() // idempotente: no cierra dos veces
	v.mu.Unlock()
	select {
	case <-lanzado:
	case <-time.After(2 * time.Second):
		t.Fatal("not started after the mount")
	}
	// Un montaje anterior a la espera también cuenta.
	w := &Volumes{}
	w.mu.Lock()
	w.noteAcquiredLocked()
	w.mu.Unlock()
	startAfterVolumes(w, func() { lanzado <- struct{}{} })
	select {
	case <-lanzado:
	case <-time.After(2 * time.Second):
		t.Fatal("a mount before the wait was missed")
	}
	if parseVolWait(map[string]string{"kling.volwait": "1"}) != true || parseVolWait(nil) {
		t.Fatal("parseVolWait")
	}
}
