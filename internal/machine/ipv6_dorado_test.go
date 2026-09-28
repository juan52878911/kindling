package machine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Pruebas de F2: un dorado congelado antes de la barrera IPv6
// (internal/net/net.go BootArg, internal/net/firewall.go applyIPv6Barrier) no
// lleva `ipv6.disable=1` en el kernel que se congeló con su memoria. El campo
// GuestIPv6Off distingue los dorados nuevos de los viejos ("no consta"), y
// runFrom avisa una vez por dorado cuando falta. La barrera del namespace
// (applyIPv6Barrier) sigue cerrando el paso igual en ambos casos: esto es
// diagnóstico, no un límite de seguridad nuevo.

// TestCommitMarcaGuestIPv6OffEnArranqueEnFrio: una plantilla que arrancó en
// frío (sin From) ya tiene `ipv6.disable=1` en su línea de arranque con este
// binario, así que su dorado sale marcado.
func TestCommitMarcaGuestIPv6OffEnArranqueEnFrio(t *testing.T) {
	if _, err := exec.LookPath("cp"); err != nil {
		t.Skip("sin cp no se puede copiar el overlay")
	}
	m := newTestManager(t)
	id := "c3aa170000000001"
	falso, _ := plantillaParaCommit(t, m, id)

	dir := m.snapDir("dorado")
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			_ = os.WriteFile(filepath.Join(dir, "snap.file"), []byte("estado"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "mem.file"), make([]byte, 1<<20), 0o644)
		}
	})

	snap, err := m.Commit(context.Background(), id, "dorado", false)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !snap.GuestIPv6Off {
		t.Error("una plantilla arrancada en frío debería congelar GuestIPv6Off = true")
	}
	// Y lo que queda en disco lo lleva también, no solo la copia en memoria.
	got, err := m.loadSnapshot("dorado")
	if err != nil {
		t.Fatal(err)
	}
	if !got.GuestIPv6Off {
		t.Error("meta.json en disco: guest_ipv6_off no quedó grabado")
	}
}

// TestCommitHeredaGuestIPv6OffDeLaCadena: si la plantilla es a su vez una
// instancia de otro dorado (fork), no volvió a arrancar en frío, así que su
// línea de arranque es la que traía AQUEL: se hereda su marca, no se supone.
func TestCommitHeredaGuestIPv6OffDeLaCadena(t *testing.T) {
	if _, err := exec.LookPath("cp"); err != nil {
		t.Skip("sin cp no se puede copiar el overlay")
	}
	for _, caso := range []struct {
		nombre   string
		origen   bool
		esperado bool
	}{
		{"origen sin la barrera (viejo)", false, false},
		{"origen ya con la barrera", true, true},
	} {
		t.Run(caso.nombre, func(t *testing.T) {
			m := newTestManager(t)
			escribirSnapshot(t, m, "origen", api.Snapshot{GuestIPv6Off: caso.origen})

			id := "c4aa170000000001"
			falso, _ := plantillaParaCommit(t, m, id)
			m.mu.Lock()
			m.byID[id].From = "origen"
			m.mu.Unlock()

			dir := m.snapDir("fork")
			falso.enGancho(func(metodo, ruta string) {
				if ruta == "/snapshot/create" {
					_ = os.WriteFile(filepath.Join(dir, "snap.file"), []byte("estado"), 0o644)
					_ = os.WriteFile(filepath.Join(dir, "mem.file"), make([]byte, 1<<20), 0o644)
				}
			})

			snap, err := m.Commit(context.Background(), id, "fork", false)
			if err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if snap.GuestIPv6Off != caso.esperado {
				t.Errorf("GuestIPv6Off = %v, quería %v (heredado de origen)", snap.GuestIPv6Off, caso.esperado)
			}
		})
	}
}

// TestAvisoIPv6Invitado: solo avisa si falta la marca, y solo una vez por
// nombre de dorado (sync.Map, igual que resyncAvisado).
func TestAvisoIPv6Invitado(t *testing.T) {
	m := newTestManager(t)

	if aviso := m.avisoIPv6Invitado("dorado", true); aviso != "" {
		t.Errorf("con GuestIPv6Off no debería avisar: %q", aviso)
	}
	aviso := m.avisoIPv6Invitado("dorado", false)
	if aviso == "" {
		t.Fatal("sin GuestIPv6Off debería avisar la primera vez")
	}
	if aviso := m.avisoIPv6Invitado("dorado", false); aviso != "" {
		t.Errorf("el mismo dorado no debería avisar dos veces: %q", aviso)
	}
	// Otro dorado sin la marca sí avisa: el aviso es por nombre, no global.
	if aviso := m.avisoIPv6Invitado("otro", false); aviso == "" {
		t.Error("un dorado distinto debería avisar por su cuenta")
	}
}

// TestRunFromAvisaIPv6UnaVezPorDorado: runFrom publica el evento la primera
// vez que instancia un dorado sin GuestIPv6Off, y no lo repite en la
// siguiente instanciación del mismo. No hace falta que runFrom llegue a
// arrancar de verdad: el aviso se emite antes de copiar el overlay, así que
// basta con que el snapshot pase la comprobación de integridad (sin digests
// grabados, se salta) para llegar hasta ahí.
func TestRunFromAvisaIPv6UnaVezPorDorado(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	m := newTestManager(t)
	m.bus = events.New()
	escribirSnapshot(t, m, "dorado", api.Snapshot{}) // GuestIPv6Off por defecto: false ("no consta")

	eventos, cancelar := m.bus.Subscribe()
	defer cancelar()

	if _, err := m.runFrom(context.Background(), api.RunRequest{From: "dorado"}); err == nil {
		t.Fatal("se esperaba un error más adelante (sin overlay.ext4 real que copiar)")
	}
	select {
	case ev := <-eventos:
		if ev.Type != api.EvGuestIPv6 || ev.Name != "dorado" {
			t.Fatalf("evento inesperado: %+v", ev)
		}
	default:
		t.Fatal("se esperaba el evento snapshot.guest_ipv6 en la primera instanciación")
	}

	if _, err := m.runFrom(context.Background(), api.RunRequest{From: "dorado"}); err == nil {
		t.Fatal("se esperaba el mismo error de nuevo")
	}
	select {
	case ev := <-eventos:
		t.Fatalf("el aviso se repitió en la segunda instanciación: %+v", ev)
	default:
	}
}

// Un dorado con GuestIPv6Off no publica nada.
func TestRunFromNoAvisaSiGuestIPv6Off(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	m := newTestManager(t)
	m.bus = events.New()
	escribirSnapshot(t, m, "dorado", api.Snapshot{GuestIPv6Off: true})

	eventos, cancelar := m.bus.Subscribe()
	defer cancelar()

	if _, err := m.runFrom(context.Background(), api.RunRequest{From: "dorado"}); err == nil {
		t.Fatal("se esperaba un error más adelante (sin overlay.ext4 real que copiar)")
	}
	select {
	case ev := <-eventos:
		t.Fatalf("no debería haber evento con GuestIPv6Off: %+v", ev)
	default:
	}
}
