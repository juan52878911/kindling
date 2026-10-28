package main

// daemon.vmm auto (el de por defecto en macOS): el daemon elige hv o vz al
// arrancar y no lo cambia hasta reiniciarse. hv si este macOS lo admite y
// kling-hv está, va firmado con su entitlement y pasa su sonda (--probe: crea
// y destruye una VM vacía con su GIC, unos ms); si no, vz. La elección y su
// motivo salen en el log, en GET /info (backend_reason) y en `kling status -v`.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/config"
)

// entornoAuto es lo que mira la elección automática, inyectable en los tests.
type entornoAuto struct {
	macOS   string                       // kern.osproductversion
	env     string                       // KLING_VMM
	hv      string                       // kling-hv resuelto (ruta o nombre a secas)
	existe  func(string) bool            // el binario está y es ejecutable
	firmado func(string) bool            // lleva com.apple.security.hypervisor
	sonda   func(string) (string, error) // kling-hv --probe
	// volcadosVZ son las máquinas congeladas y los dorados de la raíz hechos
	// con kling-vz: kling-hv no carga su memoria.
	volcadosVZ int
}

// elegirAuto decide entre hv y vz y dice por qué, en una línea.
func elegirAuto(e entornoAuto) (backend, motivo string) {
	vz := machine.BackendVZ
	hv := e.hv
	if e.env != "" && e.env != config.VMMAuto && e.env != config.VMMVZ && e.env != config.VMMHV && e.env != config.VMMFirecracker {
		// KLING_VMM con una ruta: decide su nombre.
		if !strings.HasPrefix(filepath.Base(e.env), "kling-hv") {
			return vz, fmt.Sprintf("KLING_VMM=%s is not a kling-hv", e.env)
		}
		hv = e.env
	}
	switch {
	case e.macOS == "":
		return vz, "could not read the macOS version (kling-hv needs " + macOSMinHV + ")"
	case !versionAlMenos(e.macOS, macOSMinHV):
		return vz, fmt.Sprintf("macOS %s is older than %s, which kling-hv needs", e.macOS, macOSMinHV)
	case !e.existe(hv):
		return vz, "kling-hv not found next to kling nor in PATH"
	case !e.firmado(hv):
		return vz, hv + " is not signed with " + entitlementDe(machine.BackendHV)
	}
	// Lo ya congelado con kling-vz solo lo descongela kling-vz: pasar solo a
	// hv dejaría esas máquinas y dorados sin poder usarse tras un upgrade.
	if e.volcadosVZ > 0 {
		return vz, fmt.Sprintf("%d frozen machines or goldens were made with kling-vz; set daemon.vmm: hv to switch (they must be thawed and removed, or rebuilt)", e.volcadosVZ)
	}
	if out, err := e.sonda(hv); err != nil {
		if out = strings.TrimSpace(firstLine(out)); out != "" {
			return vz, fmt.Sprintf("%s --probe failed: %s", hv, out)
		}
		return vz, fmt.Sprintf("%s --probe failed: %v", hv, err)
	}
	return machine.BackendHV, fmt.Sprintf("macOS %s, %s signed and probed", e.macOS, hv)
}

// entornoAutoReal es el de este Mac, con los datos en root.
func entornoAutoReal(env, root string) entornoAuto {
	return entornoAuto{
		macOS:      versionMacOS(),
		volcadosVZ: machine.VolcadosDeVMM(root, "kling-vz"),
		env:        env,
		hv:         machine.ResolverVMM(machine.BackendHV, "", ""),
		existe: func(p string) bool {
			_, err := exec.LookPath(p)
			return err == nil
		},
		firmado: func(p string) bool {
			if lp, err := exec.LookPath(p); err == nil {
				p = lp
			}
			return tieneEntitlement(p, entitlementDe(machine.BackendHV))
		},
		sonda: sondaHV,
	}
}

// sondaHV ejecuta `kling-hv --probe`. KLING_HV_PROBE_FAIL=1 la da por fallida
// sin ejecutar nada: para probar la caída a vz en un Mac donde hv funciona.
func sondaHV(p string) (string, error) {
	if os.Getenv("KLING_HV_PROBE_FAIL") == "1" {
		return "forced by KLING_HV_PROBE_FAIL=1", fmt.Errorf("forced")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, p, "--probe").CombinedOutput()
	return string(out), err
}

// resolverBackend es daemonBackend con auto ya resuelto: el backend concreto
// y por qué ("" en Linux, donde no hay elección).
func resolverBackend(ajuste, env, goos, goarch string, entorno func(string) entornoAuto) (string, string, error) {
	b, err := daemonBackend(ajuste, env, goos, goarch)
	if err != nil {
		return "", "", err
	}
	switch {
	case b == config.VMMAuto:
		backend, motivo := elegirAuto(entorno(env))
		return backend, "auto: " + motivo, nil
	case goos != "darwin":
		return b, "", nil
	case env == b:
		return b, "KLING_VMM=" + env, nil
	default:
		return b, "daemon.vmm " + b, nil
	}
}
