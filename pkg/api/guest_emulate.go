package api

// Imágenes de otra arquitectura con qemu-user (docs/imagenes-amd64.md).
//
// Una imagen importada con emulación (kling image import -arch amd64 en un
// anfitrión arm64) lleva el agente de la arquitectura del anfitrión, un
// qemu-user estático en GuestEmulators[arch] y, en GuestEmulateFile, la
// arquitectura emulada. El init del agente registra el intérprete en
// binfmt_misc antes de lanzar nada de la imagen (pkg/guest/binfmt.go).

// GuestEmulateFile dice qué arquitectura emula la imagen ("amd64\n"). Sin él,
// la imagen es nativa.
const GuestEmulateFile = "/etc/kindling/emulate"

// GuestEmulators es dónde va el qemu-user de cada arquitectura emulable. El
// sufijo -binfmt-P es el convenio de Debian para que qemu sepa que
// binfmt_misc le pasa el argv[0] original (bandera P) aunque el kernel no lo
// dijera en AT_FLAGS.
var GuestEmulators = map[string]string{
	"amd64": "/usr/local/lib/kindling/qemu-x86_64-binfmt-P",
}
