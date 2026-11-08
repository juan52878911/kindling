//go:build darwin

package machine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// En cuanto el invitado reanuda, su volcado deja de valer: escribe en su
// disco. Si el sello seguía, un daemon que moría con su VMM antes de escribir
// el running (o después: reconcile da por warm una running sin proceso con
// el volcado sellado) volvía a cargar aquella memoria sobre otro disco.
func TestThawRetiraElSelloDelVolcadoQueCargo(t *testing.T) {
	m, _ := managerVZ(t)
	id := "ee55ff66aa77bb99"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snap.file", "mem.file", "overlay.ext4"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := volcadoEnCurso(dir); err != nil {
		t.Fatal(err)
	}
	if err := sellarVolcado(dir, "", "", "", false); err != nil {
		t.Fatal(err)
	}
	m.byID[id] = &api.Machine{ID: id, Name: "vz-sello", State: api.StateWarm, Egress: "none",
		IP: knet.GuestIP, CreatedAt: time.Now()}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Thaw(ctx, id)
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err != nil {
		t.Fatalf("thaw: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, marcaOK)); !os.IsNotExist(err) {
		t.Fatalf("el sello del volcado sigue tras reanudar (%v)", err)
	}
	if m.hasSnapshot(id) {
		t.Fatal("reconcile daría por warm una máquina que ya corre")
	}
}
