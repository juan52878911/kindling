//go:build !darwin

package machine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"
)

func TestProyectoLibre(t *testing.T) {
	if got := proyectoLibre(nil); got != primerProyecto {
		t.Errorf("vacío: %d", got)
	}
	if got := proyectoLibre(map[uint32]bool{1000: true, 1001: true, 1003: true}); got != 1002 {
		t.Errorf("hueco: %d", got)
	}
}

func TestArgsLimiteXFS(t *testing.T) {
	got := strings.Join(argsLimiteXFS(1002, 1<<20+1, "/x/cow"), " ")
	if got != "-x -c limit -p bhard=1025k 1002 /x/cow" {
		t.Errorf("args: %q", got)
	}
}

func TestOpcionesConCuotaXFS(t *testing.T) {
	for opts, want := range map[string]bool{
		"rw,noexec,attr2,inode64,prjquota": true,
		"rw,pquota":                        true,
		"rw,inode64":                       false,
		"rw,prjquota,pqnoenforce":          false,
		"rw,uquota":                        false,
		"":                                 false,
	} {
		if got := opcionesConCuotaXFS(opts); got != want {
			t.Errorf("%q: %v, quería %v", opts, got, want)
		}
	}
	if !strings.Contains(opcionesMontaje("xfs"), "prjquota") {
		t.Error("el XFS se monta sin prjquota")
	}
}

// El overlay es un fichero regular: una ruta que sea un enlace no se sigue.
func TestAbrirSinSeguir(t *testing.T) {
	d := t.TempDir()
	real := filepath.Join(d, "real")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err := abrirSinSeguir(real); err != nil {
		t.Errorf("fichero regular: %v", err)
	} else {
		f.Close()
	}
	link := filepath.Join(d, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if f, err := abrirSinSeguir(link); err == nil {
		f.Close()
		t.Error("siguió un enlace")
	}
	if f, err := abrirSinSeguir(d); err == nil {
		f.Close()
		t.Error("aceptó un directorio")
	}
}

// almacenReal monta un almacén de verdad del tipo fs (root, KLING_TEST_MOUNTS=1).
func almacenReal(t *testing.T, fs string) *almacenCoW {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("KLING_TEST_MOUNTS") != "1" {
		t.Skip("needs root and KLING_TEST_MOUNTS=1")
	}
	if b, _ := os.ReadFile("/proc/filesystems"); !soportados(string(b))[fs] || buscarE2fs("mkfs."+fs) == "" {
		t.Skipf("needs %s in the kernel and mkfs.%s", fs, fs)
	}
	herramienta := map[string]string{"xfs": "xfs_quota", "btrfs": "btrfs"}[fs]
	if buscarE2fs(herramienta) == "" {
		t.Skipf("needs %s", herramienta)
	}
	a := nuevoAlmacen(t.TempDir(), &Privileges{})
	a.fs, a.img, a.errFS = fs, filepath.Join(a.root, imgAlmacen(fs)), nil
	a.estaMontado = func(dir string) (bool, error) { return estaMontadoTipo(dir, fs) }
	a.crear = func(ctx context.Context, img string, bytes int64) error {
		return crearImagenAlmacen(ctx, fs, img, bytes)
	}
	a.montar = func(ctx context.Context, img, dir string) error { return montarLoopAlmacen(ctx, fs, img, dir) }
	a.activarCuota()
	t.Cleanup(func() { _ = syscall.Unmount(a.dir, syscall.MNT_DETACH) })
	return a
}

func doradoDe(t *testing.T, dir string, mib int64) string {
	t.Helper()
	src := filepath.Join(dir, "dorado.ext4")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(mib << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("disco de verdad"), 10<<20); err != nil {
		t.Fatal(err)
	}
	return src
}

// La cuota de verdad: el overlay clonado no puede crecer por encima de su
// límite (EDQUOT) y otra instancia sigue pudiendo escribir; borrar libera.
func probarCuotaReal(t *testing.T, fs, cuota string) {
	a := almacenReal(t, fs)
	src := doradoDe(t, a.root, 64)
	ctx := context.Background()
	r1, err := a.clonarInstancia(ctx, "d", src, "id1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.cuota != cuota {
		t.Skipf("el almacén no impone cuota (%q): faltan opciones del núcleo o herramientas", a.cuota)
	}
	r2, err := a.clonarInstancia(ctx, "d", src, "id2", 1)
	if err != nil {
		t.Fatal(err)
	}
	// El clon lee lo de la base.
	g, err := os.Open(r1)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 15)
	if _, err := g.ReadAt(b, 10<<20); err != nil || string(b) != "disco de verdad" {
		t.Errorf("lee %q %v", b, err)
	}
	g.Close()

	// Crecer por encima de la cuota (tamaño lógico + holgura) falla. La cuota
	// cuenta bloques asignados y el dorado es disperso: hay que escribir más
	// que el límite entero, no solo pasar del tamaño lógico.
	w, err := os.OpenFile(r1, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	limite := cuotaInstancia(64 << 20)
	buf := make([]byte, 1<<20)
	var werr error
	for off := int64(64 << 20); off < 64<<20+limite+32<<20 && werr == nil; off += int64(len(buf)) {
		_, werr = w.WriteAt(buf, off)
	}
	w.Close()
	if !errors.Is(werr, syscall.EDQUOT) && !errors.Is(werr, syscall.ENOSPC) {
		t.Errorf("crecer el overlay por encima de la cuota: %v, quería EDQUOT", werr)
	}
	// Reescribir dentro de su tamaño lógico sigue valiendo, y la otra instancia
	// no se enteró.
	w, err = os.OpenFile(r2, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt(buf, 0); err != nil {
		t.Errorf("escribir dentro del tamaño lógico: %v", err)
	}
	w.Close()

	// Borrado y barrido.
	a.borrarInstancia("id1")
	if _, err := os.Lstat(a.dirInstancia("id1")); !os.IsNotExist(err) {
		t.Errorf("borrarInstancia: %v", err)
	}
	a.barrer(func(string) bool { return false }, func(string) string { return src }, func(string) string { return "" })
	if _, err := os.Lstat(a.dirInstancia("id2")); !os.IsNotExist(err) {
		t.Errorf("barrer: %v", err)
	}
}

func TestCuotaXFSDeVerdad(t *testing.T)   { probarCuotaReal(t, "xfs", "prjquota") }
func TestCuotaBtrfsDeVerdad(t *testing.T) { probarCuotaReal(t, "btrfs", "qgroup") }

// usuarioVMMPrueba es el usuario sin privilegios con el que corre el proceso
// hijo de TestCuotaXFSNoLaCambiaElVMM (nobody:nogroup en casi todo Linux).
const usuarioVMMPrueba = 65534

// TestAyudanteProyecto no es una prueba: es el proceso hijo que, como el
// usuario del VMM, abre el overlay en escritura (por grupo) e intenta sacarlo
// de su proyecto de XFS. Solo corre con KLING_TEST_AYUDANTE_PROYECTO.
func TestAyudanteProyecto(t *testing.T) {
	ruta := os.Getenv("KLING_TEST_AYUDANTE_PROYECTO")
	if ruta == "" {
		t.Skip("proceso hijo de TestCuotaXFSNoLaCambiaElVMM")
	}
	f, err := os.OpenFile(ruta, os.O_RDWR, 0)
	if err != nil {
		fmt.Println("RESULTADO no-abre", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte("x"), 0); err != nil {
		fmt.Println("RESULTADO no-escribe", err)
		return
	}
	switch err := fijarProyecto(f, 0); {
	case err == nil:
		fmt.Println("RESULTADO cambio-proyecto")
	case errors.Is(err, syscall.EPERM):
		fmt.Println("RESULTADO eperm")
	default:
		fmt.Println("RESULTADO otro", err)
	}
}

// De verdad (root, KLING_TEST_MOUNTS=1): con usuario sin privilegios, el
// overlay del almacén XFS es root:grupo-del-VMM 0660; el VMM lo abre y lo
// escribe, pero no puede cambiarle el id de proyecto (FS_IOC_FSSETXATTR da
// EPERM al que no es dueño) y el proyecto sigue siendo el que puso el daemon.
func TestCuotaXFSNoLaCambiaElVMM(t *testing.T) {
	a := almacenReal(t, "xfs")
	a.priv = &Privileges{Enabled: true, UID: usuarioVMMPrueba, GID: usuarioVMMPrueba}
	src := doradoDe(t, a.root, 64)
	ruta, err := a.clonarInstancia(context.Background(), "d", src, "id1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.cuota != "prjquota" {
		t.Skipf("el almacén no impone cuota (%q)", a.cuota)
	}
	f, err := abrirSinSeguir(ruta)
	if err != nil {
		t.Fatal(err)
	}
	antes, err := proyectoDe(f)
	f.Close()
	if err != nil || antes == 0 {
		t.Fatalf("proyecto del overlay %d %v", antes, err)
	}
	fi, err := os.Lstat(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != 0 || st.Gid != usuarioVMMPrueba || fi.Mode().Perm() != 0o660 {
		t.Fatalf("overlay %d:%d %v, quería 0:%d 0660", st.Uid, st.Gid, fi.Mode().Perm(), usuarioVMMPrueba)
	}

	// El hijo tiene que llegar al overlay y ejecutar el binario de la prueba:
	// se abren los directorios de la prueba (solo atravesar) y se copia el
	// binario a uno suyo.
	for d := a.root; d != filepath.Dir(d) && strings.HasPrefix(d, os.TempDir()) && d != os.TempDir(); d = filepath.Dir(d) {
		_ = os.Chmod(d, 0o711)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(a.root, "prueba.test")
	datos, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, datos, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestAyudanteProyecto$", "-test.v")
	cmd.Env = append(os.Environ(), "KLING_TEST_AYUDANTE_PROYECTO="+ruta)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: usuarioVMMPrueba, Gid: usuarioVMMPrueba, Groups: []uint32{},
	}}
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "RESULTADO eperm") {
		t.Errorf("el usuario del VMM sobre su overlay: %s", out)
	}
	f, err = abrirSinSeguir(ruta)
	if err != nil {
		t.Fatal(err)
	}
	despues, err := proyectoDe(f)
	f.Close()
	if err != nil || despues != antes {
		t.Errorf("proyecto %d → %d (%v)", antes, despues, err)
	}
}

// En XFS cada fichero de memoria de una copia (mem.full, mem.diff) tiene su
// propio proyecto. Un overlay nuevo no puede recibir el id de uno de ellos:
// su límite ya estaría gastado y el invitado vería EDQUOT.
func TestCuotaXFSMemoriaYOverlayNoComparten(t *testing.T) {
	a := almacenReal(t, "xfs")
	src := doradoDe(t, a.root, 64)
	ctx := context.Background()
	if _, err := a.clonarInstancia(ctx, "d", src, "id1", 1); err != nil {
		t.Fatal(err)
	}
	if a.cuota != "prjquota" {
		t.Skipf("el almacén no impone cuota (%q)", a.cuota)
	}
	mem := filepath.Join(a.dirInstancia("id1"), memFull)
	if err := os.WriteFile(mem, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.limitar(a.dirInstancia("id1"), mem, 8<<20); err != nil {
		t.Fatal(err)
	}
	f, err := abrirSinSeguir(mem)
	if err != nil {
		t.Fatal(err)
	}
	idMem, err := proyectoDe(f)
	f.Close()
	if err != nil || idMem == 0 {
		t.Fatalf("proyecto de mem.full: %d %v", idMem, err)
	}
	r2, err := a.clonarInstancia(ctx, "d", src, "id2", 1)
	if err != nil {
		t.Fatal(err)
	}
	g, err := abrirSinSeguir(r2)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if id2, err := proyectoDe(g); err != nil || id2 == idMem {
		t.Fatalf("el overlay nuevo tiene el proyecto %d (%v), el mismo que el mem.full de otra copia", id2, err)
	}
}

// 1 000 asignaciones concurrentes dan ids distintos; un id liberado se reutiliza.
func TestAsignadorProyectosUnicosYReutiliza(t *testing.T) {
	p := &asignadorProyectos{}
	const n = 1000
	ids := make([]uint32, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i] = p.asignar(fmt.Sprintf("d%d", i))
		}()
	}
	wg.Wait()
	vistos := map[uint32]bool{}
	for _, id := range ids {
		if id < primerProyecto || vistos[id] {
			t.Fatalf("id %d repetido o menor que %d", id, primerProyecto)
		}
		vistos[id] = true
	}
	liberado := ids[500]
	p.liberar("d500")
	if got := p.asignar("nuevo"); got != liberado {
		t.Errorf("tras liberar %d se asignó %d", liberado, got)
	}
	if got := p.asignar("otro"); vistos[got] {
		t.Errorf("id %d ya en uso", got)
	}
}

// Se siembra una vez del disco: los ids existentes no se reparten y se
// liberan con su directorio.
func TestAsignadorProyectosResiembra(t *testing.T) {
	llamadas := 0
	p := &asignadorProyectos{sembrar: func() (map[string][]uint32, error) {
		llamadas++
		return map[string][]uint32{"/x/a": {1000, 1001}, "/x/b": {1003}}, nil
	}}
	if got := p.asignar("/x/c"); got != 1002 {
		t.Errorf("primer id %d, quería 1002", got)
	}
	if got := p.asignar("/x/d"); got != 1004 {
		t.Errorf("segundo id %d, quería 1004", got)
	}
	p.liberar("/x/a")
	if got := p.asignar("/x/e"); got != 1000 {
		t.Errorf("tras liberar /x/a: %d, quería 1000", got)
	}
	if llamadas != 1 {
		t.Errorf("el escaneo se hizo %d veces", llamadas)
	}
}

func TestLimiteQuotactlXFS(t *testing.T) {
	if sz := unsafe.Sizeof(fsDiskQuota{}); sz != 112 {
		t.Errorf("struct fs_disk_quota de %d bytes, quería 112", sz)
	}
	q := limiteQuotactlXFS(1234, 1025)
	if q.BlkHard != 3 || q.ID != 1234 || q.Flags != 2 || q.Fieldmask != 8 || q.Version != 1 {
		t.Errorf("%+v", q)
	}
	if qXSetQLim != 0x580402 {
		t.Errorf("cmd %#x", qXSetQLim)
	}
}

// copiarDisco en proceso conserva el contenido y los huecos.
func TestCopiarDiscoConservaHuecos(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	f, err := os.OpenFile(src, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("inicio"), 1<<20)
	f.WriteAt([]byte("fin"), 40<<20)
	f.Truncate(64 << 20)
	f.Close()
	if err := copiarDispersoEnProceso(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	if !bytes.Equal(a, b) {
		t.Fatal("el contenido difiere")
	}
	blq := func(p string) int64 {
		var st syscall.Stat_t
		if err := syscall.Stat(p, &st); err != nil {
			t.Fatal(err)
		}
		return st.Blocks
	}
	// Bloques de 512 bytes: 2048 = 1 MiB, muy por debajo de los 64 MiB lógicos.
	if bs, bd := blq(src), blq(dst); bd > 2048 || bd > bs+2048 {
		t.Errorf("bloques asignados: src=%d dst=%d", bs, bd)
	}
	if out, err := copiarDisco(context.Background(), src, dst); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if c, _ := os.ReadFile(dst); !bytes.Equal(a, c) {
		t.Fatal("copiarDisco: el contenido difiere")
	}
}

// H7: recrear mem.full/mem.diff sin borrar la instancia pedía un id nuevo cada
// vez; el asignador recuerda el id por (directorio, fichero).
func TestAsignadorReutilizaIdPorFichero(t *testing.T) {
	p := &asignadorProyectos{}
	vistos := map[uint32]bool{}
	for range 1000 {
		for _, f := range []string{"overlay.ext4", "mem.full", "mem.diff"} {
			id, _ := p.asignarFichero("/x/a", f)
			vistos[id] = true
		}
	}
	if len(vistos) > 3 {
		t.Fatalf("%d ids distintos para 3 ficheros", len(vistos))
	}
	a, _ := p.asignarFichero("/x/a", "mem.full")
	b, _ := p.asignarFichero("/x/b", "mem.full")
	if a == b {
		t.Fatalf("dos instancias comparten el id %d", a)
	}
	p.liberar("/x/a")
	if len(p.porDir["/x/a"]) != 0 {
		t.Fatal("liberar no soltó los ids")
	}
}

// Un fallo pasajero (EINTR) no desactiva quotactl para siempre.
func TestQuotactlPasajeroNoEsRoto(t *testing.T) {
	llamadas := 0
	var err error = syscall.EINTR
	q := &quotactlXFS{dev: "/dev/x", llamar: func(string, uint32, int64) error { llamadas++; return err }}
	if q.fijar(1, 10) {
		t.Fatal("debía fallar")
	}
	err = nil
	if !q.fijar(1, 10) || llamadas != 2 {
		t.Fatalf("la segunda llamada no volvió a quotactl (llamadas=%d)", llamadas)
	}
	err = syscall.ENOSYS
	q.fijar(1, 10)
	if q.fijar(1, 10) || llamadas != 3 {
		t.Fatalf("ENOSYS debía ser permanente (llamadas=%d)", llamadas)
	}
}

// Si el escaneo de la siembra falló, no se da por hecha: se reintenta.
func TestAsignadorSiembraFallidaSeReintenta(t *testing.T) {
	llamadas := 0
	p := &asignadorProyectos{sembrar: func() (map[string][]uint32, error) {
		llamadas++
		if llamadas == 1 {
			return nil, syscall.EIO
		}
		return map[string][]uint32{"/x/a": {1000}}, nil
	}}
	// Con el escaneo fallido no se reparte nada: el id libre podría ser de
	// una instancia que no se llegó a leer (dos ficheros vivos con la misma
	// cuota).
	if id, err := p.asignarFichero("/x/b", "overlay.ext4"); err == nil || id != 0 {
		t.Fatalf("con el escaneo fallido se repartió %d (err %v)", id, err)
	}
	if got := p.asignar("/x/c"); got == 1000 {
		t.Fatalf("se repartió el id 1000, ya usado por /x/a")
	}
	if llamadas < 2 {
		t.Fatalf("el escaneo se hizo %d veces", llamadas)
	}
}
