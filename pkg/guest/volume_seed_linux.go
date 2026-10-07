//go:build linux

package guest

// Un volumen nuevo hereda el directorio de la imagen, como en Docker.
//
// mke2fs deja la raíz del ext4 como root:root 0755 con un lost+found y nada
// más. Montado tal cual sobre un directorio de la imagen, tapa lo que la imagen
// traía allí: el dueño, el modo y el contenido. Un servicio que no corre como
// root (grafana: USER 472 y VOLUME /var/lib/grafana) se encuentra entonces un
// directorio en el que no puede escribir, y no arranca.
//
// Docker resuelve esto rellenando un volumen VACÍO con lo que la imagen tiene en
// ese punto, dueño y modo incluidos. Aquí se hace lo mismo con dos reglas que no
// se negocian: solo un volumen virgen (nada más que lost+found) y nunca uno de
// solo lectura. Un volumen con datos es del usuario, aunque lo que tenga sea
// basura.
//
// Y solo una vez: al acabar se deja una marca dentro de lost+found (seedMark).
// Sin ella, cada arranque de un servicio que aún no ha escrito nada volvería a
// recorrer la imagen y a poner el dueño y el modo, deshaciendo un chmod que el
// usuario hiciera en la raíz. Va en lost+found y no en la raíz porque ahí no la
// ve nadie: un initdb que exige un directorio vacío no la encuentra.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

const (
	// seedMaxBytes acota cuánto contenido de la imagen se copia a un volumen
	// nuevo. Es lo que cabe en un arranque sin que nadie lo note; por encima,
	// el volumen recibe solo el dueño y el modo, que es lo que hace falta para
	// que el servicio pueda escribir.
	seedMaxBytes = 64 << 20
	// seedMaxEntries acota lo mismo por número de entradas: un árbol de
	// millones de ficheros vacíos no pesa nada y tarda igual.
	seedMaxEntries = 65536
	// seedTmp es donde se copia antes de dar nada por bueno. Un volumen que solo
	// tiene lost+found y esto sigue siendo virgen: si la máquina muere a media
	// copia, el siguiente arranque lo borra y empieza de nuevo, en vez de dejar
	// para siempre un volumen a medio rellenar.
	seedTmp = ".kling-seed"
	// seedMark, dentro de lost+found, dice que el volumen ya heredó lo suyo.
	seedMark = ".kling-seeded"
)

// imageDir es lo que la imagen tiene en un punto de montaje, leído ANTES de
// montar encima.
type imageDir struct {
	path     string // ya sin enlaces: donde lo deja el montaje
	uid, gid uint32
	mode     uint32 // permisos con setuid, setgid y sticky
	dev      uint64
	content  bool
}

// statImageDir lee el directorio de la imagen en mount. Devuelve nil si no
// existe o no es un directorio: entonces lo crea MkdirAll y no hay nada que
// heredar, igual que antes.
func statImageDir(mount string) *imageDir {
	path, err := filepath.EvalSymlinks(mount)
	if err != nil {
		return nil
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return nil
	}
	d := &imageDir{path: path, uid: st.Uid, gid: st.Gid, mode: st.Mode & 0o7777, dev: uint64(st.Dev)}
	if f, err := os.Open(path); err == nil {
		names, _ := f.Readdirnames(1)
		f.Close()
		d.content = len(names) > 0
	}
	return d
}

// volumeIsVirgin dice si la raíz de un volumen no tiene más que lo que deja
// mke2fs (lost+found) y, como mucho, los restos de una copia interrumpida.
func volumeIsVirgin(root string) (bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		switch {
		case e.Name() == "lost+found" && e.IsDir():
		case e.Name() == seedTmp && e.IsDir():
		default:
			return false, nil
		}
	}
	return true, nil
}

// volumeNeedsSeed dice si un volumen aún tiene que heredar el directorio de la
// imagen: virgen y sin la marca de haberlo hecho ya.
func volumeNeedsSeed(root string) (bool, error) {
	virgin, err := volumeIsVirgin(root)
	if err != nil || !virgin {
		return false, err
	}
	_, err = os.Lstat(filepath.Join(root, "lost+found", seedMark))
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return false, err
}

// markSeeded deja la marca de volumeNeedsSeed. Sin lost+found (alguien lo
// borró) no hay dónde ponerla, y el volumen se trata como antes: virgen hasta
// que tenga algo.
func markSeeded(root string) error {
	f, err := os.OpenFile(filepath.Join(root, "lost+found", seedMark),
		os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// seedVolume hace que la raíz de un volumen virgen montado en root herede el
// dueño y el modo de img y, si src no está vacío y pesa poco, una copia de su
// contenido. Un volumen con datos, o que ya heredó, no se toca.
//
// Devuelve si lo tocó. Un fallo copiando el contenido no es un error: se avisa,
// se deshace la copia y el volumen se queda con el dueño y el modo, que es lo
// que el servicio necesita para escribir; no se marca, y el siguiente arranque
// lo reintenta.
func seedVolume(root, src string, img *imageDir) (bool, error) {
	needs, err := volumeNeedsSeed(root)
	if err != nil || !needs {
		return false, err
	}
	tmp := filepath.Join(root, seedTmp)
	if err := os.RemoveAll(tmp); err != nil {
		return false, err
	}
	copied, done := false, true
	if src != "" {
		if fits, err := seedFits(src, img.dev); err != nil {
			log.Printf("volume %s: not copying the image's content: %v", root, err)
			done = false
		} else if !fits {
			log.Printf("volume %s: the image's content is over %d MiB or %d entries; "+
				"the volume gets only its owner and mode", root, seedMaxBytes>>20, seedMaxEntries)
		} else if err := copyTree(src, tmp, img.dev); err != nil {
			log.Printf("volume %s: copying the image's content: %v", root, err)
			os.RemoveAll(tmp)
			done = false
		} else {
			copied = true
		}
	}
	// Dueño y modo ANTES de sacar la copia a la raíz: si la máquina muere entre
	// medias, la raíz sigue teniendo solo lost+found y seedTmp, y el siguiente
	// arranque lo rehace entero. Al revés quedaría un volumen con datos y una
	// raíz de root en la que el servicio no puede escribir.
	if err := os.Lchown(root, int(img.uid), int(img.gid)); err != nil {
		return false, err
	}
	if err := syscall.Chmod(root, img.mode); err != nil {
		return false, err
	}
	if copied {
		entries, err := os.ReadDir(tmp)
		if err != nil {
			return false, err
		}
		for _, e := range entries {
			if err := os.Rename(filepath.Join(tmp, e.Name()), filepath.Join(root, e.Name())); err != nil {
				return false, err
			}
		}
		if err := os.Remove(tmp); err != nil {
			return false, err
		}
	}
	if done {
		return true, markSeeded(root)
	}
	return true, nil
}

// seedSkip dice qué entradas de primer nivel de la imagen no se copian:
// lost+found pisaría el del sistema de ficheros, y seedTmp es nuestro.
func seedSkip(rel string) bool {
	return rel == "lost+found" || rel == seedTmp
}

// seedFits recorre src sin seguir enlaces y dice si cabe en los límites.
func seedFits(src string, dev uint64) (bool, error) {
	var bytes, entries int64
	errBig := errors.New("too big")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return nil
		}
		if seedSkip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		var st syscall.Stat_t
		if err := syscall.Lstat(path, &st); err != nil {
			return err
		}
		if isOtherMount(&st, dev) {
			return filepath.SkipDir
		}
		entries++
		if st.Mode&syscall.S_IFMT == syscall.S_IFREG {
			bytes += st.Size
		}
		if bytes > seedMaxBytes || entries > seedMaxEntries {
			return errBig
		}
		return nil
	})
	if errors.Is(err, errBig) {
		return false, nil
	}
	return err == nil, err
}

// isOtherMount dice si la entrada es la raíz de otro montaje colgado dentro del
// directorio de la imagen, que no es de la imagen y no se copia.
//
// Solo mira los directorios, que es donde va un punto de montaje. Mirar el
// st_dev de todo dejaría el volumen sin un solo fichero: la raíz del invitado
// es un overlay sin xino (el kernel no trae CONFIG_OVERLAY_FS_XINO_AUTO), y ahí
// los directorios llevan el st_dev del overlay pero los ficheros, enlaces y
// fifos el de la capa de abajo. Un fichero suelto montado con bind se copiaría
// con lo que el bind enseña; en el arranque de una máquina no los hay.
func isOtherMount(st *syscall.Stat_t, dev uint64) bool {
	return st.Mode&syscall.S_IFMT == syscall.S_IFDIR && uint64(st.Dev) != dev
}

// copyTree copia src en dst (que no debe existir) conservando tipo, dueño,
// modo y fechas, sin seguir nunca un enlace. Los montajes colgados dentro de src (ver
// isOtherMount) y los sockets se omiten; los enlaces duros
// se copian como ficheros independientes. Los atributos extendidos no se
// copian.
func copyTree(src, dst string, dev uint64) error {
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	// Los directorios reciben dueño, modo y fechas al FINAL, del más hondo al
	// más alto: uno de solo lectura (0555) no dejaría crear lo de dentro, y
	// crear lo de dentro le cambia la fecha.
	type dirMeta struct {
		path string
		st   syscall.Stat_t
	}
	var dirs []dirMeta
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return nil
		}
		if seedSkip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		var st syscall.Stat_t
		if err := syscall.Lstat(path, &st); err != nil {
			return err
		}
		if isOtherMount(&st, dev) {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		switch st.Mode & syscall.S_IFMT {
		case syscall.S_IFDIR:
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirMeta{target, st})
			return nil
		case syscall.S_IFREG:
			if err := copyFileNoFollow(path, target); err != nil {
				return err
			}
		case syscall.S_IFLNK:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			// Un enlace no tiene modo propio, y cambiarle la fecha sin
			// seguirlo pide utimensat con AT_SYMLINK_NOFOLLOW, que syscall
			// no expone: se queda con el dueño.
			return os.Lchown(target, int(st.Uid), int(st.Gid))
		case syscall.S_IFIFO, syscall.S_IFCHR, syscall.S_IFBLK:
			if err := syscall.Mknod(target, st.Mode&^0o7777|0o600, int(st.Rdev)); err != nil {
				return err
			}
		default:
			return nil // sockets: son de un proceso vivo, no de la imagen
		}
		return applyMeta(target, &st)
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := applyMeta(dirs[i].path, &dirs[i].st); err != nil {
			return err
		}
	}
	return nil
}

// applyMeta pone dueño, modo y fechas de st en path, que no es un enlace.
// Primero el dueño: chown borra setuid y setgid, y el modo los repone.
func applyMeta(path string, st *syscall.Stat_t) error {
	if err := os.Lchown(path, int(st.Uid), int(st.Gid)); err != nil {
		return err
	}
	if err := syscall.Chmod(path, st.Mode&0o7777); err != nil {
		return err
	}
	ts := []syscall.Timespec{st.Atim, st.Mtim}
	return syscall.UtimesNano(path, ts)
}

// copyFileNoFollow copia un fichero regular. O_NOFOLLOW en el origen por si
// alguien lo cambió por un enlace entre el recorrido y la copia; O_EXCL en el
// destino porque nada debería existir ya ahí.
func copyFileNoFollow(src, dst string) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("%s: %w", src, err)
	}
	return out.Close()
}
