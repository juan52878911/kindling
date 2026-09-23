package machine

// Piezas comunes a los dos backends. Las funciones propias de cada plataforma
// están en plataforma_fc.go (Linux, Firecracker) y plataforma_vz.go (macOS,
// kling-vz); lo que hay aquí es puro y se prueba en los dos sistemas.

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/juan52878911/kindling/pkg/api"
)

// Nombres de los backends, los mismos que acepta la clave daemon.vmm de la
// configuración y que devuelve GET /info.
const (
	BackendFirecracker = "firecracker"
	BackendVZ          = "vz"
)

// Backend es el VMM con el que arranca este daemon.
func (m *Manager) Backend() string { return backendVMM }

// BackendCompilado es el VMM para el que se compiló este binario: el gestor
// solo sabe hablar con ese, porque lo que hace el host alrededor del VMM
// (red, cgroups, /proc) va por etiquetas de compilación.
func BackendCompilado() string { return backendVMM }

// NombreVMM es el ejecutable por defecto de cada backend.
func NombreVMM(backend string) string {
	if backend == BackendVZ {
		return "kling-vz"
	}
	return "firecracker"
}

// ResolverVMM decide el binario del VMM.
//
//   - KLING_VMM (env) manda si está: un nombre de backend ("vz",
//     "firecracker") elige el ejecutable por defecto de ese backend; cualquier
//     otra cosa es la ruta o el nombre del binario.
//   - Si no, firecracker usa fcBin (el flag -firecracker o KLING_FIRECRACKER,
//     como siempre) y kling-vz se busca junto al ejecutable de kling y luego en
//     el PATH.
//
// Si no se encuentra, devuelve el nombre a secas: el daemon arranca igual (vale
// para inspeccionar estado) y lo avisa al lado de los otros binarios que faltan.
func ResolverVMM(backend, env, fcBin string) string {
	exe, _ := os.Executable()
	return resolverVMM(backend, env, fcBin, filepath.Dir(exe), exec.LookPath, existeFichero)
}

func resolverVMM(backend, env, fcBin, exeDir string, look func(string) (string, error), existe func(string) bool) string {
	switch env {
	case "":
	case BackendVZ, BackendFirecracker:
		if env != backend {
			// Pedir por nombre el OTRO backend no cambia el protocolo, que ya
			// eligió la configuración: se ignora y lo dirá la validación.
			break
		}
		if backend == BackendFirecracker {
			return fcBin
		}
		return buscarJunto(NombreVMM(backend), exeDir, look, existe)
	default:
		return env
	}
	if backend == BackendFirecracker {
		return fcBin
	}
	return buscarJunto(NombreVMM(backend), exeDir, look, existe)
}

// buscarJunto busca el binario junto a kling (así un tar con los dos funciona
// sin tocar el PATH), luego en el PATH, y si no lo encuentra devuelve el nombre.
func buscarJunto(nombre, exeDir string, look func(string) (string, error), existe func(string) bool) string {
	if exeDir != "" {
		if p := filepath.Join(exeDir, nombre); existe(p) {
			return p
		}
	}
	if p, err := look(nombre); err == nil {
		return p
	}
	return nombre
}

func existeFichero(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// buscarE2fs localiza una herramienta de e2fsprogs: el PATH, los sbin de
// siempre (en Debian no están en el PATH de un usuario) y los directorios
// propios de la plataforma (Homebrew en macOS). "" si no está.
func buscarE2fs(nombre string) string {
	if bin, err := exec.LookPath(nombre); err == nil {
		return bin
	}
	dirs := append([]string{"/sbin", "/usr/sbin"}, dirsE2fsExtra...)
	for _, d := range dirs {
		p := filepath.Join(d, nombre)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// errFaltaE2fs es el error cuando falta una herramienta de e2fsprogs.
func errFaltaE2fs(nombre string) error {
	return fmt.Errorf("%s not found: install e2fsprogs (brew install e2fsprogs)", nombre)
}

// defaultMinMemLevel es el nivel de memoria de macOS (kern.memorystatus_level,
// porcentaje de memoria que el sistema da por disponible) por debajo del cual
// no se admiten máquinas. Con menos del 15 % el sistema ya comprime y avisa de
// presión; una VM más, que restaura su memoria entera, lo empuja a matar
// procesos auxiliares de Virtualization.framework —las otras VMs—.
const defaultMinMemLevel = 15

// minMemLevel lee KLING_MIN_MEM_LEVEL; "0" apaga la comprobación.
func minMemLevel() int {
	if v := os.Getenv("KLING_MIN_MEM_LEVEL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 100 {
			return n
		}
	}
	return defaultMinMemLevel
}

// evaluarNivelMemoria es la admisión de macOS sobre un nivel ya leído: 507,
// como checkPressure, para que el planificador conteste congelando ociosas.
func evaluarNivelMemoria(nivel, minimo int) error {
	if minimo <= 0 || nivel < 0 || nivel >= minimo {
		return nil
	}
	return &api.StatusError{Code: api.StatusInsufficientMemory, Message: fmt.Sprintf(
		"macOS reports only %d%% of memory available (the minimum to start a machine is %d%%).\n"+
			"Every vz microVM restores its whole memory, so another one would push the system "+
			"into killing processes.\n"+
			"Freeze or remove idle instances (`kling ps`), or lower the minimum with KLING_MIN_MEM_LEVEL",
		nivel, minimo)}
}

// leerUint64LE interpreta el valor crudo de un sysctl entero de 64 bits. El
// syscall.Sysctl de la biblioteca estándar quita un byte final a cero, así que
// se rellena hasta ocho antes de leerlo.
func leerUint64LE(s string) uint64 {
	b := []byte(s)
	if len(b) > 8 {
		b = b[:8]
	}
	for len(b) < 8 {
		b = append(b, 0)
	}
	return binary.LittleEndian.Uint64(b)
}

// E2fsDisponible dice si la herramienta de e2fsprogs está donde el daemon la
// va a buscar (PATH, sbin o Homebrew). mkfs.ext4 vale también como mke2fs.
func E2fsDisponible(nombre string) bool {
	if buscarE2fs(nombre) != "" {
		return true
	}
	return nombre == "mkfs.ext4" && buscarE2fs("mke2fs") != ""
}
