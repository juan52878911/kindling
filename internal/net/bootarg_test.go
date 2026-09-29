package net

import (
	"strings"
	"testing"
)

// Por defecto el módulo IPv6 del invitado no carga; con la pila pedida por la
// imagen, carga sin direcciones (ipv6.disable_ipv6=1) y nunca van las dos.
func TestBootArgIPv6(t *testing.T) {
	off := BootArg(false)
	if !strings.Contains(off, " ipv6.disable=1") || strings.Contains(off, "disable_ipv6") {
		t.Errorf("BootArg(false) = %q, quiero ipv6.disable=1", off)
	}
	pila := BootArg(true)
	if !strings.Contains(pila, " ipv6.disable_ipv6=1") || strings.Contains(pila, "ipv6.disable=1") {
		t.Errorf("BootArg(true) = %q, quiero ipv6.disable_ipv6=1 y no ipv6.disable=1", pila)
	}
	for _, s := range []string{off, pila} {
		if !strings.HasPrefix(s, "ip="+GuestIP+"::") {
			t.Errorf("la red del kernel cambió: %q", s)
		}
	}
}
