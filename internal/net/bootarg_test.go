package net

import (
	"strings"
	"testing"
)

// Por defecto el módulo IPv6 del invitado no carga; con la pila pedida por la
// imagen, carga sin direcciones (ipv6.disable_ipv6=1) y nunca van las dos.
func TestBootArgIPv6(t *testing.T) {
	off := BootArg(false, false)
	if !strings.Contains(off, " ipv6.disable=1") || strings.Contains(off, "disable_ipv6") {
		t.Errorf("BootArg(false) = %q, quiero ipv6.disable=1", off)
	}
	pila := BootArg(true, false)
	if !strings.Contains(pila, " ipv6.disable_ipv6=1") || strings.Contains(pila, "ipv6.disable=1") {
		t.Errorf("BootArg(true) = %q, quiero ipv6.disable_ipv6=1 y no ipv6.disable=1", pila)
	}
	for _, s := range []string{off, pila} {
		if !strings.HasPrefix(s, "ip="+GuestIP+"::") {
			t.Errorf("la red del kernel cambió: %q", s)
		}
	}
}

// Con el agente que anuncia "net" la red va en kling.net= y sin ip=; sin él,
// la línea de siempre byte a byte.
func TestBootArgRedAgente(t *testing.T) {
	for _, tc := range []struct {
		v6, agente bool
		want       string
	}{
		{false, false, "ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off ipv6.disable=1"},
		{true, false, "ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off ipv6.disable_ipv6=1"},
		{false, true, "kling.net=172.16.0.2/30,172.16.0.1 ipv6.disable=1"},
		{true, true, "kling.net=172.16.0.2/30,172.16.0.1 ipv6.disable_ipv6=1"},
	} {
		if got := BootArg(tc.v6, tc.agente); got != tc.want {
			t.Errorf("BootArg(%v,%v) = %q, quiero %q", tc.v6, tc.agente, got, tc.want)
		}
	}
}
