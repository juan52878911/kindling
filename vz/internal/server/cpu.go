package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Tope de CPU en macOS: lo que en Linux hace el cgroup de cada microVM.
//
// Virtualization.framework no limita la CPU de una VM, y sin límite un bucle
// infinito dentro de un sandbox se comía un núcleo entero del Mac por vCPU. El
// regulador hace lo mismo que cpu.max: cada ventana mide la CPU que gastó el
// proceso auxiliar de Apple donde corren las vCPU y, si se pasó del
// presupuesto, pausa la VM el tiempo justo para que la media quede en el
// techo. Pausar y reanudar cuestan milisegundos, así que con ventanas de 100 ms
// el sobrecoste es pequeño; el precio, como con cpu.max, es que el invitado ve
// su CPU a trompicones cuando va al límite.
//
// Como el impulso de arranque de Linux, no regula hasta que el agente del
// invitado escucha (o pasa la gracia): arrancar el kernel a medio núcleo
// costaba cientos de ms a cada sandbox.

const (
	// cpuVentanaDef es cada cuánto se mide y decide, como el periodo de cpu.max.
	cpuVentanaDef = 100 * time.Millisecond
	// cpuGraciaDef es lo más que se espera al agente antes de regular.
	cpuGraciaDef = 10 * time.Second
	// cpuPausaMax acota una pausa: aunque la cuenta pida más, se vuelve a
	// medir antes.
	cpuPausaMax = time.Second
	// agentPort es el puerto del agente del invitado (api.GuestPort del núcleo).
	agentPort = 8080
)

// putKlingCPU fija el techo: {"pct": N}, en porcentaje de un núcleo (como
// cpu_pct del núcleo). 0 lo quita. Se puede cambiar cuando se quiera.
func (s *Server) putKlingCPU(w http.ResponseWriter, r *http.Request) {
	var c struct {
		Pct int `json:"pct"`
	}
	if err := decode(r, maxConfigBody, &c); err != nil {
		fault(w, err)
		return
	}
	if c.Pct < 0 || c.Pct > 100*64 {
		fault(w, fmt.Errorf("pct must be between 0 and %d", 100*64))
		return
	}
	s.mu.Lock()
	s.cpuPct = c.Pct
	arrancar := !s.cpuEnMarcha && s.d.CPUTime != nil
	if arrancar {
		s.cpuEnMarcha = true
	}
	s.mu.Unlock()
	if arrancar {
		go s.regularCPU()
	}
	noContent(w)
}

func (s *Server) getKlingCPU(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := map[string]any{"pct": s.cpuPct, "throttled_ms": s.cpuPausado.Milliseconds()}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// regularCPU es la goroutine del regulador. Termina cuando se para la VM.
func (s *Server) regularCPU() {
	ventana, gracia := s.cpuVentana, s.cpuGracia
	if ventana <= 0 {
		ventana = cpuVentanaDef
	}
	if gracia <= 0 {
		gracia = cpuGraciaDef
	}

	// Primero, dejar arrancar al invitado a toda máquina.
	limite := time.Now().Add(gracia)
	for time.Now().Before(limite) {
		s.mu.Lock()
		n, st := s.net, s.st
		s.mu.Unlock()
		if st == stStopped {
			return
		}
		if n != nil && st == stRunning {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			ok := n.Probe(ctx, agentPort)
			cancel()
			if ok {
				break
			}
		}
		time.Sleep(ventana)
	}

	var (
		vmBase VM
		base   time.Duration
		t0     time.Time
	)
	for {
		time.Sleep(ventana)
		s.mu.Lock()
		if s.st == stStopped {
			s.mu.Unlock()
			return
		}
		pct, vcpus := s.cpuPct, 1
		if s.spec != nil && s.spec.MachineConfig.VCPUCount > 0 {
			vcpus = s.spec.MachineConfig.VCPUCount
		}
		if s.st != stRunning || s.vm == nil || pct <= 0 || pct >= vcpus*100 {
			vmBase = nil
			s.mu.Unlock()
			continue
		}
		usado, err := s.d.CPUTime()
		ahora := time.Now()
		if err != nil {
			s.mu.Unlock()
			continue
		}
		if vmBase != s.vm || usado < base {
			// VM nueva (restaurada) o contador reiniciado: empezar a contar.
			vmBase, base, t0 = s.vm, usado, ahora
			s.mu.Unlock()
			continue
		}
		gastado, presupuesto := usado-base, ahora.Sub(t0)*time.Duration(pct)/100
		base, t0 = usado, ahora
		if gastado <= presupuesto {
			s.mu.Unlock()
			continue
		}
		// Pausa para que, contando la ventana y la pausa, la media sea pct.
		pausa := (gastado - presupuesto) * 100 / time.Duration(pct)
		if pausa > cpuPausaMax {
			pausa = cpuPausaMax
		}
		vm := s.vm
		if err := vm.Pause(); err != nil {
			s.mu.Unlock()
			continue
		}
		s.regulando = true
		s.mu.Unlock()

		time.Sleep(pausa)

		s.mu.Lock()
		// Si mientras tanto el núcleo la pausó (patchVM) o se paró, ya no es
		// nuestra: no se reanuda.
		if s.regulando && s.vm == vm && s.st == stRunning {
			_ = vm.Resume()
		}
		s.regulando = false
		s.cpuPausado += pausa
		if u, err := s.d.CPUTime(); err == nil {
			base = u
		}
		t0 = time.Now()
		s.mu.Unlock()
	}
}
