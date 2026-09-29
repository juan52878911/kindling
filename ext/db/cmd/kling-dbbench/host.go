package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

// HostInfo es la cabecera del informe: sin ella un número no se puede comparar
// ni reproducir. Lo que no se pueda averiguar queda vacío, nunca inventado.
type HostInfo struct {
	Hostname    string `json:"hostname"`
	Kernel      string `json:"kernel"`
	CPU         string `json:"cpu"`
	CPUs        int    `json:"cpus"`
	MemTotalMiB int64  `json:"mem_total_mib"`
	// Nested: la CPU anuncia el bit "hypervisor" (este host es una VM).
	Nested    bool   `json:"nested"`
	Container string `json:"container"`
	Arch      string `json:"arch"`
	OS        string `json:"os"`
	Kling     string `json:"kling_version"`
	Docker    string `json:"docker_version"`
	GitSHA    string `json:"git_sha"`
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// cpuInfo saca el modelo y el bit hypervisor de /proc/cpuinfo.
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
		switch strings.TrimSpace(k) {
		case "model name":
			if model == "" {
				model = strings.TrimSpace(v)
			}
		case "flags":
			for _, fl := range strings.Fields(v) {
				if fl == "hypervisor" {
					hypervisor = true
				}
			}
		}
	}
	return model, hypervisor
}

// containerKind: lxc/docker/... si esto corre dentro de un contenedor.
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

// toolVersion ejecuta una orden corta y devuelve su primera línea (vacío si
// falla: la herramienta puede no estar).
func toolVersion(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return firstLine(string(out))
}

func hostInfo(klingBin, dockerBin string) HostInfo {
	h := HostInfo{
		Kernel:    readTrim("/proc/sys/kernel/osrelease"),
		CPUs:      runtime.NumCPU(),
		Container: containerKind(),
		Arch:      runtime.GOARCH,
		OS:        runtime.GOOS,
		GitSHA:    gitSHA(),
	}
	h.Hostname, _ = os.Hostname()
	h.CPU, h.Nested = cpuInfo()
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		h.MemTotalMiB, _ = parseMemTotalMiB(string(b))
	}
	if klingBin != "" {
		h.Kling = toolVersion(klingBin, "version")
	}
	if dockerBin != "" {
		h.Docker = toolVersion(dockerBin, "version", "--format", "{{.Server.Version}}")
	}
	return h
}

// memAvailableMiB lee MemAvailable; -1 si no se puede (no es Linux).
func memAvailableMiB() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return -1
	}
	if v, ok := parseMemAvailableMiB(string(b)); ok {
		return v
	}
	return -1
}

// psiSome lee "some avg10" de la presión de memoria; -1 si no existe.
func psiSome() float64 {
	b, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return -1
	}
	if v, ok := parsePSISome(string(b)); ok {
		return v
	}
	return -1
}

// diskUsage devuelve bytes usados y libres (para no root) del sistema de
// ficheros de path. Fuerza un sync antes: las escrituras pendientes no
// cuentan hasta que llegan al disco y falsearían el delta.
func diskUsage(path string) (used, avail int64, err error) {
	syscall.Sync()
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)
	return (int64(st.Blocks) - int64(st.Bfree)) * bs, int64(st.Bavail) * bs, nil
}
