package guest

import (
	"log"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// binfmt_misc para las imágenes emuladas (api.GuestEmulateFile,
// docs/imagenes-amd64.md): el agente es nativo y cada ELF de la imagen va a
// qemu-user.

const (
	binfmtDir      = "/proc/sys/fs/binfmt_misc"
	binfmtRegister = binfmtDir + "/register"
)

// binfmtRule es una entrada de binfmt_misc: magia y máscara de la cabecera
// ELF, como las de qemu (usr/share/qemu/binfmt.d/qemu-x86_64.conf del mismo
// .deb que el binario).
type binfmtRule struct {
	name, magic, mask string
}

var binfmtRules = map[string]binfmtRule{
	"amd64": {
		name:  "qemu-x86_64",
		magic: `\x7f\x45\x4c\x46\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00`,
		mask:  `\xff\xff\xff\xff\xff\xfe\xfe\xfc\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff`,
	},
}

// binfmtFlags: P conserva el argv[0] original (lo que ve ps y lo que mira un
// binario multillamada como busybox); F abre el intérprete al registrarlo,
// así que vale dentro de un chroot o de otro espacio de montajes sin que la
// ruta exista allí; C toma las credenciales (setuid) del binario emulado, no
// de qemu, y con ella O (qemu recibe el fichero abierto). Las mismas que usa
// Docker Desktop (tonistiigi/binfmt).
const binfmtFlags = "POCF"

// binfmtLine es lo que se escribe en register para emular arch.
func binfmtLine(arch string) (name, line string, ok bool) {
	r, ok := binfmtRules[arch]
	interp, ok2 := api.GuestEmulators[arch]
	if !ok || !ok2 {
		return "", "", false
	}
	return r.name, ":" + r.name + ":M::" + r.magic + ":" + r.mask + ":" + interp + ":" + binfmtFlags, true
}

// initBinfmt registra el intérprete si la imagen va emulada. Va con la raíz
// definitiva ya montada y /proc dentro, antes de lanzar nada de la imagen.
// Un fallo no para el arranque: el agente es nativo y contesta igual, y el
// servicio dirá "exec format error", que con este aviso en la consola se
// entiende.
func initBinfmt(s initSys) {
	b, err := s.ReadFile(api.GuestEmulateFile)
	if err != nil {
		return
	}
	arch := strings.TrimSpace(string(b))
	name, line, ok := binfmtLine(arch)
	if !ok {
		log.Printf("warning: %s asks for %q, which this agent can't emulate", api.GuestEmulateFile, arch)
		return
	}
	if err := s.Stat(api.GuestEmulators[arch]); err != nil {
		log.Printf("warning: emulated %s image without its interpreter: %v", arch, err)
		return
	}
	if s.Stat(binfmtRegister) != nil {
		if err := s.Mount("binfmt_misc", binfmtDir, "binfmt_misc", msNosuid|msNodev|msNoexec, ""); err != nil {
			log.Printf("warning: mounting binfmt_misc (kernel without CONFIG_BINFMT_MISC?): %v", err)
			return
		}
	}
	if s.Stat(binfmtDir+"/"+name) == nil {
		return // ya estaba (otra vuelta del init no la hay, pero no cuesta mirar)
	}
	if err := s.WriteFile(binfmtRegister, []byte(line)); err != nil {
		log.Printf("warning: registering %s in binfmt_misc: %v", name, err)
		return
	}
	log.Printf("%s binaries run with %s", arch, api.GuestEmulators[arch])
}
