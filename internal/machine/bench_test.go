package machine

import (
	"fmt"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// MEDICIÓN DE persist().
//
// persist() se llama desde los doce sitios del ciclo de vida CON m.mu tomado, así
// que su coste no lo paga solo quien la llama: mientras copia, ninguna otra
// máquina puede arrancar, congelarse ni descongelarse. La copia es POR VALOR y a
// propósito —es lo que evita la carrera con json.Marshal, ver el comentario de
// persist()— pero un coste que crece con el número de máquinas dentro del lock
// global merece tener número, no suposición.
//
// No toca KVM, red ni disco: corre en CI.

// benchManager arma un Manager con n máquinas y SIN persistLoop.
//
// No se reutiliza newTestManager (persist_test.go) por dos razones: pide un
// *testing.T, y arranca el bucle de escritura. Con el bucle vivo, las fotos que
// encola el benchmark acabarían en fsync a disco y lo medido sería la latencia
// del disco, no la copia bajo lock. persist() funciona igual sin lector: el
// aviso a m.wake es un envío no bloqueante sobre un canal con hueco.
func benchManager(n int) *Manager {
	m := &Manager{
		root:   "/nonexistent", // nunca se escribe: nadie vacía m.pending
		byID:   make(map[string]*api.Machine, n),
		socket: make(map[string]string, n),
		wake:   make(chan struct{}, 1),
		quit:   make(chan struct{}),
	}
	now := time.Now()
	for i := range n {
		id := fmt.Sprintf("m-%04d", i)
		started := now
		m.byID[id] = &api.Machine{
			ID:        id,
			Name:      id,
			Image:     "alpine",
			State:     api.StateRunning,
			VCPUs:     1,
			MemMiB:    256,
			PID:       10000 + i,
			DiskBytes: 48 << 20,
			CreatedAt: now,
			StartedAt: &started,
			IP:        fmt.Sprintf("172.16.%d.2", i%256),
			NetIndex:  i,
			CPUPct:    defaultCPUPct,
			// Con etiquetas, como las máquinas reales del gateway: la copia por
			// valor es SUPERFICIAL, así que este mapa se comparte entre la
			// máquina viva y su foto. Ponerlo aquí es lo que hace que el número
			// medido corresponda al coste real y no a uno optimista.
			Labels: map[string]string{"service": "filesystem"},
		}
		m.socket[id] = "/var/lib/kindling/machines/" + id + "/fc.sock"
	}
	return m
}

// BenchmarkPersist mide la toma de la foto con 10, 100 y 500 máquinas.
//
// El lock se coge FUERA del bucle porque en producción ya viene cogido: quien
// llama a persist() está a mitad de una operación de ciclo de vida. Medirlo con
// el RLock dentro añadiría un coste constante que no es de persist() y que, con
// 10 máquinas, pesaría más que lo que se quiere observar.
func BenchmarkPersist(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		m := benchManager(n)
		b.Run(fmt.Sprintf("maquinas=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			m.mu.RLock()
			defer m.mu.RUnlock()
			for b.Loop() {
				m.persist()
			}
		})
	}
}
