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
	buf := make([]byte, 1<<20)
	for off := int64(0); off < tam; {
		if err := ctx.Err(); err != nil {
			return err
		}
		ini, fin := siguientesDatos(in, off, tam)
		if ini >= tam {
			break
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

// fusionarDiff deja en dir/mem.file todo lo escrito desde el dorado: el diff
// que acaba de volcar el VMM (mem.diff) sobre el que ya hubiera de un freeze
// anterior, o él solo si es el primero. Retira mem.full, que ya no mapea
// nadie.
func fusionarDiff(ctx context.Context, dir string) error {
	diff, mem := filepath.Join(dir, memDiff), filepath.Join(dir, "mem.file")
	switch _, err := os.Stat(mem); {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Rename(diff, mem); err != nil {
			return fmt.Errorf("keeping the diff: %w", err)
		}
	case err != nil:
		return err
	default:
		if err := aplicarDiff(ctx, diff, mem); err != nil {
			return fmt.Errorf("merging the diff onto the previous one: %w", err)
		}
		if err := os.Remove(diff); err != nil {
			return err
		}
	}
	_ = os.Remove(filepath.Join(dir, memFull))
	return nil
}

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
