package machine

import (
	"fmt"
	"os"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
)

// metodoClon es lo que da un clon que sale bien en esta plataforma.
const metodoClon = api.CloneReflink

// ficlone es FICLONE = _IOW(0x94, 9, int). Esa codificación es la genérica de
// Linux, la de amd64 y arm64, que son las dos arquitecturas del daemon.
const ficlone = 0x40049409

// clonarFichero clona src en dst (que NO debe existir) con FICLONE: el kernel
// hace que dst comparta los extents de src, sin copiar un byte. Tarda lo mismo
// con 1 MiB que con 100 GiB, y dst no ocupa nada hasta que uno de los dos
// escribe. Los huecos de un fichero disperso siguen siendo huecos.
//
// Se hace con el ioctl y no con `cp --reflink=always` por dos razones. La
// primera, que cp no sabe decir POR QUÉ no pudo: aquí el errno distingue "este
// sistema de ficheros no clona" (EOPNOTSUPP) de "están en discos distintos"
// (EXDEV), y es justo lo que el usuario necesita oír. La segunda, que cp
// rechaza --reflink junto a --sparse=always, que es como el resto del daemon
// copia discos: comprobado con coreutils 9.4.
//
// Cualquier sistema de ficheros que implemente FICLONE vale sin cambiar nada:
// XFS formateado con reflink=1 (el defecto desde xfsprogs 5.1), btrfs,
// bcachefs, OCFS2, y ZFS ≥ 2.2 con block cloning activo. Los que no, dan
// errSinClon y el llamante decide si cae a copia.
func clonarFichero(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// O_EXCL: dos clones al mismo destino no se pisan, y nunca se trunca un
	// fichero que ya estaba ahí.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ficlone, in.Fd())
	cerr := out.Close()
	if errno != 0 {
		_ = os.Remove(dst)
		if sinClon(errno) {
			return fmt.Errorf("%w: %w", errSinClon, errno)
		}
		return errno
	}
	if cerr != nil {
		_ = os.Remove(dst)
		return cerr
	}
	return nil
}

// sinClon dice si el errno de FICLONE significa "aquí no se puede clonar",
// frente a un fallo de verdad (disco lleno, E/S).
func sinClon(e syscall.Errno) bool {
	switch e {
	case syscall.EOPNOTSUPP, // el sistema de ficheros no clona (ext4, XFS sin reflink)
		syscall.EXDEV,  // origen y destino en sistemas de ficheros distintos
		syscall.EINVAL, // ZFS con block cloning desactivado, overlayfs
		syscall.ENOTTY, // el sistema de ficheros no conoce el ioctl
		syscall.EPERM,  // nfs, fuse y compañía, según versión
		syscall.EBADF:
		return true
	}
	return false
}

// magias de statfs(2) de los sistemas de ficheros que interesa nombrar. Solo
// sirven para el mensaje: la decisión la toma la sonda, nunca esta tabla.
var magiasFS = map[int64]string{
	0xEF53:     "ext4",
	0x58465342: "xfs",
	0x9123683E: "btrfs",
	0x2FC12FC1: "zfs",
	0xCA451A4E: "bcachefs",
	0x7461636F: "ocfs2",
	0xF2F52010: "f2fs",
	0x01021994: "tmpfs",
	0x794C7630: "overlayfs",
	0x6969:     "nfs",
	0x65735546: "fuse",
}

// tipoFS devuelve el nombre del sistema de ficheros de path, o su número
// mágico si no está en la tabla.
func tipoFS(path string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return ""
	}
	if n, ok := magiasFS[int64(st.Type)]; ok {
		return n
	}
	return fmt.Sprintf("0x%x", uint64(st.Type))
}
