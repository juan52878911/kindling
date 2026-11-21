package net

import (
	"runtime"
	"errors"
	"slices"
	"strings"
	"testing"
)

// hostDocker es un iptables mínimo con FORWARD en DROP y las cadenas de
// Docker: solo entiende lo que usa montarCadenaForward.
type hostDocker struct {
	forward []string
	cadena  []string
	existe  bool
}

func (h *hostDocker) ejecutar(a ...string) error {
	switch {
	case a[1] == "-N":
		h.existe = true
	case a[1] == "-F":
		h.cadena = nil
	case a[1] == "-A" && a[2] == cadenaForward:
		h.cadena = append(h.cadena, strings.Join(a[3:], " "))
	case a[1] == "-A" && a[2] == "FORWARD":
		h.forward = append(h.forward, strings.Join(a[3:], " "))
	case a[1] == "-I" && a[2] == "FORWARD":
		pos := int(a[3][0] - '0')
		h.forward = slices.Insert(h.forward, min(pos-1, len(h.forward)), strings.Join(a[4:], " "))
	case a[1] == "-D" && a[2] == "FORWARD":
		if i := slices.Index(h.forward, strings.Join(a[3:], " ")); i >= 0 {
			h.forward = slices.Delete(h.forward, i, i+1)
		}
	}
	return nil
}

func (h *hostDocker) estado(a []string) (bool, error) {
	if a[1] != "-C" {
		return false, nil
	}
	lista := h.forward
	if a[2] == cadenaForward {
		lista = h.cadena
	}
	return slices.Contains(lista, strings.Join(a[3:], " ")), nil
}

func (h *hostDocker) listar(tabla, cadena string) ([]string, error) {
	if cadena == cadenaForward {
		if !h.existe {
			return nil, errors.New("falso")
		}
		return append([]string{"-N " + cadena}, prefijar("-A "+cadena+" ", h.cadena)...), nil
	}
	return append([]string{"-P FORWARD DROP"}, prefijar("-A FORWARD ", h.forward)...), nil
}

func prefijar(p string, l []string) []string {
	var out []string
	for _, x := range l {
		out = append(out, p+x)
	}
	return out
}

func (h *hostDocker) instalar(t *testing.T) {
	t.Helper()
	oldE, oldC, oldL := ejecutar, comprobarEstado, listarTabla
	t.Cleanup(func() { ejecutar, comprobarEstado, listarTabla = oldE, oldC, oldL })
	ejecutar, comprobarEstado, listarTabla = h.ejecutar, h.estado, h.listar
}

// Con KLING_FORWARD_CHAIN=1 existe KINDLING-FWD y FORWARD salta a ella en la
// posición 1 para vh-+, delante de lo que Docker tuviera; sin ella, nada.
func TestCadenaForwardPorEntorno(t *testing.T) {
	h := &hostDocker{forward: []string{"-j DOCKER-USER", "-j DOCKER-FORWARD"}}
	h.instalar(t)
	t.Setenv("KLING_FORWARD_CHAIN", "")
	if CadenaForwardActiva() || centinelaCadenaForward() != nil {
		t.Fatal("la cadena está activa por defecto")
	}
	t.Setenv("KLING_FORWARD_CHAIN", "1")
	for i := 0; i < 2; i++ { // idempotente
		if err := montarCadenaForward(); err != nil {
			t.Fatal(err)
		}
	}
	if !h.existe {
		t.Fatal("no creó la cadena")
	}
	if h.forward[0] != "-i vh-+ -j KINDLING-FWD" || h.forward[1] != "-o vh-+ -j KINDLING-FWD" ||
		h.forward[2] != "-j DOCKER-USER" || len(h.forward) != 4 {
		t.Fatalf("FORWARD = %q", h.forward)
	}
	ult := h.cadena[len(h.cadena)-2:]
	if ult[0] != "-i vh-+ -s 172.30.0.0/16 -j ACCEPT" || !strings.Contains(ult[1], "-o vh-+ -d 172.30.0.0/16") ||
		!strings.Contains(ult[1], "ESTABLISHED,RELATED") {
		t.Errorf("final de la cadena: %q", ult)
	}
	if len(h.cadena) != len(ReglasCadenaForward()) || !slices.ContainsFunc(h.cadena, func(r string) bool {
		return strings.Contains(r, "-i vh-+ -o vh-+ -j DROP")
	}) {
		t.Errorf("la cadena no lleva las DROP de HostEgressRules: %q", h.cadena)
	}
	// Un reload que vació la cadena y bajó el salto: se rehace en su sitio.
	h.cadena, h.forward = nil, append([]string{"-j DOCKER-USER"}, h.forward[2:]...)
	if err := montarCadenaForward(); err != nil {
		t.Fatal(err)
	}
	if h.forward[0] != "-i vh-+ -j KINDLING-FWD" || len(h.cadena) != len(ReglasCadenaForward()) {
		t.Errorf("no se repuso: %q / %d reglas", h.forward, len(h.cadena))
	}
}

// Con la cadena, un host con FORWARD en DROP y Docker ya no se avisa: las
// ACCEPT de la cadena dan salida y vuelta.
func TestForwardSinSalidaConLaCadena(t *testing.T) {
	h := &hostDocker{forward: []string{"-j DOCKER-USER"}}
	h.instalar(t)
	t.Setenv("KLING_FORWARD_CHAIN", "1")
	if err := montarCadenaForward(); err != nil {
		t.Fatal(err)
	}
	reglas := "-P FORWARD DROP\n-N KINDLING-FWD\n"
	for _, l := range h.forward {
		reglas += "-A FORWARD " + l + "\n"
	}
	for _, l := range h.cadena {
		reglas += "-A KINDLING-FWD " + l + "\n"
	}
	if ForwardSinSalida(reglas) {
		t.Errorf("avisa pese a la cadena:\n%s", reglas)
	}
	if !ForwardSinSalida("-P FORWARD DROP\n-A FORWARD -j DOCKER-USER\n") {
		t.Error("sin la cadena debería avisar")
	}
	if !strings.Contains(AvisoForward("-P FORWARD DROP\n"), "KLING_FORWARD_CHAIN=1") {
		t.Error("el aviso no menciona la cadena")
	}
}

func TestFaltaParaAllowlist(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ipset y la allowlist solo existen en Linux")
	}
	old := hayIpset
	t.Cleanup(func() { hayIpset = old })
	hayIpset = func() error { return errors.New("falso") }
	if f := FaltaParaAllowlist(); !strings.Contains(f, "ipset") {
		t.Errorf("sin ipset: %q", f)
	}
	hayIpset = func() error { return nil }
	if f := FaltaParaAllowlist(); f != "" {
		t.Errorf("con ipset: %q", f)
	}
}
