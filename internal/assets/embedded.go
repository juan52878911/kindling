//go:build linux && embed_assets

// La etiqueta lleva `linux` además de `embed_assets` porque los artefactos solo
// sirven en un host con KVM: un binario de macOS con el kernel dentro no es una
// opción rara, es un error de compilación esperando a ocupar 20 MB.
//
// Los blobs se preparan con `make assets` y NO están versionados (van en
// .gitignore). Si faltan, este fichero no compila y el mensaje es
// "pattern blobs/vmlinux: no matching files found", que es exactamente lo que
// debe pasar: pedir el binario completo sin tener los artefactos es un error,
// no algo que se resuelva en silencio produciendo un daemon a medias.

package assets

import (
	"embed"
	"io/fs"
)

//go:embed blobs/vmlinux blobs/min.ext4
var blobs embed.FS

// source expone los blobs con sus nombres pelados, sin el prefijo "blobs/",
// para que Artifacts sea la única lista de nombres del paquete.
func source() (fs.FS, bool) {
	sub, err := fs.Sub(blobs, "blobs")
	if err != nil {
		return nil, false
	}
	return sub, true
}
