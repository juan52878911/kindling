//go:build linux

package machine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// envHijoJaula convierte el binario de pruebas en el «e2fsck» de
// TestE2fsckNoLlegaAOtraMaquina.
const envHijoJaula = "KLING_TEST_HIJO_JAULA"

// TestAyudanteJaulaE2fs no es una prueba: es el «e2fsck» encerrado, y dice
// qué consiguió hacer.
func TestAyudanteJaulaE2fs(t *testing.T) {
	if os.Getenv(envHijoJaula) == "" {
		t.Skip("proceso hijo de TestE2fsckNoLlegaAOtraMaquina")
	}
	fmt.Printf("RESULTADO uid=%d\n", os.Getuid())
	if f, err := os.OpenFile(os.Getenv("KLING_TEST_OTRO"), os.O_RDWR, 0); err == nil {
		f.Close()
		fmt.Println("RESULTADO otro=abierto")
	} else {
		fmt.Println("RESULTADO otro=no")
	}
	if _, err := os.ReadFile(os.Getenv("KLING_TEST_PUBLICO")); err == nil {
		fmt.Println("RESULTADO publico=leido")
	} else {
		fmt.Println("RESULTADO publico=no")
	}
	if b, err := os.ReadFile(rutaE2fsck); err == nil {
		fmt.Printf("RESULTADO disco=%s\n", b)
	} else {
		fmt.Println("RESULTADO disco=no", err)
	}
	n := 0
	if es, err := os.ReadDir("/proc"); err == nil {
		for _, e := range es {
			if _, err := strconv.Atoi(e.Name()); err == nil {
				n++
			}
		}
	}
	fmt.Printf("RESULTADO procesos=%d\n", n)
	if err := os.WriteFile("/escrito", []byte("x"), 0o644); err == nil {
		fmt.Println("RESULTADO escribir=si")
	} else {
		fmt.Println("RESULTADO escribir=no")
	}
}

// A1: un e2fsck explotado sobre el overlay de la máquina A no llega al de la
// máquina B. Los dos son del usuario del VMM (como en un daemon con -run-as):
// antes corría con ese usuario y abría B; ahora corre como nobody, en un
// chroot sin ruta a B ni a nada del host (ni siquiera a un fichero que todos
// pueden leer), con un /proc donde solo está él y la raíz de solo lectura. Su
// disco sí lo lee, por /dev/fd/3, y al acabar vuelve a su dueño. Con root.
func TestE2fsckNoLlegaAOtraMaquina(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("hace falta root: la jaula, el cambio de uid y de dueño")
	}
	if err := sondaJaulaE2fs(); err != nil {
		t.Fatalf("la jaula de e2fsprogs no se puede montar en este host: %v", err)
	}
	const uidVMM = 4243
	base := t.TempDir()
	for d := base; d != filepath.Dir(d) && strings.HasPrefix(d, os.TempDir()) && d != filepath.Clean(os.TempDir()); d = filepath.Dir(d) {
		_ = os.Chmod(d, 0o755)
	}
	disco := func(maquina, contenido string) string {
		dir := filepath.Join(base, "machines", maquina)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "overlay.ext4")
		if err := os.WriteFile(p, []byte(contenido), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(p, uidVMM, uidVMM); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := disco("A", "disco-A"), disco("B", "disco-B")
	publico := filepath.Join(base, "publico")
	if err := os.WriteFile(publico, []byte("todos"), 0o644); err != nil {
		t.Fatal(err)
	}
	// El binario de pruebas, en su propio directorio (la jaula monta el del
	// binario que ejecuta): ni A ni B ni publico están debajo.
	binDir := filepath.Join(base, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	datos, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "e2fsck.test")
	if err := os.WriteFile(bin, datos, 0o755); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(a, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := exec.Command(bin, "-test.run=^TestAyudanteJaulaE2fs$", "-test.count=1")
	c.Env = []string{envHijoJaula + "=1", "KLING_TEST_OTRO=" + b, "KLING_TEST_PUBLICO=" + publico}
	c.ExtraFiles = []*os.File{f}
	deshacer, err := confinarE2fs(c, f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.CombinedOutput()
	deshacer()
	if err != nil {
		t.Fatalf("hijo encerrado: %v\n%s", err, out)
	}
	res := map[string]string{}
	for _, linea := range strings.Split(string(out), "\n") {
		if r, ok := strings.CutPrefix(strings.TrimSpace(linea), "RESULTADO "); ok {
			k, v, _ := strings.Cut(r, "=")
			res[k] = v
		}
	}
	if res["disco"] != "disco-A" {
		t.Fatalf("no leyó su disco por %s: %q\n%s", rutaE2fsck, res["disco"], out)
	}
	for k, want := range map[string]string{
		"uid":      strconv.Itoa(uidNadieE2fsck),
		"otro":     "no",
		"publico":  "no",
		"procesos": "1",
		"escribir": "no",
	} {
		if res[k] != want {
			t.Errorf("%s = %q, quería %q\n%s", k, res[k], want, out)
		}
	}
	fi, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != uidVMM || st.Gid != uidVMM || fi.Mode().Perm() != 0o600 {
		t.Errorf("el disco de A quedó %d:%d %o; quería %d:%d 0600", st.Uid, st.Gid, fi.Mode().Perm(), uidVMM, uidVMM)
	}
}

// Sin root no hay jaula ni usuario al que bajar, pero el tope de memoria sí,
// ya sin depender de prlimit (M6): la orden pasa por el hijo en modo simple.
func TestConfinarE2fsSinRootPoneElTope(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("sin root")
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "d.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := exec.Command("/bin/sh", "-c", "ulimit -v")
	c.ExtraFiles = []*os.File{f}
	deshacer, err := confinarE2fs(c, f)
	if err != nil {
		t.Fatal(err)
	}
	defer deshacer()
	o, ok := ordenEnJaula(c)
	if !ok || o.modo != modoSimple || o.uid != -1 || o.mem != memE2fsck {
		t.Fatalf("orden %+v (ok=%v); quería modo simple, sin cambio de usuario y con el tope", o, ok)
	}
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), strconv.Itoa(memE2fsck>>10); got != want {
		t.Errorf("ulimit -v = %q, quería %s KiB", got, want)
	}
}

// El argv del hijo va y vuelve igual.
func TestOrdenJaulaIdaYVuelta(t *testing.T) {
	o := ordenJaula{modo: modoJaula, uid: 65534, gid: 4, mem: memE2fsck, raiz: "/tmp/r",
		argv: []string{"/usr/sbin/e2fsck", "-p", "-f", "--", "/dev/fd/3"}}
	got, err := leerOrdenJaula(o.args()[1:])
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(o) {
		t.Fatalf("%+v\n%+v", got, o)
	}
	if _, err := leerOrdenJaula([]string{"jaula", "1"}); err == nil {
		t.Fatal("argv corto aceptado")
	}
}
