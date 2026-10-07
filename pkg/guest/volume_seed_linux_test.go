//go:build linux

package guest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// nuevoVolumen imita la raíz de un ext4 recién hecho: root 0755 y un
// lost+found vacío.
func nuevoVolumen(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func statT(t *testing.T, path string) syscall.Stat_t {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// mustChmod con syscall: os.Chmod no entiende 0o2770 como setgid.
func mustChmod(t *testing.T, path string, mode uint32) {
	t.Helper()
	if err := syscall.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// imagenGrafana monta un directorio como el /var/lib/grafana de la imagen:
// de un usuario sin privilegios (si el test corre como root), con un
// subdirectorio, un fichero, un enlace que no hay que seguir y una fifo.
func imagenGrafana(t *testing.T) string {
	t.Helper()
	return imagenGrafanaEn(t, t.TempDir())
}

func imagenGrafanaEn(t *testing.T, parent string) string {
	t.Helper()
	src := filepath.Join(parent, "grafana")
	if err := os.MkdirAll(filepath.Join(src, "plugins", "ro"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "plugins", "ro", "a.txt"), []byte("hola"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "grafana.ini"), []byte("[server]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "fuera")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(src, "tubo"), 0o620); err != nil {
		t.Fatal(err)
	}
	mustChmod(t, filepath.Join(src, "plugins", "ro"), 0o555)
	mustChmod(t, filepath.Join(src, "grafana.ini"), 0o640)
	mustChmod(t, filepath.Join(src, "tubo"), 0o620)
	mustChmod(t, src, 0o2770)
	if os.Geteuid() == 0 {
		filepath.Walk(src, func(p string, _ os.FileInfo, _ error) error {
			return os.Lchown(p, 472, 0)
		})
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(src, "plugins", "ro"), 0o755) })
	return src
}

func TestSeedVolumeCopiesTheImageDir(t *testing.T) {
	src := imagenGrafana(t)
	root := nuevoVolumen(t)
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "plugins", "ro"), 0o755) })

	img := statImageDir(src)
	if img == nil || !img.content {
		t.Fatalf("statImageDir = %+v", img)
	}
	seeded, err := seedVolume(root, img.path, img)
	if err != nil || !seeded {
		t.Fatalf("seedVolume = %v, %v", seeded, err)
	}

	st := statT(t, root)
	if st.Mode&0o7777 != 0o2770 {
		t.Errorf("root mode = %04o, want 2770", st.Mode&0o7777)
	}
	if os.Geteuid() == 0 && (st.Uid != 472 || st.Gid != 0) {
		t.Errorf("root owner = %d:%d, want 472:0", st.Uid, st.Gid)
	}
	if b, err := os.ReadFile(filepath.Join(root, "plugins", "ro", "a.txt")); err != nil || string(b) != "hola" {
		t.Errorf("a.txt = %q, %v", b, err)
	}
	if st := statT(t, filepath.Join(root, "plugins", "ro")); st.Mode&0o7777 != 0o555 {
		t.Errorf("plugins/ro mode = %04o, want 0555", st.Mode&0o7777)
	}
	ini := statT(t, filepath.Join(root, "grafana.ini"))
	if ini.Mode&0o7777 != 0o640 {
		t.Errorf("grafana.ini mode = %04o, want 0640", ini.Mode&0o7777)
	}
	if os.Geteuid() == 0 && ini.Uid != 472 {
		t.Errorf("grafana.ini uid = %d, want 472", ini.Uid)
	}
	if want := statT(t, filepath.Join(src, "grafana.ini")); ini.Mtim != want.Mtim {
		t.Errorf("grafana.ini mtime = %v, want %v", ini.Mtim, want.Mtim)
	}
	if l, err := os.Readlink(filepath.Join(root, "fuera")); err != nil || l != "/etc/passwd" {
		t.Errorf("symlink = %q, %v (must be copied, not followed)", l, err)
	}
	if st := statT(t, filepath.Join(root, "tubo")); st.Mode&syscall.S_IFMT != syscall.S_IFIFO || st.Mode&0o7777 != 0o620 {
		t.Errorf("fifo mode = %o", st.Mode)
	}
	if _, err := os.Stat(filepath.Join(root, seedTmp)); !os.IsNotExist(err) {
		t.Errorf("%s left behind: %v", seedTmp, err)
	}
	if _, err := os.Stat(filepath.Join(root, "lost+found")); err != nil {
		t.Errorf("lost+found gone: %v", err)
	}
}

// Un volumen con datos es del usuario: ni dueño, ni modo, ni contenido.
func TestSeedVolumeLeavesDataAlone(t *testing.T) {
	src := imagenGrafana(t)
	root := nuevoVolumen(t)
	if err := os.WriteFile(filepath.Join(root, "grafana.db"), []byte("mío"), 0o644); err != nil {
		t.Fatal(err)
	}
	seeded, err := seedVolume(root, src, statImageDir(src))
	if err != nil || seeded {
		t.Fatalf("seedVolume = %v, %v; want untouched", seeded, err)
	}
	if st := statT(t, root); st.Mode&0o7777 != 0o755 {
		t.Errorf("root mode changed to %04o", st.Mode&0o7777)
	}
	if _, err := os.Lstat(filepath.Join(root, "grafana.ini")); !os.IsNotExist(err) {
		t.Errorf("image content copied over user data: %v", err)
	}
}

// Una copia interrumpida deja el volumen virgen: el siguiente arranque la tira
// y la rehace.
func TestSeedVolumeRedoesAnInterruptedCopy(t *testing.T) {
	src := imagenGrafana(t)
	root := nuevoVolumen(t)
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "plugins", "ro"), 0o755) })
	if err := os.MkdirAll(filepath.Join(root, seedTmp, "a-medias"), 0o700); err != nil {
		t.Fatal(err)
	}
	seeded, err := seedVolume(root, src, statImageDir(src))
	if err != nil || !seeded {
		t.Fatalf("seedVolume = %v, %v", seeded, err)
	}
	if _, err := os.Stat(filepath.Join(root, "grafana.ini")); err != nil {
		t.Errorf("grafana.ini not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, seedTmp)); !os.IsNotExist(err) {
		t.Errorf("%s left behind: %v", seedTmp, err)
	}
}

// Lo que pasa de los límites no se copia, pero la raíz hereda dueño y modo.
func TestSeedVolumeOnlyOwnerAndModeWhenTooBig(t *testing.T) {
	src := filepath.Join(t.TempDir(), "img")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(src, "grande"))
	if err != nil {
		t.Fatal(err)
	}
	// Disperso: el límite cuenta el tamaño aparente, no lo que ocupa.
	if err := f.Truncate(seedMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	mustChmod(t, src, 0o770)
	root := nuevoVolumen(t)
	seeded, err := seedVolume(root, src, statImageDir(src))
	if err != nil || !seeded {
		t.Fatalf("seedVolume = %v, %v", seeded, err)
	}
	if st := statT(t, root); st.Mode&0o7777 != 0o770 {
		t.Errorf("root mode = %04o, want 0770", st.Mode&0o7777)
	}
	if _, err := os.Lstat(filepath.Join(root, "grande")); !os.IsNotExist(err) {
		t.Errorf("oversized content copied: %v", err)
	}
}

// Un directorio vacío de la imagen se hereda sin copiar nada (el camino que
// no monta dos veces), y uno que no existe no hereda nada.
func TestSeedVolumeOwnerOnly(t *testing.T) {
	src := filepath.Join(t.TempDir(), "vacio")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	mustChmod(t, src, 0o1777)
	img := statImageDir(src)
	if img == nil || img.content {
		t.Fatalf("statImageDir = %+v", img)
	}
	root := nuevoVolumen(t)
	if seeded, err := seedVolume(root, "", img); err != nil || !seeded {
		t.Fatalf("seedVolume = %v, %v", seeded, err)
	}
	if st := statT(t, root); st.Mode&0o7777 != 0o1777 {
		t.Errorf("root mode = %04o, want 1777", st.Mode&0o7777)
	}
	if statImageDir(filepath.Join(src, "no-existe")) != nil {
		t.Error("statImageDir of a missing dir must be nil")
	}
}

// El lost+found que traiga la imagen no pisa el del sistema de ficheros.
func TestSeedVolumeSkipsImageLostFound(t *testing.T) {
	src := filepath.Join(t.TempDir(), "img")
	if err := os.MkdirAll(filepath.Join(src, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "lost+found", "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	root := nuevoVolumen(t)
	if _, err := seedVolume(root, src, statImageDir(src)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "lost+found", "x")); !os.IsNotExist(err) {
		t.Errorf("image lost+found copied: %v", err)
	}
}

// loopExt4 hace un ext4 de 32 MiB en un loop y devuelve el dispositivo. Salta
// el test si no corre como root o faltan las herramientas.
func loopExt4(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root (loop mounts)")
	}
	for _, c := range []string{"mkfs.ext4", "losetup"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skipf("no %s on this host", c)
		}
	}
	disk := filepath.Join(t.TempDir(), "vol.ext4")
	if err := os.WriteFile(disk, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(disk, 32<<20); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", disk).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v\n%s", err, out)
	}
	out, err := exec.Command("losetup", "-f", "--show", disk).Output()
	if err != nil {
		t.Fatalf("losetup: %v", err)
	}
	dev := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command("losetup", "-d", dev).Run() })
	return dev
}

// mountExt4 monta un loopExt4 nuevo en un directorio temporal y lo desmonta al
// acabar el test.
func mountExt4(t *testing.T) string {
	t.Helper()
	dev := loopExt4(t)
	mnt := t.TempDir()
	if err := syscall.Mount(dev, mnt, "ext4", 0, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Unmount(mnt, syscall.MNT_DETACH) })
	return mnt
}

// La raíz del invitado es un overlay de dos ext4 sin xino (minimal-init.sh y un
// kernel sin CONFIG_OVERLAY_FS_XINO_AUTO). Ahí solo los directorios llevan el
// st_dev del overlay: los ficheros, los enlaces y las fifos llevan el de la
// capa de abajo, y nada de eso puede tomarse por otro montaje.
func TestSeedVolumeFromOverlayWithoutXino(t *testing.T) {
	lower := mountExt4(t)
	upper := mountExt4(t)
	src := imagenGrafanaEn(t, lower)
	for _, d := range []string{"u", "w"} {
		if err := os.Mkdir(filepath.Join(upper, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	merged := t.TempDir()
	opts := "lowerdir=" + lower + ",upperdir=" + filepath.Join(upper, "u") +
		",workdir=" + filepath.Join(upper, "w") + ",xino=off"
	if err := syscall.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		t.Skipf("no overlayfs with xino=off: %v", err)
	}
	t.Cleanup(func() { syscall.Unmount(merged, syscall.MNT_DETACH) })
	src = filepath.Join(merged, filepath.Base(src))
	if dir, file := statT(t, src), statT(t, filepath.Join(src, "grafana.ini")); dir.Dev == file.Dev {
		t.Logf("this kernel gives files the overlay's st_dev (dir %d, file %d)", dir.Dev, file.Dev)
	}

	root := nuevoVolumen(t)
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "plugins", "ro"), 0o755) })
	img := statImageDir(src)
	if seeded, err := seedVolume(root, img.path, img); err != nil || !seeded {
		t.Fatalf("seedVolume = %v, %v", seeded, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "grafana.ini")); err != nil || string(b) != "[server]\n" {
		t.Errorf("file from the overlay's lower layer not copied: %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "plugins", "ro", "a.txt")); err != nil || string(b) != "hola" {
		t.Errorf("nested file not copied: %q, %v", b, err)
	}
	if l, err := os.Readlink(filepath.Join(root, "fuera")); err != nil || l != "/etc/passwd" {
		t.Errorf("symlink not copied: %q, %v", l, err)
	}
	if st := statT(t, filepath.Join(root, "tubo")); st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		t.Errorf("fifo not copied: mode %o", st.Mode)
	}
}

// El camino de verdad: un ext4 recién hecho, en un loop, montado aparte para
// copiar y desmontado después. Necesita root.
func TestSeedFromImageOnExt4(t *testing.T) {
	dev := loopExt4(t)

	src := imagenGrafana(t)
	seedFromImage(dev, statImageDir(src))

	mnt := t.TempDir()
	if err := syscall.Mount(dev, mnt, "ext4", 0, ""); err != nil {
		t.Fatal(err)
	}
	if st := statT(t, mnt); st.Mode&0o7777 != 0o2770 || st.Uid != 472 {
		t.Errorf("volume root = %d %04o, want 472 2770", st.Uid, st.Mode&0o7777)
	}
	if b, err := os.ReadFile(filepath.Join(mnt, "plugins", "ro", "a.txt")); err != nil || string(b) != "hola" {
		t.Errorf("a.txt = %q, %v", b, err)
	}
	// Con datos ya no es virgen: otra imagen no lo toca.
	if err := os.WriteFile(filepath.Join(mnt, "grafana.db"), []byte("mío"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Unmount(mnt, 0); err != nil {
		t.Fatal(err)
	}
	otra := t.TempDir()
	if err := os.WriteFile(filepath.Join(otra, "grafana.db"), []byte("de la imagen"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedFromImage(dev, statImageDir(otra))
	if err := syscall.Mount(dev, mnt, "ext4", 0, ""); err != nil {
		t.Fatal(err)
	}
	defer syscall.Unmount(mnt, 0)
	if b, _ := os.ReadFile(filepath.Join(mnt, "grafana.db")); string(b) != "mío" {
		t.Errorf("grafana.db = %q: a volume with data was overwritten", b)
	}
}
