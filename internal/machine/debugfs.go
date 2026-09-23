package machine

// debugfsBin localiza debugfs, que en Debian vive en /sbin y no siempre está en
// el PATH de un servicio de systemd.
func debugfsBin() string {
	return buscarE2fs("debugfs")
}
