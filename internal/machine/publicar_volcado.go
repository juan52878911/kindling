package machine

// Publicar el volcado de un commit en macOS.
//
// kling-vz y kling-hv están confinados (vz/cmd/*/kling-*.sb) y no escriben en
// <raíz>/snapshots: ahí están los dorados de TODAS las máquinas, y el mem.file
// de un dorado no lleva hash (lo que se restaura de él es la memoria de cada
// instancia futura). Un VMM tomado por su invitado que pudiera escribir ahí
// reescribiría los dorados de los demás. Así que el commit le pide el volcado
// en el directorio de su máquina y es el daemon, que creó snapshots/<nombre>/
// y es dueño de él, quien lo clona a su sitio. Antes lo hacía un custodio, un
// proceso de ~6 MiB por máquina lanzado por el VMM antes de encerrarse; este
// hace las mismas comprobaciones y alguna más:
//
//   - el origen se abre relativo al directorio de la máquina abierto sin
//     seguir enlaces, y el fichero también sin seguirlos: el VMM puede cambiar
//     lo que hay en su directorio, y hasta el directorio por un enlace;
//   - tiene que ser un fichero regular, de este usuario y con un solo enlace
//     (un hardlink a un fichero que el VMM no puede leer y el daemon sí se
//     llevaría al dorado lo que no es suyo);
//   - se clona DESDE ese descriptor (fclonefileat: lo que el VMM escriba
//     después en el origen no llega al clon) a un fichero que no existe; sin
//     clonefile, copia dispersa a un fichero nuevo (copiarOverlayDesde).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// volcadoEnLaMaquina: el VMM vuelca en el directorio de su máquina y el
// daemon publica el volcado (macOS). En Linux Firecracker escribe en
// snapshots/<nombre>/ directamente (o en su jail, que recupera
// recuperarDelJail).
const volcadoEnLaMaquina = runtime.GOOS == "darwin"

// Nombres del volcado temporal de un commit dentro del directorio de la
// máquina. Empiezan por punto, como el resto de temporales del daemon ahí.
const (
	volcadoTmpSnap = ".commit-snap.file"
	volcadoTmpMem  = ".commit-mem.file"
)

// publicarVolcado clona mdir/tmp (lo que escribió el VMM) en dst, un fichero
// nuevo de un directorio del daemon. Ver el comentario del fichero.
func (m *Manager) publicarVolcado(ctx context.Context, mdir, tmp, dst string) error {
	dfd, err := syscall.Open(mdir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("publishing %s: the machine directory: %w", filepath.Base(dst), err)
	}
	defer syscall.Close(dfd)
	fd, err := jOpenat(dfd, tmp, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("publishing %s: %w", filepath.Base(dst), err)
	}
	in := os.NewFile(uintptr(fd), filepath.Join(mdir, tmp))
	defer in.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	switch {
	case st.Mode&syscall.S_IFMT != syscall.S_IFREG:
		return fmt.Errorf("publishing %s: the dump is not a regular file", filepath.Base(dst))
	case st.Nlink != 1:
		return fmt.Errorf("publishing %s: the dump has %d links", filepath.Base(dst), st.Nlink)
	case int(st.Uid) != os.Geteuid():
		return fmt.Errorf("publishing %s: the dump belongs to uid %d", filepath.Base(dst), st.Uid)
	}
	if _, err := m.copiarOverlayDesde(ctx, in, dst, nil); err != nil {
		return fmt.Errorf("publishing %s: %w", filepath.Base(dst), err)
	}
	return nil
}

// quitarVolcadoTemporal borra los temporales de un commit del directorio de
// la máquina, relativo a él abierto sin seguir enlaces: se borra el nombre, no
// a lo que apunte, y no en otro directorio si el VMM cambió el suyo por un
// enlace.
func quitarVolcadoTemporal(mdir string) {
	dfd, err := syscall.Open(mdir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer syscall.Close(dfd)
	for _, n := range []string{volcadoTmpSnap, volcadoTmpMem} {
		_ = jUnlinkat(dfd, n, false)
	}
}
