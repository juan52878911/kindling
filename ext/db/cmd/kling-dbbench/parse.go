package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// Los parsers de este fichero leen la salida de las herramientas que el banco
// lanza (kling, docker, /proc). Son funciones puras para poder probarlas con
// salida de ejemplo.

// runInfo es lo que cuenta `kling run`.
type runInfo struct {
	ID   string // 12 caracteres
	Name string
	Ms   int    // thaw_ms (instanciada) o boot_ms (arranque en frío)
	From string // vacío si arrancó en frío
}

var (
	reRunFrom = regexp.MustCompile(`^([0-9a-f]{12})\s+(\S+)\s+instantiated from (\S+) in (\d+) ms`)
	reRunCold = regexp.MustCompile(`^([0-9a-f]{12})\s+(\S+)\s+booted cold in (\d+) ms`)
)

// parseRunOutput lee la primera línea de `kling run`:
//
//	<id12>  <name>  instantiated from <tpl> in <N> ms
//	<id12>  <name>  booted cold in <N> ms
func parseRunOutput(out string) (runInfo, error) {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if m := reRunFrom.FindStringSubmatch(l); m != nil {
			ms, _ := strconv.Atoi(m[4])
			return runInfo{ID: m[1], Name: m[2], From: m[3], Ms: ms}, nil
		}
		if m := reRunCold.FindStringSubmatch(l); m != nil {
			ms, _ := strconv.Atoi(m[3])
			return runInfo{ID: m[1], Name: m[2], Ms: ms}, nil
		}
	}
	return runInfo{}, fmt.Errorf("unrecognised `kling run` output: %q", firstLine(out))
}

// parseInspect lee `kling inspect` y devuelve la dirección host:puerto por la
// que el host llega al puerto port de la máquina (IP en Linux, reenvío en
// macOS; ver api.Machine.Addr).
func parseInspect(b []byte, port int) (string, error) {
	var m api.Machine
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("inspect: %w", err)
	}
	if !m.Reachable() {
		return "", errors.New("the machine has no address reachable from the host")
	}
	return m.Addr(port), nil
}

// parseFork lee `kling sandbox fork -json`.
func parseFork(b []byte) (*api.ForkResult, error) {
	var r api.ForkResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("fork: %w", err)
	}
	return &r, nil
}

// parseDockerPort lee `docker port <c> 5432/tcp` ("127.0.0.1:49153", y a
// veces una segunda línea "[::]:49153") y devuelve siempre loopback:puerto:
// el contenedor se publica solo en 127.0.0.1.
func parseDockerPort(out string) (string, error) {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		_, p, err := net.SplitHostPort(l)
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n < 65536 {
			return net.JoinHostPort("127.0.0.1", p), nil
		}
	}
	return "", fmt.Errorf("unrecognised `docker port` output: %q", firstLine(out))
}

// parseMemAvailableMiB saca MemAvailable de /proc/meminfo, en MiB.
func parseMemAvailableMiB(meminfo string) (int64, bool) {
	return meminfoMiB(meminfo, "MemAvailable:")
}

// parseMemTotalMiB saca MemTotal de /proc/meminfo, en MiB.
func parseMemTotalMiB(meminfo string) (int64, bool) {
	return meminfoMiB(meminfo, "MemTotal:")
}

func meminfoMiB(meminfo, key string) (int64, bool) {
	for _, l := range strings.Split(meminfo, "\n") {
		if v, ok := strings.CutPrefix(l, key); ok {
			f := strings.Fields(v)
			if len(f) == 0 {
				return 0, false
			}
			kb, err := strconv.ParseInt(f[0], 10, 64)
			if err != nil {
				return 0, false
			}
			return kb >> 10, true
		}
	}
	return 0, false
}

// parsePSISome lee /proc/pressure/memory y devuelve "some avg10".
func parsePSISome(psi string) (float64, bool) {
	for _, l := range strings.Split(psi, "\n") {
		if !strings.HasPrefix(l, "some ") {
			continue
		}
		for _, f := range strings.Fields(l) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				x, err := strconv.ParseFloat(v, 64)
				return x, err == nil
			}
		}
	}
	return 0, false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
