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
func aplicarDiff(ctx context.Context, diffPath, dstPath string) error {
	in, err := os.Open(diffPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dstPath, os.O_RDWR, 0)
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
		ini, fin := siguientesDatos(in, off, tam)
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
// thaw sin reflink (el disco lo paga solo mientras la copia corre).
func (m *Manager) prepararMemoriaDesdeDiff(ctx context.Context, mc *api.Machine, dir, base string) (string, error) {
	if !existe(base) {
		return "", fmt.Errorf("it was frozen as a diff against the golden snapshot's memory (%s), which is gone", base)
	}
	full := filepath.Join(dir, memFull)
	_ = os.Remove(full)
	m.borrarMemoriaAlmacen(mc.ID)
	// Primero el almacén (cow_memoria.go): un clon del espejo del dorado, que
	// no cuesta ni tiempo ni más disco que el diff.
	if enlace := m.memoriaEnAlmacen(ctx, mc.ID, dir, base); enlace != "" {
		return enlace, nil
	}
	if err := clonarFichero(base, full); err != nil {
		if err := m.checkDiskParaVolcado(max(mc.MemMiB, mc.MemMaxMiB), "thaw"); err != nil {
			return "", err
		}
		if err := copiarFicheroDisperso(ctx, base, full); err != nil {
			_ = os.Remove(full)
			return "", fmt.Errorf("copying the golden memory: %w", err)
		}
	}
	// Lo abre el VMM, que corre sin privilegios: como el mem.file de un
	// freeze, que escribe él mismo.
	if err := os.Chmod(full, 0o640); err == nil && m.priv != nil {
		_ = m.priv.Own(full)
	}
	if err := aplicarDiff(ctx, filepath.Join(dir, "mem.file"), full); err != nil {
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
