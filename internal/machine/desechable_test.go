package machine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
	"github.com/juan52878911/kindling/pkg/api"
)

// Firecracker rechaza los campos que no conoce: kling_disposable solo puede
// salir hacia kling-vz y kling-hv.
func TestDesechableSoloConKlingVZ(t *testing.T) {
	if vmmAdmiteDesechable(BackendFirecracker) {
		t.Fatal("Firecracker no conoce kling_disposable")
	}
	if !vmmAdmiteDesechable(BackendVZ) || !vmmAdmiteDesechable(BackendHV) {
		t.Fatal("kling-vz y kling-hv sí lo conocen")
	}
	sandbox := &api.Machine{ID: "a", OnTTL: api.OnTTLRemove}
	if got, want := overlayDesechable(sandbox), vmmAdmiteDesechable(backendVMM); got != want {
		t.Fatalf("overlayDesechable(on_ttl=remove) con %s = %v, quiero %v", backendVMM, got, want)
	}
	for _, onTTL := range []string{"", api.OnTTLFreeze} {
		if overlayDesechable(&api.Machine{ID: "a", OnTTL: onTTL}) {
			t.Fatalf("on_ttl=%q se congela y conserva su overlay: no es desechable", onTTL)
		}
	}
	if overlayDesechable(nil) {
		t.Fatal("una máquina que no existe no es desechable")
	}
}

// Con false el campo no viaja: es lo que mantiene a Firecracker a salvo.
func TestDriveSinDesechableNoLlevaElCampo(t *testing.T) {
	b, err := json.Marshal(fc.Drive{DriveID: "overlay", PathOnHost: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "kling_disposable") {
		t.Fatalf("un drive normal no debe llevar kling_disposable: %s", b)
	}
	b, _ = json.Marshal(fc.Drive{DriveID: "overlay", PathOnHost: "/x", KlingDisposable: true})
	if !strings.Contains(string(b), `"kling_disposable":true`) {
		t.Fatalf("falta kling_disposable: %s", b)
	}
}

func TestPatchDriveDisposableMandaElCampo(t *testing.T) {
	f := nuevoFcFalso(t)
	c := fc.New(f.Sock)
	if err := c.PatchDrive(context.Background(), "overlay", "/a"); err != nil {
		t.Fatal(err)
	}
	if err := c.PatchDriveDisposable(context.Background(), "overlay", "/b"); err != nil {
		t.Fatal(err)
	}
	ll := f.todas()
	if len(ll) != 2 {
		t.Fatalf("quiero 2 llamadas, hay %d", len(ll))
	}
	if strings.Contains(string(ll[0].Cuerpo), "kling_disposable") {
		t.Fatalf("PatchDrive no debe marcar nada: %s", ll[0].Cuerpo)
	}
	var d struct {
		DriveID    string `json:"drive_id"`
		PathOnHost string `json:"path_on_host"`
		Disposable *bool  `json:"kling_disposable"`
	}
	if err := json.Unmarshal(ll[1].Cuerpo, &d); err != nil {
		t.Fatal(err)
	}
	if ll[1].Metodo != "PATCH" || ll[1].Ruta != "/drives/overlay" || d.PathOnHost != "/b" || d.Disposable == nil || !*d.Disposable {
		t.Fatalf("PatchDriveDisposable mandó %s %s %s", ll[1].Metodo, ll[1].Ruta, ll[1].Cuerpo)
	}
}

// Solo queda fallida la que corría con el overlay sin sincronizar y vio
// reiniciarse el host desde que arrancó.
func TestFallidaTrasCorte(t *testing.T) {
	antes := time.Now().Add(-time.Hour)
	despues := time.Now()
	sandbox := func(arrancada *time.Time) *api.Machine {
		return &api.Machine{ID: "a", OnTTL: api.OnTTLRemove, StartedAt: arrancada}
	}
	vz := vmmAdmiteDesechable(backendVMM)
	casos := []struct {
		nombre      string
		mc          *api.Machine
		arranque    time.Time
		conArranque bool
		quiero      bool
	}{
		{"el host arrancó después: corte", sandbox(&antes), despues, true, vz},
		{"el host ya estaba: solo el daemon", sandbox(&despues), antes, true, false},
		{"sin hora de arranque del host: lo peor", sandbox(&despues), time.Time{}, false, vz},
		{"sin StartedAt: lo peor", sandbox(nil), antes, true, vz},
		{"on_ttl=freeze: nunca", &api.Machine{ID: "a", StartedAt: &antes}, despues, true, false},
	}
	for _, c := range casos {
		if got := fallidaTrasCorte(c.mc, c.arranque, c.conArranque); got != c.quiero {
			t.Errorf("%s: %v, quiero %v", c.nombre, got, c.quiero)
		}
	}
}

// asentarOverlay sincroniza el overlay de un sandbox; sin fichero no hay nada
// que hacer, y un enlace (el VMM es dueño del directorio) no se sigue.
func TestAsentarOverlay(t *testing.T) {
	m := &Manager{root: t.TempDir(), byID: map[string]*api.Machine{}}
	mc := &api.Machine{ID: "ee11bb22cc33dd44", OnTTL: api.OnTTLRemove}
	if err := m.asentarOverlay(mc); err != nil {
		t.Fatalf("sin overlay: %v", err)
	}
	if err := os.MkdirAll(m.dir(mc.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	ov := filepath.Join(m.dir(mc.ID), "overlay.ext4")
	if err := os.WriteFile(ov, []byte("datos"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.asentarOverlay(mc); err != nil {
		t.Fatalf("overlay regular: %v", err)
	}
	if err := os.Remove(ov); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", ov); err != nil {
		t.Fatal(err)
	}
	if err := m.asentarOverlay(mc); err != nil {
		t.Fatalf("un enlace se salta, no es error: %v", err)
	}
	// Una que no es desechable no se toca, exista o no su overlay.
	if err := m.asentarOverlay(&api.Machine{ID: mc.ID}); err != nil {
		t.Fatal(err)
	}
}
