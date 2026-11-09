package machine

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// Un debugfs que solo lee no se lleva el dueño de una imagen base que los VMM
// abren por grupo (darLecturaVMM): corre como nobody con ese grupo y el
// fichero no cambia. Uno que escribe, sí la recibe (y la devuelve). Con
// root.
func TestConfinarE2fsSoloLecturaNoCambiaElDueno(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("hace falta root: el cambio de uid y de dueño")
	}
	const grupoVMM = 4321
	img := filepath.Join(t.TempDir(), "base.ext4")
	if err := os.WriteFile(img, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(img, 0, grupoVMM); err != nil {
		t.Fatal(err)
	}
	dueno := func() (uint32, uint32) {
		fi, err := os.Stat(img)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		return st.Uid, st.Gid
	}

	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("/bin/true")
	deshacer, err := confinarE2fs(c, f)
	if err != nil {
		t.Fatal(err)
	}
	if u, g := dueno(); u != 0 || g != grupoVMM {
		t.Errorf("leer cambió el dueño de la imagen a %d:%d", u, g)
	}
	if cr := c.SysProcAttr.Credential; cr.Uid != uint32(uidNadieE2fsck) || cr.Gid != grupoVMM {
		t.Errorf("debugfs de lectura con %d:%d; quería %d:%d", cr.Uid, cr.Gid, uidNadieE2fsck, grupoVMM)
	}
	deshacer()
	f.Close()

	f, err = os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c = exec.Command("/bin/true")
	deshacer, err = confinarE2fs(c, f)
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := dueno(); u != uint32(uidNadieE2fsck) {
		t.Errorf("escribir no cedió la imagen (dueño %d)", u)
	}
	deshacer()
	if u, g := dueno(); u != 0 || g != grupoVMM {
		t.Errorf("tras escribir la imagen quedó %d:%d", u, g)
	}
}
