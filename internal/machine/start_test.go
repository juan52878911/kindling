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

	"github.com/juan52878911/kindling/internal/events"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// paradaParaStart registra una máquina parada con lo que Start mira antes de
// lanzar nada: su overlay, una imagen y un kernel de pega. La red no se toca
// de verdad (montarRedHost/desmontarRedHost falsos, que apuntan lo que se
// monta y desmonta).
func paradaParaStart(t *testing.T, m *Manager, id string) (*api.Machine, *[]string) {
	t.Helper()
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	t.Setenv("KLING_MAX_MEM_PRESSURE", "0")
	t.Setenv("KLING_MAX_SWAP_PCT", "0")
	m.bus = events.New()
	m.priv = &Privileges{}
	img := "prueba-start"
	for _, f := range []string{m.imagePath(img), m.KernelPath()} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("no es un disco"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.dir(id), "overlay.ext4"), []byte("lo que escribió"), 0o644); err != nil {
		t.Fatal(err)
	}
	var red []string
	oldM, oldD := montarRedHost, desmontarRedHost
	t.Cleanup(func() { montarRedHost, desmontarRedHost = oldM, oldD })
	montarRedHost = func(n *knet.Net, _ knet.Egress, _ []string, _ int) error {
		red = append(red, "+"+n.NS)
		return nil
	}
	desmontarRedHost = func(n *knet.Net) { red = append(red, "-"+n.NS) }
	// e2fsck de pega: el overlay de aquí no es un ext4 (ver
	// TestStartRevisaSuDiscoAntesDeArrancar).
	orden := ordenE2fsck
	t.Cleanup(func() { ordenE2fsck = orden })
	ordenE2fsck = func(ctx context.Context, _ string) *exec.Cmd { return exec.CommandContext(ctx, "true") }

	parada := time.Now().Add(-time.Minute)
	mc := &api.Machine{ID: id, Name: "parada-" + id[:4], Image: img, State: api.StateStopped,
		VCPUs: 1, MemMiB: 64, NetIndex: 9, CreatedAt: parada, StoppedAt: &parada}
	m.mu.Lock()
	m.byID[id] = mc
	m.persist()
	m.mu.Unlock()
	return mc, &red
}

// Start exige las claves de su entorno: el daemon solo guardó los nombres, y
// arrancar el servicio sin su contraseña es peor que no arrancar. El error
// dice cuáles faltan y nunca un valor; la máquina sigue parada.
func TestStartExigeLasClavesDeSuEntorno(t *testing.T) {
	m := newTestManager(t)
	mc, _ := paradaParaStart(t, m, "5a5a000000000001")
	m.mu.Lock()
	m.byID[mc.ID].EnvKeys = []string{"DB_PASSWORD", "TOKEN"}
	m.mu.Unlock()

	_, err := m.Start(context.Background(), mc.ID, nil)
	if !errors.Is(err, ErrEnvRequest) || !strings.Contains(err.Error(), "DB_PASSWORD, TOKEN") {
		t.Fatalf("Start sin entorno = %v; quería que nombrase las dos claves", err)
	}
	_, err = m.Start(context.Background(), mc.ID, []string{"DB_PASSWORD=s3cr3t-valor"})
	if !errors.Is(err, ErrEnvRequest) || !strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("Start con una clave = %v; quería que nombrase TOKEN", err)
	}
	if strings.Contains(err.Error(), "s3cr3t-valor") || strings.Contains(err.Error(), "DB_PASSWORD,") {
		t.Fatalf("el error enseña un valor o una clave que sí vino: %v", err)
	}
	if got := vivaDe(t, m, mc.ID).State; got != api.StateStopped {
		t.Fatalf("estado = %s; un entorno incompleto no debe tocar la máquina", got)
	}
}

// Solo se arranca una parada: a una congelada se la manda a thaw, y una que ya
// corre se devuelve tal cual.
func TestStartSoloArrancaParadas(t *testing.T) {
	m := newTestManager(t)
	mc, _ := paradaParaStart(t, m, "5a5a000000000002")
	m.mu.Lock()
	m.byID[mc.ID].State = api.StateWarm
	m.mu.Unlock()
	if _, err := m.Start(context.Background(), mc.ID, nil); err == nil || !strings.Contains(err.Error(), "kling thaw") {
		t.Fatalf("Start de una congelada = %v; quería que remitiera a thaw", err)
	}
	m.mu.Lock()
	m.byID[mc.ID].State = api.StateRunning
	m.mu.Unlock()
	out, err := m.Start(context.Background(), mc.ID, nil)
	if err != nil || out.State != api.StateRunning {
		t.Fatalf("Start de una que corre = %v, %v; quería devolverla", out, err)
	}
	if _, err := m.Start(context.Background(), "no-existe", nil); err == nil || !strings.Contains(err.Error(), "doesn't exist") {
		t.Fatalf("Start de una que no existe = %v", err)
	}
}

// Una parada no cuenta como usuaria de sus volúmenes, así que Start los
// vuelve a reclamar: si otra máquina tiene el volumen en escritura, no
// arranca (dos escritores sobre un ext4 es corrupción) y se queda parada.
func TestStartRespetaAlQueEscribeEnSuVolumen(t *testing.T) {
	m := newTestManager(t)
	mc, _ := paradaParaStart(t, m, "5a5a000000000003")
	if err := os.MkdirAll(filepath.Dir(m.volumePath("datos")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.volumePath("datos"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	vol := []api.VolumeAttachment{{Name: "datos", Mount: "/data"}}
	m.mu.Lock()
	m.byID[mc.ID].Volumes = vol
	m.byID["otra0000000000aa"] = &api.Machine{ID: "otra0000000000aa", Name: "otra", State: api.StateRunning, Volumes: vol}
	m.mu.Unlock()

	_, err := m.Start(context.Background(), mc.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "WRITE mode") {
		t.Fatalf("Start con el volumen en uso = %v; quería el rechazo por escritor", err)
	}
	if got := vivaDe(t, m, mc.ID).State; got != api.StateStopped {
		t.Fatalf("estado = %s tras el rechazo; quería stopped", got)
	}
	if p := pendiente(m); p != 0 {
		t.Fatalf("pendingMiB = %d; la reserva de memoria quedó colgada", p)
	}
}

// Un arranque que falla (aquí, el VMM no existe) deja la máquina PARADA con
// el motivo, no failed: se puede reintentar, y el GC no la recoge por un
// intento roto. Sin red montada ni reserva colgada.
func TestStartFallidoLaDejaParada(t *testing.T) {
	m := newTestManager(t)
	m.fcBin = filepath.Join(t.TempDir(), "no-hay-vmm")
	mc, red := paradaParaStart(t, m, "5a5a000000000004")

	_, err := m.Start(context.Background(), mc.ID, nil)
	if err == nil {
		t.Fatal("Start sin VMM no falló")
	}
	viva := vivaDe(t, m, mc.ID)
	if viva.State != api.StateStopped || viva.LastErr == "" || viva.PID != 0 {
		t.Fatalf("tras el fallo: estado %s, LastErr %q, PID %d; quería stopped con el motivo", viva.State, viva.LastErr, viva.PID)
	}
	ns := knet.Plan(mc.NetIndex, mc.ID).NS
	if len(*red) < 2 || (*red)[0] != "+"+ns || (*red)[len(*red)-1] != "-"+ns {
		t.Fatalf("red = %v; quería montarla y desmontarla", *red)
	}
	if p := pendiente(m); p != 0 {
		t.Fatalf("pendingMiB = %d; la reserva de memoria quedó colgada", p)
	}
	if _, err := os.Stat(filepath.Join(m.dir(mc.ID), "overlay.ext4")); err != nil {
		t.Fatalf("el disco de la máquina desapareció: %v", err)
	}
}

// Parar conserva el disco y suelta el volcado: una parada arranca en frío, y
// el mem.file de una congelada (del tamaño de su RAM) se quedaba en disco
// hasta el rm sin que nadie pudiera cargarlo.
func TestStopDeUnaCongeladaBorraSuVolcado(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	m.priv = &Privileges{}
	mc := congeladaParaThaw(t, m, "5a5a000000000005", 64)
	dir := m.dir(mc.ID)
	if err := os.WriteFile(filepath.Join(dir, "overlay.ext4"), []byte("disco"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, memDiff), []byte("diff"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := m.Stop(mc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != api.StateStopped || out.StoppedAt == nil || out.FrozenAt != nil {
		t.Fatalf("Stop = %s, StoppedAt %v, FrozenAt %v", out.State, out.StoppedAt, out.FrozenAt)
	}
	for _, f := range []string{"snap.file", "mem.file", memDiff, marcaOK} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s sigue ahí tras parar (%v)", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "overlay.ext4")); err != nil {
		t.Fatalf("parar se llevó el disco: %v", err)
	}
}

// Una parada no retiene su imagen: si se borró, Start lo dice nombrándola y
// con cómo volver (importarla otra vez con el mismo nombre), sin tocar la
// máquina ni dejar memoria reservada.
func TestStartSinImagenDiceComoVolver(t *testing.T) {
	m := newTestManager(t)
	mc, _ := paradaParaStart(t, m, "5a5a000000000009")
	if err := os.Remove(m.imagePath(mc.Image)); err != nil {
		t.Fatal(err)
	}
	_, err := m.Start(context.Background(), mc.ID, nil)
	if err == nil || !strings.Contains(err.Error(), `image "`+mc.Image+`" is gone`) ||
		!strings.Contains(err.Error(), "kling image import -name "+mc.Image) {
		t.Fatalf("Start sin imagen = %v; quería que la nombrase y dijera cómo volver a traerla", err)
	}
	if got := vivaDe(t, m, mc.ID).State; got != api.StateStopped {
		t.Fatalf("estado = %s; quería stopped", got)
	}
	if p := pendiente(m); p != 0 {
		t.Fatalf("pendingMiB = %d", p)
	}
}

// Start revisa el overlay antes de arrancar sobre él: es ext4 sin journal, y
// una parada a la brava (pausada, o muerta con el anfitrión) lo deja sucio.
// Lo que e2fsck no arregla solo no se monta: la máquina sigue parada, con el
// motivo, y sin red ni memoria colgadas.
func TestStartRevisaSuDiscoAntesDeArrancar(t *testing.T) {
	m := newTestManager(t)
	m.fcBin = filepath.Join(t.TempDir(), "no-hay-vmm")
	mc, red := paradaParaStart(t, m, "5a5a000000000007")
	overlay := filepath.Join(m.dir(mc.ID), "overlay.ext4")

	var revisados []string
	codigo := "0"
	ordenE2fsck = func(ctx context.Context, path string) *exec.Cmd {
		revisados = append(revisados, path)
		return exec.CommandContext(ctx, "sh", "-c", "echo e2fsck dijo algo; exit "+codigo)
	}
	// Sano: se revisa y el arranque sigue (y falla luego, sin VMM).
	if _, err := m.Start(context.Background(), mc.ID, nil); err == nil || strings.Contains(err.Error(), "checking its disk") {
		t.Fatalf("Start con el disco sano = %v; quería que siguiera hasta el VMM", err)
	}
	if len(revisados) != 1 || revisados[0] != overlay {
		t.Fatalf("e2fsck revisó %v; quería solo el overlay %s", revisados, overlay)
	}

	// Errores sin corregir: no arranca, sigue parada y no montó la red.
	revisados, codigo, *red = nil, "4", nil
	_, err := m.Start(context.Background(), mc.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "checking its disk") || !strings.Contains(err.Error(), "uncorrected") {
		t.Fatalf("Start con el disco roto = %v; quería el error de e2fsck", err)
	}
	viva := vivaDe(t, m, mc.ID)
	if viva.State != api.StateStopped || viva.LastErr == "" {
		t.Fatalf("tras el fallo: estado %s, LastErr %q; quería stopped con el motivo", viva.State, viva.LastErr)
	}
	for _, r := range *red {
		if strings.HasPrefix(r, "+") {
			t.Fatalf("red = %v; con el disco roto no tenía que montarla", *red)
		}
	}
	if p := pendiente(m); p != 0 {
		t.Fatalf("pendingMiB = %d; la reserva de memoria quedó colgada", p)
	}
}

// Parar vacía la caché del invitado aunque no tenga volúmenes: el overlay
// sobrevive (kling start) y no lleva journal. Borrar sin volúmenes
// escribibles no la pide.
func TestHayQueVaciarElOverlayAlParar(t *testing.T) {
	sinVol := &api.Machine{}
	soloLectura := &api.Machine{Volumes: []api.VolumeAttachment{{ReadOnly: true}}}
	escribible := &api.Machine{Volumes: []api.VolumeAttachment{{}}}
	for _, c := range []struct {
		mc      *api.Machine
		conserv bool
		quiero  bool
	}{
		{sinVol, true, true}, {soloLectura, true, true}, {escribible, true, true},
		{sinVol, false, false}, {soloLectura, false, false}, {escribible, false, true},
	} {
		if got := hayQueVaciar(c.mc, c.conserv); got != c.quiero {
			t.Errorf("hayQueVaciar(%d volúmenes, conservaDisco=%v) = %v; quería %v", len(c.mc.Volumes), c.conserv, got, c.quiero)
		}
	}
}
