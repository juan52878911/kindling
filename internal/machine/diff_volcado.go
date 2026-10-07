package machine

// Congelado diferencial de las copias de un dorado.
//
// Una copia (run -from) mapea el mem.file del dorado y, mientras corre, solo
// se separa de él en las páginas que escribe: medido con Hindsight, ~100 MiB
// propios de una RAM de 3 GiB. Congelarla volcaba la RAM ENTERA —1,9 GiB y
// 6-8 s por copia—, que es lo que el disco pagaba por cada copia dormida.
//
// Con seguimiento de páginas sucias desde la carga (LoadSnapshotTracking),
// Firecracker sabe volcar solo lo escrito (SnapshotDiff): un fichero del
// tamaño de la RAM pero disperso, con datos únicamente en las páginas sucias.
// Ese es el mem.file de la copia congelada; el sello apunta a su base, el
// mem.file del dorado (sello.DiffBase, Machine.DiffBase mientras corre).
//
// Para descongelar, el VMM necesita un solo fichero: se clona la base
// (FICLONE, gratis en btrfs y XFS; en ext4, una copia dispersa) a mem.full,
// se le escriben encima los tramos con datos del diff, y se carga de ahí, otra
// vez con seguimiento. El siguiente freeze vuelca lo escrito desde ENTONCES
// (mem.diff) y lo funde sobre el diff que ya había: el mem.file sigue siendo
// "todo lo que cambió desde el dorado" en un solo nivel, y mem.full se borra.
//
// Dos cosas que no son obvias:
//
//   - En un diferencial un HUECO significa "igual que la base" y una página
//     de CEROS significa "el invitado la puso a cero". Por eso un diff nunca
//     se perfora (perforarHuecos convertiría lo segundo en lo primero y el
//     invitado despertaría con la página vieja) y al fundir se copian los
//     ceros tal cual.
//   - Cualquier snapshot completo (commit, fork) deja a cero el mapa de
//     sucias del VMM: desde ahí un diff ya no sería "desde el dorado". Quien
//     lo hace olvida DiffBase y el siguiente freeze vuelca entero.
//
// Solo en Firecracker (en vz la copia no comparte memoria con el dorado).
// KLING_DIFF_FREEZE=0 lo apaga: las copias se cargan sin seguimiento y se
// congelan como siempre.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// memDiff es lo que acaba de volcar SnapshotDiff: solo lo escrito desde
	// la última carga o el último volcado.
	memDiff = "mem.diff"
	// memFull es base + diff, lo que mapea el VMM mientras la copia corre tras
	// un thaw. Vive solo entre un thaw y el siguiente freeze.
	memFull = "mem.full"
)

// congelarEnDiff dice si las copias se cargan con seguimiento y se congelan
// en diferencial.
func congelarEnDiff() bool {
	return restaurarComparteMemoria && os.Getenv("KLING_DIFF_FREEZE") != "0"
}

// existe dice si hay algo en path.
func existe(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// olvidarDiffBase deja a la máquina sin base: su siguiente freeze vuelca la
// RAM entera. Para después de un snapshot completo o de un diff que falló.
func (m *Manager) olvidarDiffBase(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := m.byID[id]; cur != nil && cur.DiffBase != "" {
		cur.DiffBase = ""
		m.persist()
	}
}

// aplicarDiff escribe sobre dst, en su sitio, los tramos con datos de diff:
// ceros incluidos, que en un diferencial son páginas que el invitado puso a
// cero y no "nada que decir". Los huecos de diff no se tocan.
//
// Si los dos ficheros están en el mismo Btrfs o XFS (el almacén), cada tramo
// se CLONA (FICLONERANGE) en vez de copiarse: los bloques del diff pasan a
// estar referenciados también desde dst, sin leer ni escribir datos. Es lo
// que hace que despertar de un diferencial de cientos de MiB cueste
// milisegundos. Si el sistema de ficheros no clona (ext4, otro sistema de
// ficheros, tramos no alineados) se copia, tramo a tramo.
//
// Los dos se abren sin seguir enlaces y tienen que ser ficheros regulares con
// un solo nombre: sin jailer, machines/<id> es del VMM, y un enlace plantado
// ahí llevaría al daemon (root) a escribir datos del invitado en un fichero
// del host, o a meter uno del host en la RAM del invitado.
func aplicarDiff(ctx context.Context, diffPath, dstPath string) error {
	in, err := abrirPropio(diffPath, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := abrirPropio(dstPath, os.O_RDWR)
	if err != nil {
		return err
	}
	defer out.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	tam := fi.Size()
	if ofi, err := out.Stat(); err != nil {
		return err
	} else if ofi.Size() < tam {
		if err := out.Truncate(tam); err != nil {
			return err
		}
	}
	clonar := true
	buf := make([]byte, 1<<20)
	for off := int64(0); off < tam; {
		if err := ctx.Err(); err != nil {
			return err
		}
		ini, fin, err := siguientesDatosEstricto(in, off, tam)
		if err != nil {
			return fmt.Errorf("finding the diff's data: %w", err)
		}
		if ini >= tam {
			break
		}
		if clonar && fin-ini >= bloqueDisperso && ini%bloqueDisperso == 0 {
			// El tramo entero menos la cola no alineada (en un volcado de
			// páginas no la hay).
			l := (fin - ini) / bloqueDisperso * bloqueDisperso
			if err := clonarTramo(in, out, ini, l); err == nil {
				if fin-ini == l {
					off = fin
					continue
				}
				ini += l
			} else {
				clonar = false // no en este sistema de ficheros: el resto se copia
			}
		}
		for p := ini; p < fin; {
			n := min(int64(len(buf)), fin-p)
			leidos, err := in.ReadAt(buf[:n], p)
			if leidos > 0 {
				if _, werr := out.WriteAt(buf[:leidos], p); werr != nil {
					return werr
				}
				p += int64(leidos)
			}
			if err == io.EOF {
				fin = p
				break
			}
			if err != nil {
				return err
			}
		}
		off = fin
	}
	return out.Sync()
}

// fusionarDiff deja en acum todo lo escrito desde el dorado: el diff que
// acaba de volcar el VMM (diff) sobre el que ya hubiera, o él solo si es el
// primero (un rename: los dos están en el mismo sitio, el directorio de la
// máquina o el almacén). Si acum no es machines/<id>/mem.file (vive en el
// almacén), machines/<id>/mem.file pasa a ser un enlace a él, como el overlay
// (cow_memoria.go). Retira mem.full, que ya no mapea nadie.
func fusionarDiff(ctx context.Context, dir, diff, acum string) error {
	mem := filepath.Join(dir, "mem.file")
	switch fi, err := os.Lstat(acum); {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Rename(diff, acum); err != nil {
			return fmt.Errorf("keeping the diff: %w", err)
		}
	case err != nil:
		return err
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", acum)
	default:
		if err := aplicarDiff(ctx, diff, acum); err != nil {
			return fmt.Errorf("merging the diff onto the previous one: %w", err)
		}
		if err := os.Remove(diff); err != nil {
			return err
		}
	}
	if acum != mem {
		// Ya fundido, el VMM no vuelve a escribirlo: solo lectura por grupo,
		// como mem.full. Lo que escriba, lo escribe en un mem.diff nuevo.
		if f, err := abrirPropio(acum, os.O_RDONLY); err == nil {
			_ = f.Chmod(0o640)
			f.Close()
		}
		_ = os.Remove(mem)
		if err := os.Symlink(acum, mem); err != nil {
			return fmt.Errorf("linking the diff from the store: %w", err)
		}
	}
	_ = os.Remove(filepath.Join(dir, memFull))
	return nil
}

// clonarTramo hace que out comparta con in los bloques de [off, off+l):
// FICLONERANGE, que exige los dos ficheros en el mismo sistema de ficheros
// con reflink y tramos alineados a bloque. Lo que no se pueda clonar da
// error y quien llama copia.
func clonarTramo(in, out *os.File, off, l int64) error {
	arg := struct {
		srcFd     int64
		srcOffset uint64
		srcLength uint64
		dstOffset uint64
	}{int64(in.Fd()), uint64(off), uint64(l), uint64(off)}
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ioctlFICLONERANGE, uintptr(unsafe.Pointer(&arg)))
	if e != 0 {
		return e
	}
	return nil
}

// ioctlFICLONERANGE es FICLONERANGE (_IOW(0x94, 13, struct file_clone_range
// de 32 bytes)), igual en amd64 y arm64.
const ioctlFICLONERANGE = 0x4020940d

// prepararMemoriaDesdeDiff construye dir/mem.full (base + diff) para cargar
// una copia congelada en diferencial, y devuelve su ruta. Clona la base si el
// sistema de ficheros sabe; si no, la copia dispersa, que es lo que cuesta un
// thaw sin reflink (el disco lo paga solo mientras la copia corre). t, si no
// es nil, suma lo que costaron el almacén y su espejo (memoriaEnAlmacen).
func (m *Manager) prepararMemoriaDesdeDiff(ctx context.Context, mc *api.Machine, dir, base string, t *tiemposMemoria) (string, error) {
	if !existe(base) {
		return "", fmt.Errorf("it was frozen as a diff against the golden snapshot's memory (%s), which is gone", base)
	}
	diff, err := m.fuenteDiff(mc.ID, dir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(dir, memFull)
	_ = os.Remove(full)
	m.borrarMemoriaAlmacen(mc.ID)
	// Primero el almacén (cow_memoria.go): un clon del espejo del dorado, que
	// no cuesta ni tiempo ni más disco que el diff.
	if enlace := m.memoriaEnAlmacen(ctx, mc.ID, dir, base, diff, t); enlace != "" {
		return enlace, nil
	}
	if err := clonarFichero(base, full); err != nil {
		soltarDisco, err := m.reservarDiscoParaVolcado(max(mc.MemMiB, mc.MemMaxMiB), "thaw")
		if err != nil {
			return "", err
		}
		// Hasta que la copia termina: desde ahí lo que ocupa ya se ve libre
		// de menos y no hace falta contarlo.
		err = copiarFicheroDisperso(ctx, base, full)
		soltarDisco()
		if err != nil {
			_ = os.Remove(full)
			return "", fmt.Errorf("copying the golden memory: %w", err)
		}
	}
	// Lo abre el VMM, que corre sin privilegios: como el mem.file de un
	// freeze, que escribe él mismo. Por descriptor: el directorio es suyo.
	f, err := abrirPropio(full, os.O_RDONLY)
	if err != nil {
		_ = os.Remove(full)
		return "", err
	}
	if err := f.Chmod(0o640); err == nil && m.priv != nil {
		_ = m.priv.OwnFile(f)
	}
	f.Close()
	if err := aplicarDiff(ctx, diff, full); err != nil {
		_ = os.Remove(full)
		return "", fmt.Errorf("applying the diff onto the golden memory: %w", err)
	}
	return full, nil
}

// copiarFicheroDisperso es copiarDisperso entre rutas: dst nace (O_EXCL).
func copiarFicheroDisperso(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	if err := copiarDisperso(ctx, in, out); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// fuenteDiff es la ruta del diff acumulado de la copia id: machines/<id>/mem.file
// si es un fichero, o el acumulado del almacén si es un enlace a él. Un
// enlace a cualquier otro sitio no se sigue: ese directorio es del VMM.
func (m *Manager) fuenteDiff(id, dir string) (string, error) {
	mem := filepath.Join(dir, "mem.file")
	fi, err := os.Lstat(mem)
	if err != nil {
		return "", err
	}
	if fi.Mode().IsRegular() {
		return mem, nil
	}
	if fi.Mode()&os.ModeSymlink != 0 && m.alm != nil && nombreSeguro(id) == nil {
		if dst, err := os.Readlink(mem); err == nil && dst == m.alm.acumuladoDiff(id) {
			return dst, nil
		}
	}
	return "", fmt.Errorf("%s is neither the frozen diff nor a link to it in the store", mem)
}

// abrirPropio abre ruta sin seguir enlaces y solo si es un fichero regular
// con un nombre (un enlace duro plantado en un directorio del VMM tendría dos).
func abrirPropio(ruta string, flag int) (*os.File, error) {
	fd, err := syscall.Open(ruta, flag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: ruta, Err: err}
	}
	f := os.NewFile(uintptr(fd), ruta)
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || (ok && uint64(st.Nlink) != 1) {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file with a single name", ruta)
	}
	return f, nil
}

// baseDiffValida dice si base es el mem.file del dorado del que viene la
// copia: el sello vive en un directorio del VMM y no se le cree otra ruta.
func (m *Manager) baseDiffValida(mc *api.Machine, base string) error {
	if mc.From == "" || base != filepath.Join(m.snapDir(mc.From), "mem.file") {
		return fmt.Errorf("its seal points at %q, which is not the memory of its golden snapshot", base)
	}
	return nil
}
