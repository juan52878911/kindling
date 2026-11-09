package machine

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// envHijoE2fs convierte el binario de pruebas en el «e2fsck» confinado de
// TestE2fsckConfinado: con ella, TestAyudanteE2fs cuenta qué puede hacer.
const envHijoE2fs = "KLING_TEST_HIJO_E2FS"

// TestAyudanteE2fs no es una prueba: es el proceso hijo que corre con el
// confinamiento de e2fsck y dice, línea a línea, qué consiguió: su uid, si
// leyó un fichero de root fuera de su disco, si leyó su disco por /dev/fd/3 y
// si llegó a la red.
func TestAyudanteE2fs(t *testing.T) {
	if os.Getenv(envHijoE2fs) == "" {
		t.Skip("proceso hijo de TestE2fsckConfinado")
	}
	fmt.Printf("RESULTADO uid=%d\n", os.Getuid())
	if _, err := os.ReadFile(os.Getenv("KLING_TEST_SECRETO")); err == nil {
		fmt.Println("RESULTADO secreto=leido")
	} else {
		fmt.Println("RESULTADO secreto=no")
	}
	if f, err := os.OpenFile(rutaE2fsck, os.O_RDWR, 0); err != nil {
		fmt.Println("RESULTADO disco=no", err)
	} else {
		b := make([]byte, 6)
		n, _ := f.Read(b)
		fmt.Printf("RESULTADO disco=%s\n", b[:n])
		f.Close()
	}
	if c, err := net.DialTimeout("tcp", os.Getenv("KLING_TEST_RED"), time.Second); err == nil {
		c.Close()
		fmt.Println("RESULTADO red=si")
	} else {
		fmt.Println("RESULTADO red=no")
	}
}

// e2fsck analiza un ext4 que escribió el invitado (A1): corre confinado. El
// hijo recibe el disco por /dev/fd/3 y lo lee, pero no corre como root (en
// Linux), no lee un fichero 0600 de root que está a su alcance por ruta y no
// llega a un puerto de loopback del host. En Linux necesita root (sin él no
// hay uid que bajar ni namespaces que crear); en macOS corre con el usuario.
func TestE2fsckConfinado(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		t.Skip("en Linux hace falta root para comprobar la bajada de uid y los namespaces")
	}
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
			t.Skip("sin sandbox-exec")
		}
	}
	dir := t.TempDir()
	// Directorio atravesable por cualquiera: si el hijo no lee el secreto es
	// por el confinamiento, no porque no llegue a la ruta.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	secreto := filepath.Join(dir, "secreto")
	if err := os.WriteFile(secreto, []byte("no-lo-leas"), 0o600); err != nil {
		t.Fatal(err)
	}
	disco := filepath.Join(dir, "disco.ext4")
	if err := os.WriteFile(disco, []byte("kindling"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	f, err := os.OpenFile(disco, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// El hijo sin root no puede ejecutar el binario de pruebas donde lo dejó
	// go test (un directorio 0700 de root): va una copia al directorio de la
	// prueba, y los de encima, hasta TempDir, se dejan atravesar.
	for d := filepath.Dir(dir); d != filepath.Dir(d) && strings.HasPrefix(d, os.TempDir()) && d != filepath.Clean(os.TempDir()); d = filepath.Dir(d) {
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
	bin := filepath.Join(dir, "prueba.test")
	if err := os.WriteFile(bin, datos, 0o755); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(bin, "-test.run=^TestAyudanteE2fs$", "-test.count=1")
	c.Env = []string{envHijoE2fs + "=1", "KLING_TEST_SECRETO=" + secreto, "KLING_TEST_RED=" + l.Addr().String()}
	c.ExtraFiles = []*os.File{f}
	deshacer, err := confinarE2fs(c, f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.CombinedOutput()
	deshacer()
	if err != nil {
		t.Fatalf("hijo confinado: %v\n%s", err, out)
	}
	res := map[string]string{}
	for _, linea := range strings.Split(string(out), "\n") {
		if r, ok := strings.CutPrefix(strings.TrimSpace(linea), "RESULTADO "); ok {
			k, v, _ := strings.Cut(r, "=")
			res[k] = v
		}
	}
	if !strings.HasPrefix(res["disco"], "kindli") {
		t.Fatalf("el hijo no leyó su disco por %s: %q\n%s", rutaE2fsck, res["disco"], out)
	}
	if runtime.GOOS == "linux" && (res["uid"] == "" || res["uid"] == "0") {
		t.Errorf("el hijo corrió con uid %q; quería uno sin privilegios", res["uid"])
	}
	if res["secreto"] != "no" {
		t.Errorf("el hijo leyó un fichero 0600 ajeno a su disco (%q)", res["secreto"])
	}
	if res["red"] != "no" {
		t.Errorf("el hijo llegó a la red del host (%q)", res["red"])
	}
	// Lo que se le cedió del disco se devuelve.
	if runtime.GOOS == "linux" {
		fi, err := os.Stat(disco)
		if err != nil {
			t.Fatal(err)
		}
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != 0 || fi.Mode().Perm() != 0o600 {
			t.Errorf("el disco quedó con dueño %d y modo %o; quería root y 0600", st.Uid, fi.Mode().Perm())
		}
	}
}

// imagenExt4Prueba crea un ext4 de verdad de tamMiB, o salta sin e2fsprogs.
func imagenExt4Prueba(t *testing.T, tamMiB int) string {
	t.Helper()
	if buscarE2fs("e2fsck") == "" || buscarE2fs("mkfs.ext4") == "" && buscarE2fs("mke2fs") == "" {
		t.Skip("sin e2fsprogs (e2fsck, mkfs.ext4)")
	}
	img := filepath.Join(t.TempDir(), "vol.ext4")
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, int64(tamMiB)<<20); err != nil {
		t.Fatal(err)
	}
	if out, err := e2fsCmd(context.Background(), "mkfs.ext4", "-q", "-F", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}
	return img
}

// El e2fsck de verdad, confinado y con el disco por descriptor: uno sano pasa
// sin reparar, y uno corrupto (superbloque destrozado, o basura) da un error
// que nombra el fichero, sin pánico y sin colgarse.
func TestRevisarExt4ConfinadoDeVerdad(t *testing.T) {
	img := imagenExt4Prueba(t, 16)
	reparado, out, err := revisarExt4(context.Background(), img)
	if err != nil || reparado {
		t.Fatalf("ext4 sano: reparado=%v err=%v\n%s", reparado, err, out)
	}
	if bytes.Contains(out, []byte(rutaE2fsck)) || !bytes.Contains(out, []byte("vol.ext4")) {
		t.Errorf("la salida habla de %s en vez del fichero: %s", rutaE2fsck, out)
	}

	// Superbloque y descriptores de grupo machacados.
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	basura := bytes.Repeat([]byte{0xA5, 0x5A, 0xFF, 0x00}, 16<<10/4)
	if _, err := f.WriteAt(basura, 1024); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, _, err := revisarExt4(context.Background(), img); err == nil {
		t.Fatal("ext4 con el superbloque roto: e2fsck no dio error")
	} else if !strings.Contains(err.Error(), "vol.ext4") {
		t.Errorf("el error no nombra el fichero: %v", err)
	}

	// Basura de punta a punta.
	if err := os.WriteFile(img, bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 1<<18), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := revisarExt4(context.Background(), img); err == nil {
		t.Fatal("un fichero de basura pasó por ext4 sano")
	}
	// repairVolume no falla por ello: avisa y sigue.
	repairVolume(context.Background(), img)

	// Un disco que no existe es un error, no un pánico.
	if _, _, err := revisarExt4(context.Background(), filepath.Join(t.TempDir(), "no-hay.ext4")); err == nil {
		t.Fatal("revisarExt4 sobre un fichero que no existe no dio error")
	}
}
