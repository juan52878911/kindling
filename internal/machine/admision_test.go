package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// El disco lleno se rechaza con un código que el planificador NO interpreta como
// falta de memoria: si lo hiciera, congelaría instancias, y congelar escribe en
// disco lo que falta.
func TestDiscoLlenoNoSeConfundeConMemoria(t *testing.T) {
	m := newTestManager(t)
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "999999999") // más de lo que hay en cualquier sitio
	err := m.checkDisk()
	if err == nil {
		t.Fatal("con un mínimo imposible tenía que rechazar")
	}
	if !api.IsDiskFull(err) {
		t.Fatalf("no se reconoce como disco lleno: %v", err)
	}
	if api.IsInsufficientMemory(err) {
		t.Fatal("se confunde con falta de memoria: el planificador congelaría para hacer sitio")
	}

	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	if err := m.checkDisk(); err != nil {
		t.Fatalf("con la comprobación apagada: %v", err)
	}
}

// La presión de memoria se puede apagar, y sin PSI (macOS, kernels sin él) no
// bloquea nada: una comprobación de cordura no puede volverse un requisito.
func TestPresionDeMemoria(t *testing.T) {
	// Un /proc/pressure/memory de mentira: con él se puede fingir un host bajo
	// presión en cualquier sitio, también en macOS.
	psi := filepath.Join(t.TempDir(), "memory")
	viejo := psiMemoria
	psiMemoria = psi
	t.Cleanup(func() { psiMemoria = viejo })
	escribir := func(avg10 string) {
		t.Helper()
		txt := "some avg10=" + avg10 + " avg60=1.00 avg300=0.50 total=12345\n" +
			"full avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
		if err := os.WriteFile(psi, []byte(txt), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Sin PSI (el fichero no existe) no bloquea nada.
	t.Setenv("KLING_MAX_MEM_PRESSURE", "")
	if p := memPressure(); p != -1 {
		t.Fatalf("sin PSI memPressure = %v, quería -1", p)
	}
	if err := checkPressure(); err != nil {
		t.Fatalf("sin PSI no debe bloquear: %v", err)
	}

	// Bajo el tope por defecto se admite; por encima, 507.
	escribir("19.50")
	if p := memPressure(); p != 19.5 {
		t.Fatalf("memPressure = %v, quería 19.5 (la línea some, no la full)", p)
	}
	if err := checkPressure(); err != nil {
		t.Fatalf("19,5 %% con tope %v: %v", defaultMaxMemPressure, err)
	}
	escribir("42.00")
	err := checkPressure()
	if err == nil {
		t.Fatal("con 42 % de presión y tope 20 tenía que rechazar")
	}
	if !api.IsInsufficientMemory(err) || !strings.Contains(err.Error(), "KLING_MAX_MEM_PRESSURE") {
		t.Fatalf("el rechazo tiene que ser 507 y decir cómo subir el tope: %v", err)
	}

	// El tope se sube, y "0" lo apaga.
	t.Setenv("KLING_MAX_MEM_PRESSURE", "50")
	if err := checkPressure(); err != nil {
		t.Fatalf("42 %% con tope 50: %v", err)
	}
	t.Setenv("KLING_MAX_MEM_PRESSURE", "0")
	if err := checkPressure(); err != nil {
		t.Fatalf("apagada: %v", err)
	}
}

// La puerta de arranque es de 2 en cualquier host virtual, incluido el Mac con
// Virtualization.framework, cuyo cpuinfo arm64 no dice nada.
func TestEsVirtualSegunDMI(t *testing.T) {
	for dmi, want := range map[string]bool{
		"Apple Inc.Apple Virtualization Generic Platform": true,
		"QEMUStandard PC (Q35 + ICH9, 2009)":              true,
		"Microsoft CorporationVirtual Machine":            true,
		"Dell Inc.PowerEdge R650":                         false,
		"":                                                false,
	} {
		if got := esVirtualSegunDMI(dmi); got != want {
			t.Errorf("%q: %v, quería %v", dmi, got, want)
		}
	}
}
