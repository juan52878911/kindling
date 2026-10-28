package main

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// Un Mac donde hv vale, y cada cosa que lo tumba a vz con su motivo.
func TestElegirAuto(t *testing.T) {
	bien := func() entornoAuto {
		return entornoAuto{
			macOS:   "26.5",
			hv:      "/u/bin/kling-hv",
			existe:  func(string) bool { return true },
			firmado: func(string) bool { return true },
			sonda:   func(string) (string, error) { return "ok\n", nil },
		}
	}
	if b, m := elegirAuto(bien()); b != "hv" || !strings.Contains(m, "probed") {
		t.Fatalf("Mac sano = %s (%s)", b, m)
	}
	casos := map[string]struct {
		cambia func(*entornoAuto)
		motivo string
	}{
		"macOS 14":        {func(e *entornoAuto) { e.macOS = "14.6" }, "older than 15.2"},
		"macOS 15.1":      {func(e *entornoAuto) { e.macOS = "15.1" }, "older than 15.2"},
		"sin versión":     {func(e *entornoAuto) { e.macOS = "" }, "could not read"},
		"sin kling-hv":    {func(e *entornoAuto) { e.existe = func(string) bool { return false } }, "not found"},
		"sin entitlement": {func(e *entornoAuto) { e.firmado = func(string) bool { return false } }, "com.apple.security.hypervisor"},
		"sonda que falla": {func(e *entornoAuto) {
			e.sonda = func(string) (string, error) {
				return "kling-hv: hv_vm_create failed: 0xfae9400f\n", errors.New("exit 1")
			}
		}, "0xfae9400f"},
		"sonda sin salida": {func(e *entornoAuto) {
			e.sonda = func(string) (string, error) { return "", errors.New("signal: killed") }
		}, "killed"},
		"KLING_VMM a otro":  {func(e *entornoAuto) { e.env = "/opt/kling-vz" }, "not a kling-hv"},
		"congeladas con vz": {func(e *entornoAuto) { e.volcadosVZ = 2 }, "2 frozen machines or goldens"},
		"vivas con vz":      {func(e *entornoAuto) { e.vivosVZ = 1 }, "1 running machines were started with kling-vz"},
	}
	for nombre, c := range casos {
		e := bien()
		c.cambia(&e)
		b, m := elegirAuto(e)
		if b != "vz" || !strings.Contains(m, c.motivo) {
			t.Errorf("%s = %s (%s), want vz (…%s…)", nombre, b, m, c.motivo)
		}
	}
	// KLING_VMM con una ruta a un kling-hv: se sondea esa.
	e := bien()
	e.env = "/opt/x/kling-hv"
	var sondeada string
	e.sonda = func(p string) (string, error) { sondeada = p; return "ok", nil }
	if b, _ := elegirAuto(e); b != "hv" || sondeada != "/opt/x/kling-hv" {
		t.Fatalf("KLING_VMM=kling-hv: %s, probed %q", b, sondeada)
	}
	// La sonda que falla a propósito, para probar la caída sin romper nada.
	t.Setenv("KLING_HV_PROBE_FAIL", "1")
	if _, err := sondaHV("/no/existe"); err == nil {
		t.Fatal("KLING_HV_PROBE_FAIL=1 no la tumba")
	}
}

// auto se resuelve; vz y hv explícitos dicen de dónde salen; Linux no cambia.
func TestResolverBackend(t *testing.T) {
	falso := func(string) entornoAuto {
		return entornoAuto{macOS: "26.5", hv: "kling-hv",
			existe: func(string) bool { return true }, firmado: func(string) bool { return true },
			sonda: func(string) (string, error) { return "ok", nil }}
	}
	casos := []struct {
		ajuste, env, goos, b, motivo string
	}{
		{"", "", "darwin", "hv", "auto: macOS 26.5"},
		{"auto", "", "darwin", "hv", "auto: "},
		{"vz", "", "darwin", "vz", "daemon.vmm vz"},
		{"hv", "", "darwin", "hv", "daemon.vmm hv"},
		{"hv", "vz", "darwin", "vz", "KLING_VMM=vz"},
		{"vz", "auto", "darwin", "hv", "auto: "},
		{"", "", "linux", "firecracker", ""},
	}
	for _, c := range casos {
		// Cada binario solo habla los backends de su plataforma.
		if c.goos != runtime.GOOS || runtime.GOARCH != "arm64" {
			continue
		}
		b, m, err := resolverBackend(c.ajuste, c.env, c.goos, "arm64", falso)
		if err != nil || b != c.b || !strings.HasPrefix(m, c.motivo) || (c.motivo == "" && m != "") {
			t.Errorf("resolverBackend(%q, %q, %s) = %s, %q, %v; want %s, %q…", c.ajuste, c.env, c.goos, b, m, err, c.b, c.motivo)
		}
	}
}
