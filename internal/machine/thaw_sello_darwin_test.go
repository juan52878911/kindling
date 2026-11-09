//go:build darwin

package machine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// Si el sello no se puede retirar (disco lleno), el thaw no carga nada: antes
// se cargaba y el fallo solo se avisaba, así que un daemon que moría después
// encontraba el volcado viejo con su sello (o sin marcas) y lo daba por bueno.
func TestThawSinPoderRetirarElSelloNoCarga(t *testing.T) {
	m, logPath := managerVZ(t)
	id := "ee55ff66aa77bb98"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	volcadoSellado(t, dir)
	m.byID[id] = &api.Machine{ID: id, Name: "vz-lleno", State: api.StateWarm, Egress: "none",
		IP: knet.GuestIP, CreatedAt: time.Now()}
	viejoR, viejoE := renombrarMarca, escribirMarca
	renombrarMarca = func(string, string) error { return syscall.ENOSPC }
	escribirMarca = func(string, []byte, os.FileMode) error { return syscall.ENOSPC }
	t.Cleanup(func() { renombrarMarca, escribirMarca = viejoR, viejoE })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Thaw(ctx, id)
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err == nil {
		t.Fatal("el thaw siguió sin poder retirar el sello")
	}
	for _, l := range llamadas(t, logPath) {
		if strings.HasPrefix(l, "PUT /snapshot/load") {
			t.Fatalf("cargó el volcado sin haber retirado su sello (%v)", err)
		}
	}
	if verr := volcadoValido(dir, nil); verr != nil {
		t.Fatalf("sin cargar nada, el volcado debería seguir valiendo: %v", verr)
	}
}

// Si el VMM rechaza la carga, el invitado no corrió: el volcado recupera su
// sello y la máquina se puede descongelar otra vez.
func TestThawQueElVMMRechazaConservaElSello(t *testing.T) {
	m, _ := managerVZ(t)
	t.Setenv(envFakeVZRechazaCarga, "1")
	id := "ee55ff66aa77bb97"
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	volcadoSellado(t, dir)
	m.byID[id] = &api.Machine{ID: id, Name: "vz-rechazo", State: api.StateWarm, Egress: "none",
		IP: knet.GuestIP, CreatedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := m.Thaw(ctx, id)
	if out != nil {
		defer matarVMM(out.PID)
	}
	if err == nil {
		t.Fatal("el thaw no vio el rechazo del VMM")
	}
	if verr := volcadoValido(dir, nil); verr != nil {
		t.Fatalf("el VMM no cargó nada y el volcado perdió su sello: %v", verr)
	}
}
