package machine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/digest"
)

// ext4Prueba crea un ext4 vacío de tam MiB y lo prepara con órdenes de
// debugfs. Se salta sin e2fsprogs.
func ext4Prueba(t *testing.T, tamMiB int, ordenes ...string) (*Manager, imagenDebugfs) {
	t.Helper()
	bin := debugfsBin()
	if bin == "" || buscarE2fs("mkfs.ext4") == "" && buscarE2fs("mke2fs") == "" {
		t.Skip("sin e2fsprogs (debugfs, mkfs.ext4)")
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "img.ext4")
	if err := os.WriteFile(img, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img, int64(tamMiB)<<20); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if out, err := e2fsCmd(ctx, "mkfs.ext4", "-q", "-F", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}
	m := &Manager{root: dir}
	im := imagenDebugfs{bin: bin, file: img}
	if len(ordenes) > 0 {
		if err := im.escribir(ctx, dir, ordenes); err != nil {
			t.Fatal(err)
		}
	}
	return m, im
}

func fichero(t *testing.T, contenido string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "con espacio", "src")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(contenido), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := digest.File(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, d
}

func leerEnImagen(t *testing.T, im imagenDebugfs, p string) string {
	t.Helper()
	out, err := im.leer(context.Background(), "cat "+comillas(p))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

var (
	reModo  = regexp.MustCompile(`Mode:\s+(\d+)`)
	reDueño = regexp.MustCompile(`User:\s+(\d+)\s+Group:\s+(\d+)`)
	reLinks = regexp.MustCompile(`Links:\s+(\d+)`)
)

func statEnImagen(t *testing.T, im imagenDebugfs, p string) (modo, uid, gid, links string) {
	t.Helper()
	out, err := im.leer(context.Background(), "stat "+comillas(p))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if m := reModo.FindStringSubmatch(s); m != nil {
		modo = m[1]
	}
	if m := reDueño.FindStringSubmatch(s); m != nil {
		uid, gid = m[1], m[2]
	}
	if m := reLinks.FindStringSubmatch(s); m != nil {
		links = m[1]
	}
	return
}

func fsckLimpio(t *testing.T, im imagenDebugfs) {
	t.Helper()
	if out, err := e2fsCmd(context.Background(), "e2fsck", "-fn", im.file).CombinedOutput(); err != nil {
		t.Fatalf("e2fsck -fn: %v: %s", err, out)
	}
}

func TestPutDebugfsCreaConDirectoriosModoYDueño(t *testing.T) {
	m, im := ext4Prueba(t, 16)
	src, d := fichero(t, "#!/bin/sh\necho hola\n")
	ctx := context.Background()

	if _, err := m.intentarPutDebugfs(ctx, im.file, "/usr/local/bin/x", src, d, 0o755, false); !errors.Is(err, errNoBridge) {
		t.Fatalf("sin create, un fichero que no está es errNoBridge; fue %v", err)
	}
	cambio, err := m.intentarPutDebugfs(ctx, im.file, "/usr/local/bin/x", src, d, 0o755, true)
	if err != nil || !cambio {
		t.Fatalf("put: cambio=%v err=%v", cambio, err)
	}
	if got := leerEnImagen(t, im, "/usr/local/bin/x"); got != "#!/bin/sh\necho hola\n" {
		t.Fatalf("contenido %q", got)
	}
	if modo, uid, gid, _ := statEnImagen(t, im, "/usr/local/bin/x"); modo != "0755" || uid != "0" || gid != "0" {
		t.Fatalf("modo %s dueño %s:%s; quiero 0755 0:0", modo, uid, gid)
	}
	if modo, uid, _, _ := statEnImagen(t, im, "/usr/local"); modo != "0755" || uid != "0" {
		t.Fatalf("directorio creado con modo %s dueño %s", modo, uid)
	}
	if e, _ := im.stat(ctx, "/usr/local/bin/x.nuevo"); e.existe {
		t.Fatal("queda la copia de al lado")
	}
	// Lo mismo otra vez: nada que cambiar.
	if cambio, err := m.intentarPutDebugfs(ctx, im.file, "/usr/local/bin/x", src, d, 0o755, false); err != nil || cambio {
		t.Fatalf("el mismo contenido no cambia nada: cambio=%v err=%v", cambio, err)
	}
	fsckLimpio(t, im)
}

// Reemplazar conserva los demás enlaces duros del viejo, como el rename de
// Linux: el otro nombre sigue con el contenido de antes.
func TestPutDebugfsReemplazaConEnlaceDuro(t *testing.T) {
	viejo, _ := fichero(t, "viejo\n")
	m, im := ext4Prueba(t, 16,
		"mkdir /etc",
		"cd /etc",
		"write "+comillas(viejo)+" a",
		"ln a b",
		"sif a links_count 2",
	)
	src, d := fichero(t, "nuevo\n")
	cambio, err := m.intentarPutDebugfs(context.Background(), im.file, "/etc/a", src, d, 0o644, false)
	if err != nil || !cambio {
		t.Fatalf("put: cambio=%v err=%v", cambio, err)
	}
	if got := leerEnImagen(t, im, "/etc/a"); got != "nuevo\n" {
		t.Fatalf("/etc/a = %q", got)
	}
	if got := leerEnImagen(t, im, "/etc/b"); got != "viejo\n" {
		t.Fatalf("/etc/b = %q; el otro enlace conserva el contenido viejo", got)
	}
	if _, _, _, links := statEnImagen(t, im, "/etc/b"); links != "1" {
		t.Fatalf("/etc/b con %s enlaces; quiero 1", links)
	}
	fsckLimpio(t, im)
}

// Los enlaces simbólicos de los directorios se siguen, y siempre dentro de la
// imagen: ni un destino absoluto ni un ".." salen de ella.
func TestPutDebugfsSigueEnlacesSinSalir(t *testing.T) {
	m, im := ext4Prueba(t, 16,
		"mkdir /usr",
		"mkdir /usr/bin",
		"symlink /bin usr/bin",
		"symlink /abs /usr",
		"symlink /fuga ../../../..",
	)
	ctx := context.Background()
	for _, c := range []struct{ pedido, real string }{
		{"/bin/tool", "/usr/bin/tool"},
		{"/abs/bin/otra", "/usr/bin/otra"},
		{"/fuga/raiz", "/raiz"},
	} {
		src, d := fichero(t, c.pedido)
		if _, err := m.intentarPutDebugfs(ctx, im.file, c.pedido, src, d, 0o644, true); err != nil {
			t.Fatalf("%s: %v", c.pedido, err)
		}
		if got := leerEnImagen(t, im, c.real); got != c.pedido {
			t.Fatalf("%s no acabó en %s (%q)", c.pedido, c.real, got)
		}
	}
	if e, _ := im.stat(ctx, "/bin"); e.tipo != "symlink" {
		t.Fatal("el enlace /bin sigue siendo un enlace")
	}
	fsckLimpio(t, im)
}

func TestPutDebugfsRutasQueNoPasan(t *testing.T) {
	m, im := ext4Prueba(t, 16, "mkdir /d", "write /dev/null /f")
	src, d := fichero(t, "x")
	ctx := context.Background()
	for _, p := range []string{`/a"b`, "/a\nb", `/a\b`, "/"} {
		if _, err := m.intentarPutDebugfs(ctx, im.file, p, src, d, 0o644, true); err == nil {
			t.Errorf("%q se aceptó", p)
		}
	}
	if _, err := m.intentarPutDebugfs(ctx, im.file, "/d", src, d, 0o644, true); err == nil {
		t.Error("sobre un directorio no se escribe")
	}
	if _, err := m.intentarPutDebugfs(ctx, im.file, "/f/x", src, d, 0o644, true); err == nil {
		t.Error("un fichero no hace de directorio")
	}
}

// Sin hueco dentro, errSinHueco; putOne crece la imagen y lo pone.
func TestPutDebugfsSinHuecoCrece(t *testing.T) {
	if buscarE2fs("resize2fs") == "" && !putSinMontar {
		t.Skip("sin resize2fs")
	}
	m, im := ext4Prueba(t, 4)
	grande := strings.Repeat("k", 6<<20)
	src, d := fichero(t, grande)
	ctx := context.Background()
	var sinHueco errSinHueco
	if _, err := m.intentarPutDebugfs(ctx, im.file, "/grande", src, d, 0o644, true); !errors.As(err, &sinHueco) {
		t.Fatalf("quiero errSinHueco; fue %v", err)
	}
	if !putSinMontar {
		return // en Linux putOne monta: esto lo prueba el Mac
	}
	cambio, err := m.putOne(ctx, im.file, "/grande", src, d, 0o644, true)
	if err != nil || !cambio {
		t.Fatalf("putOne: cambio=%v err=%v", cambio, err)
	}
	if h, _ := im.sha256De(ctx, "/grande"); h != d {
		t.Fatal("el fichero grande no quedó entero")
	}
	fsckLimpio(t, im)
}

func TestQuejasDebugfs(t *testing.T) {
	out := "debugfs 1.47.4 (6-Mar-2025)\ndebugfs: write a b\nAllocated inode: 17\ndebugfs: cd /x\n/x: File not found by ext2_lookup \n"
	q := quejasDebugfs([]byte(out))
	if len(q) != 1 || !strings.Contains(q[0], "File not found") {
		t.Fatalf("quejas = %q", q)
	}
}
