//go:build !linux

package machine

// dropCache no hace nada fuera de Linux: el daemon solo corre donde hay KVM,
// pero el paquete debe compilar en el resto de plataformas para poder
// desarrollarlo desde un Mac.
func dropCache(path string) {}
