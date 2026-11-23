//go:build darwin

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

// El thaw manda en PUT /snapshot/load las rutas que el daemon conoce (kernel,
// rootfs, overlay, volúmenes), para que el VMM no abra las del snap.file.
func TestThawMandaLasRutasDelDaemon(t *testing.T) {
	m, logPath := managerVZ(t)
	id := "ee55ff66aa77bb91"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	volcadoSellado(t, dir)
	if err := os.MkdirAll(filepath.Dir(m.imagePath("min")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.imagePath("min"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.byID[id] = &api.Machine{ID: id, Name: "vz-repoint", State: api.StateWarm, Egress: "none",
		IP: knet.GuestIP, CreatedAt: time.Now(), Image: "min",
		Volumes: []api.VolumeAttachment{{Name: "datos", Mount: "/data", DriveID: "volume0"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Thaw(ctx, id)
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err != nil {
		t.Fatalf("thaw: %v", err)
	}
	ls := llamadas(t, logPath)
	i := indice(ls, "PUT /snapshot/load")
	if i < 0 {
		t.Fatalf("no cargó el volcado: %v", ls)
	}
	for _, quiero := range []string{
		" repoint kernel=" + m.kernelDe(false),
		" overlay=" + filepath.Join(dir, "overlay.ext4"),
		" rootfs=" + m.imagePath("min"),
		" volume0=" + m.volumePath("datos"),
		" volume=" + m.volumePath("datos"),
	} {
		if !strings.Contains(ls[i], quiero) {
			t.Errorf("el load no lleva %q: %s", quiero, ls[i])
		}
	}
}
