package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// Comprobaciones de doctor sobre el host: el socket del daemon y su disco, y
// las de `kling up -check` como entradas de checks[].

// doctorDeRuntime pasa las comprobaciones de `kling up -check` a las de
// doctor: "runtime <qué>", con lo que se vio, por qué importa y el arreglo.
// Una que no pasa es fail, fatal o no: `up -check` sale con 1 por cualquiera.
func doctorDeRuntime(cs []check) []doctorCheck {
	out := make([]doctorCheck, 0, len(cs))
	for _, c := range cs {
		d := doctorCheck{Name: "runtime " + c.label, State: doctorOK, Detail: c.found}
		if !c.ok {
			d.State = doctorFail
			if c.why != "" {
				d.Detail = strings.TrimSpace(d.Detail + ": " + c.why)
			}
			d.Fix = strings.Join(c.fix, "; ")
		}
		out = append(out, d)
	}
	return out
}

// socketProbe es lo que se ve desde aquí del socket Unix del daemon.
type socketProbe struct {
	path     string
	exists   bool
	isSocket bool
	writable bool // se puede conectar (W_OK): sin él, "permission denied"
}

// sondearSocket mira el socket de endpoint, o nil si no es uno local.
func sondearSocket(endpoint string) *socketProbe {
	path := strings.TrimPrefix(endpoint, "unix://")
	if path == "" || strings.Contains(path, "://") {
		return nil
	}
	p := &socketProbe{path: path}
	fi, err := os.Stat(path)
	if err != nil {
		return p
	}
	p.exists = true
	p.isSocket = fi.Mode()&os.ModeSocket != 0
	p.writable = syscall.Access(path, 2 /* W_OK */) == nil
	return p
}

// checkSocket separa los tres porqués de un daemon que no contesta por un
// socket local: no hay socket (no corre ahí), no es un socket, o no es tuyo.
func checkSocket(p *socketProbe) (doctorCheck, bool) {
	if p == nil {
		return doctorCheck{}, false
	}
	c := doctorCheck{Name: "socket", State: doctorOK, Detail: p.path}
	switch {
	case !p.exists:
		c.State, c.Detail = doctorFail, "no socket at "+p.path+": no daemon listens there"
		c.Fix = "kling up   (or point kling at yours: kling context ls, KLING_HOST)"
	case !p.isSocket:
		c.State, c.Detail = doctorFail, p.path+" exists but is not a socket"
		c.Fix = "remove it and restart the daemon (kling up)"
	case !p.writable:
		c.State, c.Detail = doctorFail, "no permission to connect to "+p.path
		c.Fix = "add your user to KLING_SOCKET_USER in /etc/default/kling and restart the daemon (or use sudo)"
	}
	return c, true
}

// checkDisco compara el disco libre bajo la raíz del daemon con el mínimo de
// la admisión (KLING_MIN_FREE_DISK_MIB): por debajo, un run se rechaza con
// disk_full; por debajo del doble, se avisa antes de llegar. Sin la cifra
// (daemon anterior, o sin rol de admin) no se opina.
func checkDisco(info *api.Info) (doctorCheck, bool) {
	if info == nil || info.DiskFreeBytes <= 0 {
		return doctorCheck{}, false
	}
	minMiB := minDiscoDe(info)
	libre := info.DiskFreeBytes
	minB := minMiB << 20
	c := doctorCheck{Name: "disk", State: doctorOK,
		Detail: fmt.Sprintf("%s free under %s (minimum to admit a machine: %s)", human(libre), info.Root, human(minB))}
	fix := "kling ps -a and kling template ls show what holds disk; kling rm and kling template rm free it"
	switch {
	case libre < minB:
		c.State, c.Fix = doctorFail, fix
		c.Detail = fmt.Sprintf("%s free under %s, below the %s minimum: new machines are refused (disk_full)", human(libre), info.Root, human(minB))
	case libre < 2*minB:
		c.State, c.Fix = doctorWarn, fix
		c.Detail = fmt.Sprintf("%s free under %s, close to the %s minimum below which new machines are refused", human(libre), info.Root, human(minB))
	}
	return c, true
}

// minDiscoDe es el mínimo de disco libre del daemon en MiB: el de sus ajustes
// si los da, y si no el de su plataforma (2 GiB con Firecracker, 16 GiB en
// macOS: internal/machine).
func minDiscoDe(info *api.Info) int64 {
	if v, ok := info.Tuning["KLING_MIN_FREE_DISK_MIB"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	if info.Backend == "" || info.Backend == machine.BackendFirecracker {
		return 2048
	}
	return 16 << 10
}
