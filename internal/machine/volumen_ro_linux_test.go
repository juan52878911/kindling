package machine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// envHijoVolumen convierte el binario de pruebas en el «VMM» de
// TestVMMNoEscribeUnVolumenRO.
const envHijoVolumen = "KLING_TEST_HIJO_VOLUMEN"

// TestAyudanteVolumenRW no es una prueba: es el proceso hijo que, como el
// usuario del VMM, intenta abrir el volumen en escritura y dice qué pasó.
func TestAyudanteVolumenRW(t *testing.T) {
	ruta := os.Getenv(envHijoVolumen)
	if ruta == "" {
		t.Skip("proceso hijo de TestVMMNoEscribeUnVolumenRO")
	}
	f, err := os.OpenFile(ruta, os.O_RDWR, 0)
	switch {
	case err == nil:
		f.Close()
		fmt.Println("RESULTADO rw")
	case errors.Is(err, syscall.EACCES):
		fmt.Println("RESULTADO eacces")
	default:
		fmt.Println("RESULTADO otro", err)
	}
}

// M2 con permisos de verdad: el usuario del VMM no puede abrir O_RDWR un
// volumen que se admitió de solo lectura (EACCES), sí uno que se le adjudicó
// en escritura exclusiva, y otra vez no cuando se suelta. Necesita root (para
// cambiar dueños y lanzar el hijo con otro uid).
func TestVMMNoEscribeUnVolumenRO(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("hace falta root para cambiar el dueño del volumen y bajar el uid del hijo")
	}
	m := conVolumen(t, "datos")
	m.priv = &Privileges{Enabled: true, UID: usuarioVMMPrueba, GID: usuarioVMMPrueba}
	ruta := m.volumePath("datos")
	m.permisosVolumen(ruta)

	// El hijo tiene que llegar al volumen y ejecutar el binario de la prueba.
	for d := m.volumesDir(); d != filepath.Dir(d) && strings.HasPrefix(d, os.TempDir()) && d != os.TempDir(); d = filepath.Dir(d) {
		_ = os.Chmod(d, 0o711)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	datos, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(m.root, "prueba.test")
	if err := os.WriteFile(bin, datos, 0o755); err != nil {
		t.Fatal(err)
	}
	vmm := func() string {
		t.Helper()
		c := exec.Command(bin, "-test.run=^TestAyudanteVolumenRW$", "-test.count=1")
		c.Env = []string{envHijoVolumen + "=" + ruta}
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: usuarioVMMPrueba, Gid: usuarioVMMPrueba, Groups: []uint32{},
		}}
		out, _ := c.CombinedOutput()
		for _, l := range strings.Split(string(out), "\n") {
			if r, ok := strings.CutPrefix(strings.TrimSpace(l), "RESULTADO "); ok {
				return r
			}
		}
		t.Fatalf("el hijo no dijo nada: %s", out)
		return ""
	}

	ro := api.RunRequest{Volumes: []api.VolumeAttachment{{Name: "datos", Mount: "/data", ReadOnly: true}}}
	if _, err := m.reservarVolumenes(ro, "lector", "lector"); err != nil {
		t.Fatal(err)
	}
	if r := vmm(); r != "eacces" {
		t.Fatalf("el VMM de un lector abrió el volumen en escritura: %s", r)
	}
	m.soltarReservas("lector")

	if _, err := m.reservarVolumenes(api.RunRequest{Volume: "datos"}, "escritor", "escritor"); err != nil {
		t.Fatal(err)
	}
	if r := vmm(); r != "rw" {
		t.Fatalf("el VMM del escritor no pudo abrir su volumen en escritura: %s", r)
	}
	m.soltarReservas("escritor")
	if r := vmm(); r != "eacces" {
		t.Fatalf("soltado el escritor, el VMM sigue pudiendo escribir el volumen: %s", r)
	}
	fi, err := os.Stat(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != 0 || st.Gid != usuarioVMMPrueba || fi.Mode().Perm() != 0o640 {
		t.Fatalf("volumen %d:%d %v; quería 0:%d 0640", st.Uid, st.Gid, fi.Mode().Perm(), usuarioVMMPrueba)
	}
}
