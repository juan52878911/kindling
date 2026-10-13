package guest

// GET /procs: los procesos de usuario vivos (api.GuestProcs).
//
// Lo pregunta `kling save` antes de volcar un dorado: un proceso vivo en el
// volcado despierta igual en todas las copias, y con él lo que guarde en
// memoria a partir de una semilla tomada antes (Math.random de V8, el DRBG de
// OpenSSL dentro del proceso, random de Python). /resync vuelve a sembrar el
// kernel, no eso (#175).
//
// Solo lee /proc: cuántos hay y cómo se llaman, sin argumentos ni entorno, que
// pueden llevar secretos.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// procRoot es /proc (variable para los tests).
var procRoot = "/proc"

// procStat son los campos de /proc/<pid>/stat que hacen falta.
type procStat struct {
	comm  string
	state byte
	ppid  int
	pgrp  int
}

func readProcStat(root string, pid int) (procStat, bool) {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, false
	}
	// "pid (comm) S ppid pgrp ...": comm puede llevar espacios y paréntesis,
	// así que se corta por el último ')'.
	s := string(b)
	i, j := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if i < 0 || j < i {
		return procStat{}, false
	}
	f := strings.Fields(s[j+1:])
	if len(f) < 3 || len(f[0]) != 1 {
		return procStat{}, false
	}
	ppid, err1 := strconv.Atoi(f[1])
	pgrp, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil {
		return procStat{}, false
	}
	return procStat{comm: s[i+1 : j], state: f[0][0], ppid: ppid, pgrp: pgrp}, true
}

// listProcs cuenta los procesos de usuario de root, sin self ni sus
// antecesores. servicePgrp es el grupo del servicio con OnRestore (0 = sin):
// los suyos cuentan como cubiertos.
func listProcs(root string, self, servicePgrp int) api.GuestProcs {
	ents, err := os.ReadDir(root)
	if err != nil {
		return api.GuestProcs{}
	}
	stats := map[int]procStat{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if st, ok := readProcStat(root, pid); ok {
			stats[pid] = st
		}
	}
	// El agente y quien lo lanzó (el init, un /entrypoint de shell) no
	// cuentan: son de kindling, y vuelven a sembrar en /resync.
	mine := map[int]bool{}
	for p := self; p > 0 && !mine[p]; p = stats[p].ppid {
		mine[p] = true
	}
	var out api.GuestProcs
	names := map[string]bool{}
	for pid, st := range stats {
		switch {
		case mine[pid], st.state == 'Z', st.state == 'X':
			continue
		// Hilos del kernel: kthreadd (2) y sus hijos, sin cmdline.
		case pid == 2 || st.ppid == 2:
			continue
		}
		if cl, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cmdline")); err != nil || len(cl) == 0 {
			continue
		}
		out.Count++
		if servicePgrp > 0 && st.pgrp == servicePgrp {
			out.Covered++
		}
		names[st.comm] = true
	}
	for n := range names {
		out.Names = append(out.Names, n)
	}
	slices.Sort(out.Names)
	if len(out.Names) > api.GuestProcsMaxNames {
		out.Names = out.Names[:api.GuestProcsMaxNames]
	}
	return out
}

// servicePgrp es el grupo de procesos del servicio de la imagen si declara
// OnRestore (lo lanza en su propio grupo: StartTracked), o 0.
func servicePgrp() int {
	serviceState.mu.Lock()
	s := serviceState.svc
	serviceState.mu.Unlock()
	if s == nil {
		return 0
	}
	if action, _, err := api.ParseOnRestore(s.spec.OnRestore); err != nil || action == api.OnRestoreNone {
		return 0
	}
	if st := s.Status(0); st.Running {
		return st.PID
	}
	return 0
}

// ProcsHandler sirve GET /procs.
func ProcsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		p := listProcs(procRoot, os.Getpid(), servicePgrp())
		// Solo las que nadie va a renovar: con ssh-keygen en la imagen, cada
		// copia regenera las suyas (sshRenewal).
		if sshRenewal() == nil {
			p.HostKeys = hostKeys()
		}
		_ = json.NewEncoder(w).Encode(p)
	}
}

// listeningPorts son los puertos TCP en LISTEN de los procesos del grupo pgrp:
// los sockets de /proc/net/tcp{,6} cuyo inodo abre alguno de ellos. Es lo que
// OnRestoreRestart vuelve a esperar tras reiniciar el servicio, para que una
// imagen sin sonda no se dé por lista antes de que el proceso nuevo escuche.
func listeningPorts(root string, pgrp int) map[int]bool {
	inodes := map[string]bool{}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if st, ok := readProcStat(root, pid); !ok || st.pgrp != pgrp {
			continue
		}
		fdDir := filepath.Join(root, e.Name(), "fd")
		fds, _ := os.ReadDir(fdDir)
		for _, fd := range fds {
			if l, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil {
				if in, ok := strings.CutPrefix(l, "socket:["); ok {
					inodes[strings.TrimSuffix(in, "]")] = true
				}
			}
		}
	}
	ports := map[int]bool{}
	if len(inodes) == 0 {
		return ports
	}
	for _, f := range []string{"net/tcp", "net/tcp6"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n")[1:] {
			// sl local_address rem_address st tx:rx tr:when retrnsmt uid timeout inode
			c := strings.Fields(l)
			if len(c) < 10 || c[3] != "0A" || !inodes[c[9]] {
				continue
			}
			_, port, ok := strings.Cut(c[1], ":")
			if !ok {
				continue
			}
			if n, err := strconv.ParseInt(port, 16, 32); err == nil {
				ports[int(n)] = true
			}
		}
	}
	return ports
}
