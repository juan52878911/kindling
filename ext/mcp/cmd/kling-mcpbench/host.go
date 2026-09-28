package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// HostInfo es la cabecera de cada resultado: sin ella, un número no se puede
// comparar con otro ni reproducir. Lo que no se puede averiguar queda vacío o
// "unknown", nunca inventado.
type HostInfo struct {
	Hostname    string `json:"hostname"`
	Kernel      string `json:"kernel"`
	CPU         string `json:"cpu"`
	CPUs        int    `json:"cpus"`
	MemTotalMiB int64  `json:"mem_total_mib"`
	// Nested: la CPU anuncia el bit "hypervisor", o sea, este host es a su vez
	// una VM y Firecracker corre con virtualización anidada.
	Nested bool `json:"nested"`
	// Container: el gestor de contenedores si esto corre dentro de uno (lxc en
	// un CT de Proxmox), "none" si no, "unknown" si no se pudo leer.
	Container   string `json:"container"`
	Kling       string `json:"kling_version"`
	Firecracker string `json:"firecracker"`
	Backend     string `json:"backend"`
	Arch        string `json:"arch"`
	GitSHA      string `json:"git_sha"`
	// PSILimit es el KLING_MAX_MEM_PRESSURE del DAEMON, que este proceso no
	// puede leer: lo pasa quien lanza la prueba con -psi-limit.
	PSILimit string `json:"psi_limit"`
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// cpuInfo saca el modelo y si hay bit hypervisor de /proc/cpuinfo.
func cpuInfo() (model string, hypervisor bool) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "model name":
			if model == "" {
				model = v
			}
		case "flags":
			if !hypervisor {
				for _, fl := range strings.Fields(v) {
					if fl == "hypervisor" {
						hypervisor = true
						break
					}
				}
			}
		}
	}
	return model, hypervisor
}

func memTotalMiB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			fs := strings.Fields(v)
			if len(fs) > 0 {
				kb, _ := strconv.ParseInt(fs[0], 10, 64)
				return kb >> 10
			}
		}
	}
	return 0
}

// containerKind mira /run/systemd/container (lo escribe systemd dentro de un
// contenedor) y, si no, la variable container= del PID 1 (legible solo como
// root). Sin ninguna de las dos pistas: "none" si /proc existe, "unknown" si no.
func containerKind() string {
	if v := readTrim("/run/systemd/container"); v != "" {
		return v
	}
	if b, err := os.ReadFile("/proc/1/environ"); err == nil {
		for _, kv := range strings.Split(string(b), "\x00") {
			if v, ok := strings.CutPrefix(kv, "container="); ok && v != "" {
				return v
			}
		}
		return "none"
	}
	return "unknown"
}

func gitSHA() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev == "" {
		return ""
	}
	return rev + dirty
}

// hostInfo reúne la cabecera. dc puede ser nil (sin daemon): entonces faltan
// las versiones de kling y Firecracker.
func hostInfo(ctx context.Context, dc *http.Client, psiLimit string) HostInfo {
	h := HostInfo{
		Kernel:      readTrim("/proc/sys/kernel/osrelease"),
		CPUs:        runtime.NumCPU(),
		MemTotalMiB: memTotalMiB(),
		Container:   containerKind(),
		Arch:        runtime.GOARCH,
		GitSHA:      gitSHA(),
		PSILimit:    psiLimit,
	}
	h.Hostname, _ = os.Hostname()
	h.CPU, h.Nested = cpuInfo()
	if dc != nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://kling/info", nil)
		if err == nil {
			if resp, err := dc.Do(req); err == nil {
				var info struct {
					Version     string `json:"version"`
					Firecracker string `json:"firecracker"`
					Backend     string `json:"backend"`
					Arch        string `json:"arch"`
				}
				if json.NewDecoder(resp.Body).Decode(&info) == nil {
					h.Kling, h.Firecracker, h.Backend = info.Version, info.Firecracker, info.Backend
					if info.Arch != "" {
						h.Arch = info.Arch
					}
				}
				resp.Body.Close()
			}
		}
	}
	return h
}
