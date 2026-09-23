package machine

// Acotar cuántas microVMs ENCIENDEN a la vez.
//
// Arrancar una microVM —en frío o restaurando un snapshot— es, por debajo, una
// ráfaga de ioctls a KVM: crear la VM, crear los vCPU, mapear la memoria del
// invitado. Una sola es barata. Doce a la vez son una tormenta, y en un host con
// virtualización anidada (kindling dentro de una VM de Proxmox, el laboratorio)
// esa tormenta llega a colgar el kernel del ANFITRIÓN del invitado: ni pánico ni
// OOM, solo un kernel que deja de responder mientras el hipervisor de fuera lo ve
// "vivo". Comprobado el 2026-08-12: doce arranques en frío SIMULTÁNEOS clavaron
// la VM y hubo que resetearla desde el hipervisor; con los arranques espaciados
// 200–300 ms, los mismos doce entran sin despeinarse.
//
// La puerta NO serializa el despliegue, solo el INSTANTE del encendido. Dos
// servicios distintos siguen quedando vivos a la vez y sirviendo en paralelo; lo
// único que se pacea es el tramo de KVM, que dura milisegundos. Así "desplegar en
// paralelo" pasa de colgar la máquina a, simplemente, encender por tandas.
//
// No sustituye a la reserva de memoria (memory.go): esa dice si la microVM CABE;
// esta, cuántas pueden estar naciendo a la vez aunque quepan todas. Son dos
// límites distintos —memoria y tormenta de arranque— y hacen falta los dos.
//
// El límite se ajusta con KLING_MAX_PARALLEL_BOOT. Por defecto es conservador
// porque el caso que hace daño —anidamiento, pocos vCPU— es el más común en un
// laboratorio; en hardware de verdad se puede subir sin miedo, o poner un número
// muy alto para volver al comportamiento de antes.

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// defaultMaxParallelLaunch es cuántos encendidos simultáneos se permiten en un
// host ANIDADO si nadie lo configura. Dos deja pasar el paralelismo real —los
// servicios acaban co-residentes— sin desatar la tormenta que cuelga un host
// anidado.
const defaultMaxParallelLaunch = 2

// maxParallelLaunchMetal acota la puerta en hierro desnudo. Sin tope, una
// ráfaga de cien arranques satura el disco igual; ocho ya no es el cuello.
const maxParallelLaunchMetal = 8

// maxParallelLaunch lee el tope de KLING_MAX_PARALLEL_BOOT, con mínimo 1.
//
// Sin configurar, depende de dónde corre. El 2 se eligió para hosts anidados
// (una VM con KVM dentro), donde una ráfaga de arranques cuelga el host; en
// hierro desnudo ese mismo 2 era el cuello de las ráfagas, que las pruebas de
// estrés pusieron en p95 de 13 a 15 s. Allí se usa la mitad de los núcleos,
// entre 2 y maxParallelLaunchMetal.
func maxParallelLaunch() int {
	if v := os.Getenv("KLING_MAX_PARALLEL_BOOT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	if bajoHipervisor() {
		return defaultMaxParallelLaunch
	}
	return min(max(defaultMaxParallelLaunch, runtime.NumCPU()/2), maxParallelLaunchMetal)
}

// bajoHipervisor dice si el host es a su vez una máquina virtual.
//
// Tres señales, porque ninguna vale en todas partes: el flag "hypervisor" de
// /proc/cpuinfo solo existe en x86 —en arm64 cpuinfo no lista nada parecido, y
// el laboratorio anidado en un Mac se tomaba por hierro desnudo—; el fabricante
// de la DMI, que cualquier hipervisor rellena; y /proc/device-tree/hypervisor,
// que es como se anuncia KVM en arm64 sin DMI. Sin poder leer nada (no Linux) se
// asume que sí, que es el valor prudente.
func bajoHipervisor() bool {
	cpu, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return true
	}
	if strings.Contains(string(cpu), " hypervisor") {
		return true
	}
	if _, err := os.Stat("/proc/device-tree/hypervisor"); err == nil {
		return true
	}
	var dmi strings.Builder
	for _, f := range []string{"sys_vendor", "product_name"} {
		b, _ := os.ReadFile("/sys/class/dmi/id/" + f)
		dmi.Write(b)
	}
	return esVirtualSegunDMI(dmi.String())
}

// esVirtualSegunDMI reconoce los fabricantes y productos que ponen los
// hipervisores habituales en la DMI del invitado.
func esVirtualSegunDMI(s string) bool {
	for _, marca := range []string{"QEMU", "KVM", "VMware", "VirtualBox", "innotek", "Xen",
		"Virtual Machine", "Virtualization", "Parallels", "BHYVE", "Google Compute",
		"Amazon EC2", "OpenStack", "HVM domU"} {
		if strings.Contains(s, marca) {
			return true
		}
	}
	return false
}

// enterLaunch toma un hueco en la puerta de arranque y devuelve la función para
// soltarlo. Hay que llamar a esa función SIEMPRE —encienda bien o mal— o el hueco
// se queda ocupado y estrangula a los siguientes; por eso quien la usa la pone en
// un defer.
//
// Respeta el contexto: un despliegue que se cancela mientras espera su turno no
// se queda colgado en la cola, devuelve el error del contexto y suelta su sitio.
func (m *Manager) enterLaunch(ctx context.Context) (func(), error) {
	// launchGate nil = sin límite. Un Manager construido a mano en un test no pasa
	// por NewManager, y no tiene por qué pagar la puerta: no hay nada que tomar ni
	// que soltar.
	if m.launchGate == nil {
		return func() {}, nil
	}
	select {
	case m.launchGate <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-m.launchGate }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
