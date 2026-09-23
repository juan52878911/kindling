package machine

// ¿Está cifrado en reposo lo que guarda el daemon?
//
// kindling no cifra por su cuenta y es a propósito: Firecracker mapea mem.file
// directamente, y cifrar por encima de eso obligaría a descifrar en cada thaw
// —adiós a los milisegundos— o a meter una capa de bloques propia. La capa
// correcta es la de debajo: LUKS (dm-crypt) o fscrypt sobre el disco donde vive
// $KLING_ROOT, que es transparente para Firecracker y cuesta lo que cueste el
// AES del procesador. Ver docs/cifrado.md.
//
// Lo que sí hace el daemon es DECIRLO: si los snapshots —que llevan la memoria
// de cada microVM, secretos de sesión incluidos antes de inyectarlos— están en
// un disco en claro, `kling info` lo muestra en vez de dejarlo a la suposición.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// CifradoEnReposo dice si root vive sobre un dispositivo dm-crypt, siguiendo la
// pila de dispositivos hacia abajo (LVM sobre LUKS, por ejemplo). conocido es
// false cuando no se puede saber: otro sistema operativo, un sistema de ficheros
// sin dispositivo de bloques (tmpfs, overlay), o /sys sin montar.
func CifradoEnReposo(root string) (cifrado, conocido bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(root, &st); err != nil {
		return false, false
	}
	mayor, menor := uint64(st.Dev)>>8&0xfff, uint64(st.Dev)&0xff|(uint64(st.Dev)>>12)&0xfff00
	dev := filepath.Join("/sys/dev/block", strconv.FormatUint(mayor, 10)+":"+strconv.FormatUint(menor, 10))
	real, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return false, false
	}
	return pilaCifrada(real, 0), true
}

// pilaCifrada busca un dm-crypt en el dispositivo o en los que tiene debajo.
func pilaCifrada(dev string, prof int) bool {
	if prof > 8 {
		return false
	}
	if uuid, err := os.ReadFile(filepath.Join(dev, "dm", "uuid")); err == nil &&
		strings.HasPrefix(strings.TrimSpace(string(uuid)), "CRYPT-") {
		return true
	}
	// Una partición no tiene dm/ ni slaves/: lo que cuenta es su disco padre.
	if _, err := os.Stat(filepath.Join(dev, "partition")); err == nil {
		return pilaCifrada(filepath.Dir(dev), prof+1)
	}
	debajo, _ := os.ReadDir(filepath.Join(dev, "slaves"))
	for _, d := range debajo {
		if real, err := filepath.EvalSymlinks(filepath.Join(dev, "slaves", d.Name())); err == nil &&
			pilaCifrada(real, prof+1) {
			return true
		}
	}
	return false
}
