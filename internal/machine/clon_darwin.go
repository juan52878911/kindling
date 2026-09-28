package machine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
)

// metodoClon es lo que da un clon que sale bien en esta plataforma.
const metodoClon = api.CloneClonefile

// clonarFichero clona src en dst (que NO debe existir) con clonefile(2) de
// APFS, a través de `cp -c`, que es lo que ya usa copiarDisco.
//
// `cp -c` NO falla si no puede clonar: cae en silencio a una copia completa.
// Por eso se comprueba antes lo que clonefile exige —APFS, y el mismo volumen
// para origen y destino— y si no se cumple se contesta errSinClon, igual que
// Linux con EOPNOTSUPP o EXDEV. Un "clon" que en realidad copió 50 GiB es
// justo el engaño que esta función existe para evitar.
func clonarFichero(src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s: %w", dst, os.ErrExist)
	}
	var a, b syscall.Stat_t
	if err := syscall.Stat(src, &a); err != nil {
		return err
	}
	if err := syscall.Stat(filepath.Dir(dst), &b); err != nil {
		return err
	}
	if a.Dev != b.Dev {
		return fmt.Errorf("%w: source and destination are on different volumes", errSinClon)
	}
	if fs := tipoFS(src); fs != "apfs" {
		return fmt.Errorf("%w: %s does not support clonefile", errSinClon, fs)
	}
	if out, err := exec.Command("cp", "-c", src, dst).CombinedOutput(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("cp -c: %v: %s", err, out)
	}
	return nil
}

// tipoFS devuelve el nombre del sistema de ficheros de path (apfs, hfs…).
func tipoFS(path string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return ""
	}
	b := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
