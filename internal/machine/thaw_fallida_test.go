package machine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// fallidaParaThaw: la failed de fallidaConVolcado, con el disco anterior
// al sello para que thawFallida la admita.
func fallidaParaThaw(t *testing.T, m *Manager, id string) *api.Machine {
	t.Helper()
	mc, _ := paradaParaStart(t, m, id)
	fallidaConVolcado(t, m, mc)
	antes := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(m.dir(mc.ID), "overlay.ext4"), antes, antes); err != nil {
		t.Fatal(err)
	}
	return mc
}

// A-02: una failed no cuenta como usuaria de sus volúmenes y sigue failed
// durante todo su thaw. Si readmitirlos no deja una reserva, otro arranque
// que pida el mismo volumen en escritura pasa mientras dura el thaw, y
// acaban dos VMM escribiendo el mismo ext4.
func TestThawDeUnaFailedReservaSusVolumenes(t *testing.T) {
	m := conVolumen(t, "datos")
	mc := fallidaParaThaw(t, m, "5a5a0000000000c1")
	m.mu.Lock()
	m.byID[mc.ID].Volumes = []api.VolumeAttachment{{Name: "datos", Mount: "/data"}}
	m.mu.Unlock()

	// La red se monta en medio del thaw, con los volúmenes ya readmitidos:
	// ahí se prueba otro arranque con el mismo volumen.
	req := api.RunRequest{Volume: "datos"}
	var probado bool
	var errOtro error
	cambiarGanchosRed(t, func(g *ganchosRed) {
		previo := g.montar
		g.montar = func(n *knet.Net, e knet.Egress, d []string, uid int) error {
			if !probado {
				probado = true
				otro := newID()
				_, errOtro = m.reservarVolumenes(req, otro, "otra")
				m.soltarReservas(otro)
			}
			return previo(n, e, d, uid)
		}
	})

	if _, err := m.Thaw(context.Background(), mc.ID); err == nil || !strings.Contains(err.Error(), "launching") {
		t.Fatalf("Thaw = %v; quería que llegara a lanzar el VMM (y fallara sin él)", err)
	}
	if !probado {
		t.Fatal("el thaw no llegó a montar la red: la prueba no miró la ventana")
	}
	if errOtro == nil {
		t.Fatal("durante el thaw de una failed, otro arranque tomó su volumen en escritura")
	}
	// Acabado el thaw (aquí fallido), la reserva se suelta.
	if _, err := m.reservarVolumenes(req, newID(), "despues"); err != nil {
		t.Fatalf("tras el thaw el volumen sigue reservado: %v", err)
	}
}

// A-03: una failed no cuenta en la cuota de su dueño; volver a la vida
// desde su volcado tiene que pasar por ella, como Start (reclamarParada).
func TestThawDeUnaFailedPasaPorLaCuota(t *testing.T) {
	m := newTestManager(t)
	mc := fallidaParaThaw(t, m, "5a5a0000000000c2")
	m.mu.Lock()
	m.byID[mc.ID].Labels = map[string]string{api.LabelOwner: "a"}
	m.mu.Unlock()
	m.SetCuotas(cuotasDePrueba(Cuota{Maquinas: 1, MemMiB: SinTope, DiscoMiB: SinTope}))
	if err := m.publicar(maquinaDe("a", api.StateRunning, 64)); err != nil {
		t.Fatal(err)
	}

	_, err := m.Thaw(context.Background(), mc.ID)
	if !api.IsTenantQuota(err) {
		t.Fatalf("Thaw de una failed con el dueño en su tope = %v; quería el error de cuota", err)
	}
	if got := vivaDe(t, m, mc.ID).State; got != api.StateFailed {
		t.Fatalf("estado = %s; rechazada por cuota tiene que seguir failed", got)
	}
}
