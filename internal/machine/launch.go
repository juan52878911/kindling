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
	"strconv"
	"sync"
)

// defaultMaxParallelLaunch es cuántos encendidos simultáneos se permiten si nadie
// lo configura. Dos deja pasar el paralelismo real —los servicios acaban
// co-residentes— sin desatar la tormenta que cuelga un host anidado.
const defaultMaxParallelLaunch = 2

// maxParallelLaunch lee el tope de KLING_MAX_PARALLEL_BOOT, con mínimo 1.
func maxParallelLaunch() int {
	if v := os.Getenv("KLING_MAX_PARALLEL_BOOT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return defaultMaxParallelLaunch
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
