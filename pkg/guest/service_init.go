package guest

// El servicio cuando el init de la imagen es el PID 1 (api.ServiceSpec.Init).
//
// El init de kindling ya lo ejecutó en su lugar (init.go) y lanzó a este
// agente como hijo suyo, así que aquí no hay nada que arrancar ni relanzar: el
// "servicio" es el PID 1. Parar es mandarle la señal de parada de la imagen
// (SIGTERM para s6-overlay y supervisord) y esperar a que el resto de
// procesos acabe, que es cuando ha apagado lo que supervisa. Lo que sigue lo
// hace el daemon, igual que con un servicio normal: vaciar los volúmenes y
// matar la máquina.

import (
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// initLoop es el bucle de un servicio Init: registra el PID 1 como su
// proceso y, cuando llega la orden de parar (Stop cierra wake), espera a que
// se vayan los demás procesos de la máquina.
func (s *Service) initLoop() {
	defer close(s.done)
	for _, w := range applySysctls(s.spec.Sysctls) {
		log.Printf("kling-guest: %s", w)
	}
	p, err := os.FindProcess(1)
	if err != nil {
		s.mu.Lock()
		s.err = err.Error()
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.cmd, s.starts = &exec.Cmd{Process: p}, 1
	s.mu.Unlock()
	<-s.wake

	timeout := time.Duration(s.spec.StopTimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	for end := time.Now().Add(timeout); time.Now().Before(end) && otherProcs(procRoot, os.Getpid()) > 0; {
		time.Sleep(50 * time.Millisecond)
	}
	syncFS()
}

// otherProcs cuenta los procesos de la máquina que no son el PID 1, ni el
// agente (self), ni sus hijos (las sesiones de exec), ni hilos del kernel, ni
// zombis.
func otherProcs(root string, self int) int {
	ents, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == 1 || pid == 2 || pid == self {
			continue
		}
		b, err := os.ReadFile(root + "/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) estado ppid ...: comm puede llevar espacios y paréntesis.
		st := string(b)
		i := strings.LastIndexByte(st, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(st[i+1:])
		if len(f) < 2 || f[0] == "Z" || f[0] == "X" {
			continue
		}
		if ppid, _ := strconv.Atoi(f[1]); ppid == 2 || ppid == self {
			continue
		}
		n++
	}
	return n
}
