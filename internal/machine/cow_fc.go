//go:build !darwin

package machine

// Lo que toca el sistema en las copias de disco (ver cow.go): FICLONE, el
// almacén XFS por loop y los binds de su directorio dentro del jail. Linux.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/durable"
)

// ioctlFICLONE es FICLONE (_IOW(0x94, 9, int)), igual en amd64 y arm64. Sin
// golang.org/x/sys: el núcleo no tiene dependencias externas.
const ioctlFICLONE = 0x40049409

// clonarFichero crea dst como un clon de src que comparte sus bloques
// (FICLONE): no copia nada, y cada fichero se separa del otro en cuanto
// alguno escribe. Falla sin dejar dst si el sistema de ficheros no clona
// (ext4: EOPNOTSUPP; ficheros en sistemas distintos: EXDEV).
func clonarFichero(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ioctlFICLONE, in.Fd())
	cerr := out.Close()
	if e != 0 {
		_ = os.Remove(dst)
		return &os.PathError{Op: "ficlone", Path: dst, Err: e}
	}
	if cerr != nil {
		_ = os.Remove(dst)
		return cerr
	}
	return nil
}

// probarReflink clona un fichero pequeño de srcDir a dstDir: la única forma
// fiable de saber si hay reflink entre los dos (el tipo de sistema de ficheros
// no basta: un XFS formateado sin reflink=1 no clona).
func probarReflink(srcDir, dstDir string) error {
	n := fmt.Sprintf(".cow-probe-%d", time.Now().UnixNano())
	src, dst := filepath.Join(srcDir, n), filepath.Join(dstDir, n+"-clone")
	defer os.Remove(src)
	defer os.Remove(dst)
	if err := os.WriteFile(src, make([]byte, 4096), 0o600); err != nil {
		return err
	}
	return clonarFichero(src, dst)
}

// nuevoAlmacen prepara (sin crear ni montar nada) el almacén de root.
func nuevoAlmacen(root string, priv *Privileges) *almacenCoW {
	// mountinfo da rutas absolutas y sin enlaces: con una raíz relativa o con
	// symlinks la comparación textual del punto de montaje nunca coincidiría.
	root = rutaCanonica(root)
	return &almacenCoW{
		root: root, img: filepath.Join(root, "cow.xfs"), dir: filepath.Join(root, "cow"), priv: priv,
		estaMontado: estaMontadoXFS,
		crear:       crearImagenXFS,
		montar:      montarLoopXFS,
		clonar:      clonarFichero,
		copiar: func(ctx context.Context, src, dst string) error {
			if out, err := copiarDisco(ctx, src, dst); err != nil {
				return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		libreEn: libreEnDir,
	}
}

// detectarCoW decide el modo en este host (ver decidirCoW).
func (m *Manager) detectarCoW(cfg CoWConfig) (string, string) {
	if cfg.Mode == CoWOff {
		return decidirCoW(cfg.Mode, false, nil)
	}
	src := filepath.Join(m.root, "snapshots")
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		src = filepath.Join(m.root, "machines")
	}
	nativo := probarReflink(src, filepath.Join(m.root, "machines")) == nil
	var errAlm error
	switch {
	case m.alm == nil:
		errAlm = errors.New("no store on this platform")
	case m.alm.existe():
		m.alm.mu.Lock()
		errAlm = m.alm.asegurarMontado(context.Background())
		m.alm.mu.Unlock()
	default:
		errAlm = puedeAlmacen()
	}
	return decidirCoW(cfg.Mode, nativo, errAlm)
}

// puedeAlmacen comprueba lo que hace falta para crear el almacén: root, loop
// y mkfs.xfs. Que el núcleo sepa montar XFS solo se sabe montando: si no,
// el primer run -from lo descubre y el daemon vuelve a copiar (con aviso).
func puedeAlmacen() error {
	if os.Geteuid() != 0 {
		return errors.New("the store needs the daemon to run as root")
	}
	if _, err := os.Stat("/dev/loop-control"); err != nil {
		return errors.New("no loop devices (/dev/loop-control)")
	}
	if buscarE2fs("mkfs.xfs") == "" {
		return errors.New("mkfs.xfs not found (install xfsprogs)")
	}
	if _, err := exec.LookPath("mount"); err != nil {
		if _, err := os.Stat("/bin/mount"); err != nil {
			return errors.New("mount not found")
		}
	}
	return nil
}

// crearImagenXFS reserva el fichero entero (fallocate: sin sobreasignar, ver
// tamAlmacen) y lo formatea XFS con reflink. Se hace en un temporal y se
// renombra: un cow.xfs a medias no puede quedar con su nombre.
func crearImagenXFS(ctx context.Context, img string, bytes int64) error {
	mkfs := buscarE2fs("mkfs.xfs")
	if mkfs == "" {
		return errors.New("mkfs.xfs not found (install xfsprogs)")
	}
	tmp := img + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, bytes); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("reserving %d MiB for the store: %w", bytes>>20, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	out, err := exec.CommandContext(ctx, mkfs, "-q", "-m", "reflink=1", "-L", "kling-cow", tmp).CombinedOutput()
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mkfs.xfs: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := durable.Renombrar(tmp, img); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// montarLoopXFS monta el almacén. nodev, nosuid y noexec: dentro solo hay
// discos de microVM, que ningún proceso del anfitrión tiene por qué ejecutar.
func montarLoopXFS(ctx context.Context, img, dir string) error {
	out, err := exec.CommandContext(ctx, "mount", "-t", "xfs", "-o", "loop,nodev,nosuid,noexec", img, dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mounting %s: %v: %s", img, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// estaMontadoXFS dice si dir es un punto de montaje, y exige que sea XFS: si
// alguien montó otra cosa encima, no es el almacén.
func estaMontadoXFS(dir string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()
	ms, err := parsearMountinfo(f)
	if err != nil {
		return false, err
	}
	tipo := ""
	dir = rutaCanonica(dir)
	for _, mt := range ms {
		if mt.punto == dir {
			tipo = mt.fstype // el último montaje sobre la ruta es el visible
		}
	}
	switch tipo {
	case "":
		return false, nil
	case "xfs":
		return true, nil
	}
	return false, fmt.Errorf("%s is mounted, but it is %s and not the XFS store", dir, tipo)
}

func libreEnDir(dir string) (total, libre int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Blocks) * st.Bsize, int64(st.Bavail) * st.Bsize, nil
}

// ── el almacén dentro del jail ────────────────────────────────────────────────

// dirBindJail es dónde cae, dentro del jail de id, el directorio de su
// overlay en el almacén: en su misma ruta absoluta, como todo en el jail.
func (m *Manager) dirBindJail(id string) string {
	return filepath.Join(m.jailRoot(id), m.alm.dirInstancia(id))
}

// prepararBindsJail monta, dentro del chroot de id y ANTES de lanzar jailer,
// el directorio de su overlay en el almacén.
//
// El jail recibe sus ficheros por hardlink (linkAbs), y un hardlink no cruza
// de sistema de ficheros: el overlay del almacén no puede entrar así. Entra el
// enlace simbólico de machines/<id>, que apunta a la ruta absoluta del
// almacén, y esa ruta resuelve dentro del chroot a este bind. Solo el
// directorio de ESTA instancia: el almacén entero expondría los discos de
// todas a un VMM comprometido.
//
// Antes de lanzar jailer y no después: jailer entra en su propio espacio de
// montajes y hace un bind RECURSIVO del chroot sobre sí mismo antes del
// pivot_root, así que se lleva los montajes que ya haya debajo. Uno hecho
// después dependería de la propagación del montaje del anfitrión.
func (m *Manager) prepararBindsJail(id string) error {
	if m.alm == nil {
		return nil
	}
	src := m.alm.dirInstancia(id)
	if fi, err := os.Lstat(src); err != nil || !fi.IsDir() {
		return nil // su overlay no está en el almacén
	}
	dst := m.dirBindJail(id)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("binding the overlay of %s from the copy-on-write store into its jail: %w", shortID(id), err)
	}
	return nil
}

// borrarJail borra el chroot de id, desmontando antes el bind del almacén.
// Si el bind sigue montado NO se borra: RemoveAll no se para en los puntos de
// montaje y se llevaría por delante el overlay de la instancia.
func (m *Manager) borrarJail(id string) error {
	base := filepath.Join(m.jailBase(), "firecracker", id)
	if m.alm != nil {
		dst := m.dirBindJail(id)
		for i := 0; ; i++ {
			encima, err := montadoEncima(dst)
			if err != nil {
				return fmt.Errorf("checking the store bind in the jail of %s: %w", shortID(id), err)
			}
			if !encima {
				break
			}
			if i == 8 {
				return fmt.Errorf("the store bind in the jail of %s is still mounted (%s): not removing the jail", shortID(id), dst)
			}
			if err := syscall.Unmount(dst, syscall.MNT_DETACH); err != nil {
				return fmt.Errorf("unmounting the store bind in the jail of %s: %w", shortID(id), err)
			}
		}
	}
	return os.RemoveAll(base)
}

// barrerBindsJail desmonta, al arrancar, los binds del almacén que quedaron en
// jails de máquinas que ya no existen (un daemon que murió a medias). Los de
// máquinas que siguen existiendo se quedan: su VMM puede estar vivo, y
// desmontar en el anfitrión se propagaría a su jail.
func (m *Manager) barrerBindsJail() {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return
	}
	ms, err := parsearMountinfo(f)
	f.Close()
	if err != nil {
		return
	}
	prefijo := filepath.Join(m.jailBase(), "firecracker") + "/"
	for _, mt := range ms {
		resto, ok := strings.CutPrefix(mt.punto, prefijo)
		if !ok {
			continue
		}
		id, _, _ := strings.Cut(resto, "/")
		if nombreSeguro(id) != nil {
			continue
		}
		if _, err := os.Lstat(m.dir(id)); !os.IsNotExist(err) {
			continue
		}
		if err := syscall.Unmount(mt.punto, syscall.MNT_DETACH); err != nil {
			log.Printf("warning: couldn't unmount the leftover %s: %v", mt.punto, err)
		}
	}
}
