package share

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	proto "github.com/juan52878911/kindling/internal/shareproto"
)

// Un OpSetattr con handle y un OpRelease del mismo handle van a la vez: el
// descriptor se cierra (session.release) mientras setattr le pone las fechas.
// Con f.Fd() el número podía ser ya el de otro fichero recién abierto, y las
// fechas iban a parar a él.
func TestSetattrConReleaseALaVez(t *testing.T) {
	dir := t.TempDir()
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	marca := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	otro := filepath.Join(t.TempDir(), "b")
	for i := 0; i < 3000; i++ {
		fa, err := os.OpenFile(a, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(otro, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Puede fallar (el handle ya no está); lo que no puede es tocar otro.
			_, _ = srv.setattr("a", fa, proto.SetAtime|proto.SetMtime, 0, 0, marca, marca)
		}()
		_ = fa.Close()
		fb, err := os.OpenFile(otro, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		fi, err := fb.Stat()
		fb.Close()
		if err != nil {
			t.Fatal(err)
		}
		if fi.ModTime().UnixNano() == marca {
			t.Fatalf("iteration %d: setattr on a released handle changed another file's times", i)
		}
	}
}
