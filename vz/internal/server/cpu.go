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
// Qué significa pct: lo mismo que cpu_pct en Linux (cpu.max = "pct*1000
// 100000"): porcentaje de UN núcleo del host para la VM ENTERA, sume lo que
// sumen sus vCPU. Con 2 vCPU y pct 50, las dos juntas gastan medio núcleo, es
// decir, cada una corre de media a una cuarta parte; pct >= 100×vCPU es no
// tener techo. Para medio núcleo POR vCPU está cpu_pct_per_vcpu en la receta
// de la imagen, que el núcleo multiplica antes de mandarlo aquí.
//
// Virtualization.framework no limita la CPU de una VM, y sin límite un bucle
// infinito dentro de un sandbox se comía un núcleo del Mac por vCPU. Los hilos
// de vCPU viven en el auxiliar de Apple (com.apple.Virtualization.VirtualMachine),
// que corre con nuestro usuario pero es un binario de la plataforma:
// task_for_pid se niega, así que no hay forma de tocar un hilo suelto (ni
// suspenderlo ni cambiarle la QoS). Lo que sí se puede es tocar el proceso
// entero: setpriority solo deja subir el nice (y no bajarlo después), y
// PRIO_DARWIN_BG (taskpolicy -b) lo manda a los núcleos de eficiencia, pero
// ninguno de los dos es un TECHO: con el Mac libre, la VM sigue comiéndose sus
// núcleos. El techo, como cpu.max, hay que imponerlo parando la VM.
//
// Cómo: un cubo de fichas. Se llena a pct % de un núcleo, la VM lo vacía con la
// CPU que gasta el auxiliar y, cuando se queda sin fichas, se para el auxiliar
// hasta ganar una tajada (periodo × pct): cada parada dura un periodo, 20 ms,
// en vez de las de hasta 300 ms que salían midiendo ventanas fijas de 100 ms.
// El cubo guarda como mucho cpuRafaga de CPU, así que tras estar ociosa la VM
// puede ir a toda máquina un momento, como en el primer periodo de cpu.max.
// Mientras hay fichas, se vuelve a medir justo cuando podrían acabarse (fichas
// ÷ vCPU), así que una VM ociosa cuesta pocas mediciones por segundo.
//
// El freno es SIGSTOP/SIGCONT al auxiliar entero (Deps.Freeze, que manda un
// proceso aparte: ver vz/internal/footprint/freno_darwin.go). Cuesta
// microsegundos y, sobre todo, el invitado ve pasar el tiempo parado, como
// bajo cpu.max. Pausar la VM por el framework, que es lo que se hacía antes y
// lo que queda si Freeze falla, para también el reloj del invitado: una VM
// regulada se quedaba atrás (2 vCPU a tope con techo 50: 23 s de retraso en
// 30 s, medido en un M4), y con ella los TTL, los certificados y los plazos de
// dentro.
//
// Como el impulso de arranque de Linux (internal/machine/arranque_cpu.go),
// hasta que el agente del invitado escucha (o pasa la gracia) el techo es
// max(pct, 100): un núcleo entero para arrancar, no más.

const (
	// cpuPeriodoDef es lo que dura cada parada del regulador.
	cpuPeriodoDef = 20 * time.Millisecond
	// cpuRafaga acota lo que se acumula ociosa: el periodo de cpu.max.
	cpuRafaga = 100 * time.Millisecond
	// cpuMedidaMin es lo menos que se espera entre dos mediciones.
	cpuMedidaMin = time.Millisecond
	// cpuGraciaDef es lo más que se espera al agente antes de regular.
	cpuGraciaDef = 10 * time.Second
	// cpuPausaMax acota una parada y la deuda que se arrastra.
	cpuPausaMax = time.Second
	// cpuPasoAgente es cada cuánto se pregunta si el agente escucha.
	cpuPasoAgente = 20 * time.Millisecond
	// agentPort es el puerto del agente del invitado (api.GuestPort del núcleo).
	agentPort = 8080
)

// reloj es el tiempo del regulador; las pruebas lo sustituyen por uno falso.
type reloj interface {
	ahora() time.Time
	dormir(time.Duration)
}

type relojReal struct{}

func (relojReal) ahora() time.Time       { return time.Now() }
func (relojReal) dormir(d time.Duration) { time.Sleep(d) }

// cubo es la cuenta del techo. fichas es la CPU que la VM aún puede gastar
// sin frenar; negativa, la que debe.
type cubo struct {
	fichas time.Duration
	base   time.Duration // CPU del auxiliar en la última cuenta
	t      time.Time     // cuándo fue
}

// cuenta suma lo ganado desde la última cuenta (pct % del tiempo) y resta lo
// gastado (usado - base).
func (c *cubo) cuenta(ahora time.Time, usado time.Duration, pct int) {
	c.fichas += ahora.Sub(c.t)*time.Duration(pct)/100 - (usado - c.base)
	if lleno := cpuRafaga * time.Duration(pct) / 100; c.fichas > lleno {
		c.fichas = lleno
	}
	if deuda := -cpuPausaMax * time.Duration(pct) / 100; c.fichas < deuda {
		c.fichas = deuda
	}
	c.base, c.t = usado, ahora
}

// decide dice qué hacer con las fichas que hay: parar la VM el tiempo de
// ganar una tajada más lo que se debe (parar > 0), o dormir hasta la próxima
// medida, que es cuando, gastando a vcpus núcleos, podrían acabarse.
func (c *cubo) decide(pct, vcpus int, periodo time.Duration) (parar, dormir time.Duration) {
	if c.fichas <= 0 {
		parar = periodo + (-c.fichas)*100/time.Duration(pct)
		return min(parar, cpuPausaMax), 0
	}
	return 0, min(max(c.fichas/time.Duration(vcpus), cpuMedidaMin), cpuRafaga)
}

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
		go s.esperarAgente()
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

// esperarAgente marca cpuListo cuando el agente del invitado escucha o pasa la
// gracia: desde entonces rige el techo configurado y no el de arranque.
func (s *Server) esperarAgente() {
	gracia := s.cpuGracia
	if gracia <= 0 {
		gracia = cpuGraciaDef
	}
	for limite := time.Now().Add(gracia); time.Now().Before(limite); time.Sleep(cpuPasoAgente) {
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
	}
	s.mu.Lock()
	s.cpuListo = true
	s.mu.Unlock()
}

// regularCPU es la goroutine del regulador. Termina cuando se para la VM.
func (s *Server) regularCPU() {
	rj, periodo := s.cpuReloj, s.cpuPeriodo
	if rj == nil {
		rj = relojReal{}
	}
	if periodo <= 0 {
		periodo = cpuPeriodoDef
	}
	var (
		c      cubo
		vmBase VM
	)
	for {
		// Si el último SIGCONT falló, el auxiliar sigue parado (congelado) y
		// nadie más lo iba a soltar: parado no gasta CPU, el cubo se llena y
		// el regulador ya no vuelve a frenar, que era lo único que lo
		// reintentaba. La VM se quedaba parada para siempre figurando running.
		// No hace nada si no está parado.
		s.soltarFreno()
		s.mu.Lock()
		if s.st == stStopped {
			s.mu.Unlock()
			return
		}
		pct, vcpus := s.cpuPct, 1
		if s.spec != nil && s.spec.MachineConfig.VCPUCount > 0 {
			vcpus = s.spec.MachineConfig.VCPUCount
		}
		if !s.cpuListo && pct > 0 {
			pct = max(pct, 100) // el impulso de arranque
		}
		if s.st != stRunning || s.vm == nil || pct <= 0 || pct >= vcpus*100 {
			vmBase = nil
			s.mu.Unlock()
			rj.dormir(cpuRafaga)
			continue
		}
		usado, err := s.d.CPUTime()
		ahora := rj.ahora()
		if err != nil {
			s.mu.Unlock()
			rj.dormir(cpuRafaga)
			continue
		}
		if vmBase != s.vm || usado < c.base {
			// VM nueva (restaurada), techo recién puesto o contador reiniciado:
			// empezar con el cubo lleno, como un cgroup nuevo.
			vmBase = s.vm
			c = cubo{fichas: cpuRafaga * time.Duration(pct) / 100, base: usado, t: ahora}
		} else {
			c.cuenta(ahora, usado, pct)
		}
		parar, dormir := c.decide(pct, vcpus, periodo)
		if parar == 0 {
			s.mu.Unlock()
			rj.dormir(dormir)
			continue
		}
		vm := s.vm
		if !s.frenar(vm) {
			s.mu.Unlock()
			rj.dormir(periodo)
			continue
		}
		s.mu.Unlock()

		t0 := rj.ahora()
		rj.dormir(parar)
		s.soltarRegulador(vm)
		s.mu.Lock()
		s.cpuPausado += rj.ahora().Sub(t0)
		s.mu.Unlock()
	}
}

// frenar para la VM para el regulador. Se llama con s.mu tomado y la VM
// corriendo; false si no se pudo. Si el freno de señales falla (p. ej. un
// perfil de sandbox que no deja mandarlas), se avisa una vez y se pausa por
// el framework.
func (s *Server) frenar(vm VM) bool {
	if s.d.Freeze != nil {
		s.frenoMu.Lock()
		err := s.d.Freeze(true)
		if err == nil {
			s.congelado = true
		}
		s.frenoMu.Unlock()
		if err == nil {
			return true
		}
		if !s.frenoAviso {
			s.frenoAviso = true
			s.d.Logf("warning: could not stop the VM's helper for the CPU ceiling, pausing the VM instead: %v", err)
		}
	}
	if vm.Pause() != nil {
		return false
	}
	s.regulando = true
	return true
}

// soltarRegulador deshace frenar al acabar la parada. Sin s.mu tomado.
func (s *Server) soltarRegulador(vm VM) {
	s.soltarFreno()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Si mientras tanto el núcleo la pausó (patchVM) o se paró, ya no es
	// nuestra: no se reanuda.
	if s.regulando && s.vm == vm && s.st == stRunning {
		_ = vm.Resume()
	}
	s.regulando = false
}

// soltarFreno reanuda el auxiliar si el regulador lo tiene parado. Lo llama
// cualquiera que vaya a pedirle algo al framework: parado, no contestaría
// hasta el final de la parada. No toma s.mu, así que vale con s.mu tomado o
// sin él. Un SIGCONT no reanuda una VM en pausa del framework: si el núcleo
// la pausó, sigue pausada.
func (s *Server) soltarFreno() {
	s.frenoMu.Lock()
	defer s.frenoMu.Unlock()
	if !s.congelado {
		return
	}
	if err := s.d.Freeze(false); err != nil {
		// El regulador lo reintenta en cada vuelta: se avisa una vez por
		// racha, no una por intento.
		if !s.sueltaAviso {
			s.sueltaAviso = true
			s.d.Logf("warning: could not resume the VM's helper after a CPU pause (will retry): %v", err)
		}
		return
	}
	s.congelado, s.sueltaAviso = false, false
}
