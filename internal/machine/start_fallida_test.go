package machine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// fallidaConVolcado convierte la parada de paradaParaStart en una failed que
// conserva su volcado entero, sellado después de escribir su disco.
func fallidaConVolcado(t *testing.T, m *Manager, mc *api.Machine) time.Time {
	t.Helper()
	dir := m.dir(mc.ID)
	for _, f := range []string{"snap.file", "mem.file"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("volcado "+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatal(err)
	}
	if err := sellarVolcado(dir, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	cuando := time.Now().Add(-time.Hour).Truncate(time.Second)
	m.mu.Lock()
	live := m.byID[mc.ID]
	live.State, live.LastErr, live.FailedAt, live.StoppedAt = api.StateFailed, errProcesoDesaparecido, &cuando, nil
	m.persist()
	m.mu.Unlock()
	return cuando
}

// Un start de una failed con su volcado que falla antes de lanzar el VMM la
// deja failed, con su error y su fecha, y el volcado sigue valiendo para un
// thaw. Parada, el volcado quedaba en disco sin que nadie pudiera cargarlo.
func TestStartFallidoDeUnaFailedConservaSuVolcado(t *testing.T) {
	m := newTestManager(t)
	m.fcBin = filepath.Join(t.TempDir(), "no-hay-vmm")
	mc, _ := paradaParaStart(t, m, "5a5a00000000000f")
	cuando := fallidaConVolcado(t, m, mc)
	ordenE2fsck = func(ctx context.Context, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo roto; exit 4")
	}

	_, err := m.Start(context.Background(), mc.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "checking its disk") || !strings.Contains(err.Error(), "kling thaw") {
		t.Fatalf("Start = %v; quería el error del disco y que puede descongelarse", err)
	}
	viva := vivaDe(t, m, mc.ID)
	if viva.State != api.StateFailed || viva.LastErr != errProcesoDesaparecido ||
		viva.FailedAt == nil || !viva.FailedAt.Equal(cuando) || viva.PID != 0 {
		t.Fatalf("tras el fallo: estado %s, LastErr %q, FailedAt %v; quería failed como estaba (%v)",
			viva.State, viva.LastErr, viva.FailedAt, cuando)
	}
	if err := m.thawFallida(&viva); err != nil {
		t.Fatalf("el volcado ya no se puede descongelar: %v", err)
	}
	if p := pendiente(m); p != 0 {
		t.Fatalf("pendingMiB = %d", p)
	}
}

// Si el fallo llega con el VMM ya lanzado, el volcado ya se retiró (el
// invitado pudo escribir en su disco) y la máquina queda parada: arrancar en
// frío sigue borrando el volcado viejo antes de que el invitado arranque.
func TestStartFallidoConElVMMLanzadoBorraElVolcado(t *testing.T) {
	m := newTestManager(t)
	m.fcBin = filepath.Join(t.TempDir(), "no-hay-vmm")
	mc, _ := paradaParaStart(t, m, "5a5a00000000001f")
	fallidaConVolcado(t, m, mc)

	if _, err := m.Start(context.Background(), mc.ID, nil); err == nil {
		t.Fatal("Start sin VMM no falló")
	} else if strings.Contains(err.Error(), "kling thaw") {
		t.Fatalf("Start = %v; sin volcado no puede ofrecer el thaw", err)
	}
	viva := vivaDe(t, m, mc.ID)
	if viva.State != api.StateStopped || viva.LastErr == "" {
		t.Fatalf("tras el fallo: estado %s, LastErr %q; quería stopped con el motivo", viva.State, viva.LastErr)
	}
	for _, f := range []string{"snap.file", "mem.file", marcaOK} {
		if _, err := os.Stat(filepath.Join(m.dir(mc.ID), f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s sigue ahí con el VMM ya lanzado (%v)", f, err)
		}
	}
}
