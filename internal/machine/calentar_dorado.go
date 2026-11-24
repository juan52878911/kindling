package machine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/juan52878911/kindling/pkg/api"
)

// Precalentar y anclar el mem.file de un dorado (C9-10).
//
// Con la caché fría (tras reiniciar el host o el daemon, o si la presión de
// memoria desalojó el mem.file), la primera petición a una réplica de un
// modelo esperaba a que los fallos de página trajeran el GiB de pesos del
// disco: 7,2-7,9 s medidos en EC2 (gp3). Nada lo traía antes. Aquí:
//
//   - CalentarDorado (POST /snapshots/{name}/warm) lo lee con varios lectores,
//     en segundo plano o esperando, sin el tope de 512 MiB de la precarga del
//     thaw; lo llama el gateway de IA al arrancar y tras cada deploy;
//   - con Pin, el daemon lo mapea compartido y de solo lectura y le hace
//     mlock: son las mismas páginas de la caché que mapean las réplicas
//     (MAP_PRIVATE sin escribir), y así no las desaloja nadie. Es RAM que no
//     se recupera: opt-in, solo administradores (lo mira el daemon), pasa por
//     la admisión de memoria y solo en Linux;
//   - runFromUna, si el dorado está frío, lanza la misma precarga en paralelo
//     con el VMM (precargarDoradoSiFrio).

// ErrAncladoNoSoportado: este backend no sabe anclar un dorado en RAM (macOS).
var ErrAncladoNoSoportado = errors.New("pinning a golden snapshot in RAM is not supported on this platform")

// lectoresCalentar es cuántos lectores traen un dorado a la caché: entre 4 y
// 8. Uno solo no llena la cola de un disco en red (gp3) ni de un NVMe.
func lectoresCalentar() int { return min(8, max(4, runtime.GOMAXPROCS(0))) }

// trozoCalentar es lo que lee cada lector de una vez.
const trozoCalentar = 4 << 20

// calentandoDorados son las precargas de dorado en segundo plano en curso,
// por ruta: pedir otra del mismo fichero mientras corre una no lanza otra.
var calentandoDorados sync.Map

// anclaje es un mem.file anclado en RAM.
type anclaje interface {
	bytes() int64
	soltar()
}

// anclarFichero ancla path en RAM; nil donde no se puede (anclado_other.go).
// Variable para los tests.
var anclarFichero = anclarFicheroPlataforma

// CalentarDorado trae el mem.file del dorado name a la caché de página y,
// con req.Pin, lo ancla o lo suelta. Es idempotente: con el dorado ya en
// caché no lee nada. Quien llama decide si puede anclar (solo
// administradores).
func (m *Manager) CalentarDorado(ctx context.Context, name string, req api.WarmSnapshotRequest) (*api.WarmSnapshotResult, error) {
	if _, err := m.Snapshot(name); err != nil {
		return nil, err
	}
	mem := filepath.Join(m.snapDir(name), "mem.file")
	if req.Pin != nil {
		if *req.Pin {
			if err := m.anclarDorado(name, mem); err != nil {
				return nil, err
			}
		} else {
			m.soltarAnclado(name)
		}
	}
	res := &api.WarmSnapshotResult{Bytes: allocatedBytes(mem), Pinned: m.estaAnclado(name)}
	res.CachedBytes = min(bytesEnCache(mem), res.Bytes)
	if res.CachedBytes >= res.Bytes {
		return res, nil
	}
	if req.Wait {
		precargarParalelo(ctx, mem, lectoresCalentar())
		res.CachedBytes = min(bytesEnCache(mem), res.Bytes)
		return res, nil
	}
	res.Started = m.precargarDoradoEnFondo(mem)
	return res, nil
}

// precargarDoradoEnFondo lanza precargarParalelo de mem en segundo plano si
// no corre ya una del mismo fichero, y dice si quedó una en marcha. Para al
// cerrar el Manager.
func (m *Manager) precargarDoradoEnFondo(mem string) bool {
	if isClosed(m.quit) {
		return false
	}
	if _, ya := calentandoDorados.LoadOrStore(mem, true); ya {
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.sueltas.Add(1)
	go func() {
		defer m.sueltas.Done()
		defer calentandoDorados.Delete(mem)
		defer cancel()
		go func() {
			select {
			case <-m.quit:
			case <-ctx.Done():
			}
			cancel()
		}()
		precargarParalelo(ctx, mem, lectoresCalentar())
	}()
	return true
}

// precargarDoradoSiFrio es lo de runFromUna: si menos de la mitad de lo
// asignado del mem.file está en caché, lanza la precarga en segundo plano
// para que corra a la vez que el VMM y la primera petición, en vez de a golpe
// de fallo de página. Dice si la lanzó (o ya corría una).
func (m *Manager) precargarDoradoSiFrio(mem string) bool {
	asignado := allocatedBytes(mem)
	if asignado == 0 || bytesEnCache(mem) >= asignado/2 {
		return false
	}
	return m.precargarDoradoEnFondo(mem)
}

// precargarParalelo lee path entero (sus tramos con datos) con n lectores,
// para dejarlo en la caché de página. Como precargar, pero sin tope y en
// paralelo. Para cuando ctx se cancela. Devuelve los bytes leídos.
func precargarParalelo(ctx context.Context, path string, n int) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	tam := fi.Size()
	trozos := make(chan [2]int64, n)
	var leidos int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, trozoCalentar)
			var mios int64
			for t := range trozos {
				if ctx.Err() != nil {
					continue
				}
				k, _ := f.ReadAt(buf[:t[1]], t[0])
				mios += int64(max(k, 0))
			}
			mu.Lock()
			leidos += mios
			mu.Unlock()
		}()
	}
	for off := int64(0); off < tam && ctx.Err() == nil; {
		ini, fin := siguientesDatos(f, off, tam)
		if ini >= tam {
			break
		}
		for p := ini; p < fin && ctx.Err() == nil; p += trozoCalentar {
			trozos <- [2]int64{p, min(trozoCalentar, fin-p)}
		}
		off = fin
	}
	close(trozos)
	wg.Wait()
	return leidos
}

// bytesEnCache dice cuántos bytes de path están en la caché de página, con
// mincore sobre un mapeo de solo lectura (que no trae nada). 0 si no se sabe.
func bytesEnCache(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 || int64(int(fi.Size())) != fi.Size() {
		return 0
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return 0
	}
	defer syscall.Munmap(data)
	pag := int64(os.Getpagesize())
	vec := make([]byte, (fi.Size()+pag-1)/pag)
	if _, _, e := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)), uintptr(unsafe.Pointer(&vec[0]))); e != 0 {
		return 0
	}
	var n int64
	for _, v := range vec {
		if v&1 != 0 {
			n++
		}
	}
	return min(n*pag, fi.Size())
}

// anclarDorado ancla el mem.file del dorado name, si no lo estaba ya. Antes
// pasa por la admisión de memoria con lo que va a anclar: es RAM que el
// kernel ya no puede reclamar, y una vez anclada sale de MemAvailable sola
// (páginas no desalojables), así que la reserva dura lo que tarda el mlock.
func (m *Manager) anclarDorado(name, mem string) error {
	if anclarFichero == nil {
		return ErrAncladoNoSoportado
	}
	m.ancladosMu.Lock()
	defer m.ancladosMu.Unlock()
	if _, ya := m.anclados[name]; ya {
		return nil
	}
	mib := int((allocatedBytes(mem) + 1<<20 - 1) >> 20)
	m.mu.Lock()
	if err := checkHostMemory(mib+m.pendingMiB, m.hotMemFilesMiBLocked()); err != nil {
		m.mu.Unlock()
		return err
	}
	m.pendingMiB += mib
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.pendingMiB -= mib; m.pendingMiB < 0 {
			m.pendingMiB = 0
		}
		m.mu.Unlock()
	}()
	a, err := anclarFichero(mem)
	if err != nil {
		return fmt.Errorf("pinning the memory of %s: %w", name, err)
	}
	if m.anclados == nil {
		m.anclados = map[string]anclaje{}
	}
	m.anclados[name] = a
	m.mu.Lock()
	if m.bytesAnclados == nil {
		m.bytesAnclados = map[string]int64{}
	}
	m.bytesAnclados[name] = a.bytes()
	m.mu.Unlock()
	log.Printf("snapshot %s: %d MiB of its memory pinned in RAM", name, a.bytes()>>20)
	return nil
}

// soltarAnclado suelta el anclado del dorado name, si lo tenía. Lo llaman
// Pin=false, el borrado del dorado y el cierre del Manager.
func (m *Manager) soltarAnclado(name string) {
	m.ancladosMu.Lock()
	a, ok := m.anclados[name]
	delete(m.anclados, name)
	m.mu.Lock()
	delete(m.bytesAnclados, name)
	m.mu.Unlock()
	m.ancladosMu.Unlock()
	if ok {
		a.soltar()
		log.Printf("snapshot %s: memory unpinned", name)
	}
}

// soltarAnclados suelta todos (Close).
func (m *Manager) soltarAnclados() {
	m.ancladosMu.Lock()
	todos := m.anclados
	m.anclados = nil
	m.mu.Lock()
	m.bytesAnclados = nil
	m.mu.Unlock()
	m.ancladosMu.Unlock()
	for _, a := range todos {
		a.soltar()
	}
}

func (m *Manager) estaAnclado(name string) bool {
	m.ancladosMu.Lock()
	defer m.ancladosMu.Unlock()
	_, ok := m.anclados[name]
	return ok
}
