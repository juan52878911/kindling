package machine

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// volcadoFalso escribe lo que escribiría el VMM de la máquina id al recibir
// PUT /snapshot/create para el snapshot de snapDir: en Linux, Firecracker lo
// deja en snapDir; en macOS, kling-vz lo deja en el directorio de la máquina y
// el daemon lo publica (publicar_volcado.go). Para los ganchos de fcFalso.
func volcadoFalso(m *Manager, id, snapDir string) {
	snap, mem := filepath.Join(snapDir, "snap.file"), filepath.Join(snapDir, "mem.file")
	if volcadoEnLaMaquina {
		snap, mem = filepath.Join(m.dir(id), volcadoTmpSnap), filepath.Join(m.dir(id), volcadoTmpMem)
	}
	_ = os.WriteFile(snap, []byte("estado"), 0o644)
	_ = os.WriteFile(mem, make([]byte, 1<<20), 0o644)
}

// puedePerforar salta el test si perforarHuecos no puede ir en esta máquina.
// Solo la de Linux necesita algo de fuera (fallocate --dig-holes); la de macOS
// no perfora nada (el mem.file de kling-vz no es un volcado crudo de la RAM),
// así que ahí el test corre siempre. Antes todos miraban fallocate en el PATH,
// y en macOS, donde no está, se saltaban los tests de commit, fork y meta del
// backend que no lo usa.
func puedePerforar(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := exec.LookPath("fallocate"); err != nil {
		t.Skip("sin fallocate no se puede perforar el volcado")
	}
}
