//go:build linux

package machine

import (
	"errors"
	"os"
	"testing"
	"time"
)

// sondaE2fsDePrueba deja la sonda de la jaula sin estado y con probar como
// prueba, y lo restaura al acabar el test.
func sondaE2fsDePrueba(t *testing.T, probar func() error, reintento time.Duration) {
	t.Helper()
	sondaE2fsMu.Lock()
	ok, e, cuando := sondaE2fsOK, sondaE2fsErr, sondaE2fsCuando
	p, r := probarJaulaE2fs, reintentoSondaE2fs
	sondaE2fsOK, sondaE2fsErr, sondaE2fsCuando = false, nil, time.Time{}
	probarJaulaE2fs, reintentoSondaE2fs = probar, reintento
	sondaE2fsMu.Unlock()
	t.Cleanup(func() {
		sondaE2fsMu.Lock()
		sondaE2fsOK, sondaE2fsErr, sondaE2fsCuando = ok, e, cuando
		probarJaulaE2fs, reintentoSondaE2fs = p, r
		sondaE2fsMu.Unlock()
	})
}

// M15: un fallo pasajero de la sonda no deja e2fsck sin chroot hasta
// reiniciar el daemon. Dentro del plazo se devuelve el fallo sin repetir la
// sonda; pasado, se vuelve a probar; y un éxito ya no se vuelve a probar.
func TestSondaE2fsReintentaTrasUnFallo(t *testing.T) {
	llamadas, falla := 0, true
	sondaE2fsDePrueba(t, func() error {
		llamadas++
		if falla {
			return errors.New("tmpfs: cannot allocate memory")
		}
		return nil
	}, time.Hour)

	if sondaJaulaE2fs() == nil {
		t.Fatal("la sonda falló y sondaJaulaE2fs dijo que no")
	}
	if sondaJaulaE2fs() == nil || llamadas != 1 {
		t.Fatalf("dentro del plazo: quería el mismo fallo sin repetir la sonda; %d llamadas", llamadas)
	}

	falla = false
	sondaE2fsMu.Lock()
	reintentoSondaE2fs = 0
	sondaE2fsMu.Unlock()
	if err := sondaJaulaE2fs(); err != nil {
		t.Fatalf("pasado el plazo y con la jaula ya montable, sigue degradada: %v", err)
	}
	if llamadas != 2 {
		t.Fatalf("quería 2 llamadas a la sonda, hubo %d", llamadas)
	}

	falla = true
	if err := sondaJaulaE2fs(); err != nil || llamadas != 2 {
		t.Fatalf("tras un éxito volvió a probar (%d llamadas) o falló: %v", llamadas, err)
	}
	// Lo que dice kling doctor (sin root no se consulta la sonda).
	if os.Geteuid() == 0 {
		if got := confinamientoE2fs(); got != "chroot+nobody+namespaces" {
			t.Fatalf("e2fs_confinement tras el éxito: %q", got)
		}
	}
}
