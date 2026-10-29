package net

import (
	"strings"
	"testing"
)

// dockerConKindling es `iptables -S` de un host con Docker 28 y el daemon de
// kindling ya arrancado: lo que vio el runner de CI.
const dockerConKindling = `-P INPUT ACCEPT
-P FORWARD DROP
-P OUTPUT ACCEPT
-N DOCKER
-N DOCKER-CT
-N DOCKER-FORWARD
-N DOCKER-USER
-A INPUT -s 172.30.0.0/16 -i vh-+ -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A FORWARD -j DOCKER-USER
-A FORWARD -j DOCKER-FORWARD
-A FORWARD -i vh-+ -o vh-+ -j DROP
-A FORWARD ! -s 172.30.0.0/16 -i vh-+ -j DROP
-A FORWARD -i vh-+ -m conntrack --ctstate INVALID -j DROP
-A FORWARD -s 172.30.0.0/16 -d 10.0.0.0/8 -j DROP
-A DOCKER-CT -o docker0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A DOCKER-FORWARD -j DOCKER-CT
-A DOCKER-FORWARD -i docker0 -j ACCEPT
`

func TestForwardSinSalida(t *testing.T) {
	ida := "-A FORWARD -s 172.30.0.0/16 -i vh-+ -j ACCEPT\n"
	vuelta := "-A FORWARD -d 172.30.0.0/16 -o vh-+ -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n"
	casos := []struct {
		nombre, reglas string
		mal            bool
	}{
		{"docker con kindling", dockerConKindling, true},
		{"política ACCEPT", strings.Replace(dockerConKindling, "-P FORWARD DROP", "-P FORWARD ACCEPT", 1), false},
		{"sin docker", "-P INPUT ACCEPT\n-P FORWARD ACCEPT\n-A FORWARD -i vh-+ -o vh-+ -j DROP\n", false},
		{"con el arreglo", dockerConKindling + ida + vuelta, false},
		{"solo la ida", dockerConKindling + ida, true},
		{"solo la vuelta", dockerConKindling + vuelta, true},
		{"ida y un ESTABLISHED general", dockerConKindling + ida +
			"-A FORWARD -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n", false},
		{"ACCEPT sin filtro", dockerConKindling + "-A FORWARD -j ACCEPT\n", false},
		{"arreglo en DOCKER-USER", dockerConKindling + strings.ReplaceAll(ida+vuelta, "-A FORWARD", "-A DOCKER-USER"), false},
		{"ACCEPT de otra interfaz", dockerConKindling + "-A FORWARD -i eth0 -j ACCEPT\n" + vuelta, true},
		{"negación no es nuestra ida", dockerConKindling + "-A FORWARD ! -i vh-+ -j ACCEPT\n" + vuelta, true},
		{"sin leer (vacío)", "", false},
	}
	for _, c := range casos {
		if got := ForwardSinSalida(c.reglas); got != c.mal {
			t.Errorf("%s: ForwardSinSalida = %v, want %v", c.nombre, got, c.mal)
		}
	}
}

func TestAvisoForward(t *testing.T) {
	if a := AvisoForward(strings.Replace(dockerConKindling, "DROP\n-P OUTPUT", "ACCEPT\n-P OUTPUT", 1)); a != "" {
		t.Errorf("aviso with policy ACCEPT: %q", a)
	}
	a := AvisoForward(dockerConKindling)
	for _, want := range []string{"FORWARD policy is DROP", "-A FORWARD -i vh-+ -s " + HostSubnet + " -j ACCEPT", "--ctstate ESTABLISHED,RELATED"} {
		if !strings.Contains(a, want) {
			t.Errorf("aviso without %q:\n%s", want, a)
		}
	}
	// El arreglo que se propone tiene que quitar el aviso.
	var arreglo []string
	for _, l := range ArregloForward() {
		if strings.HasPrefix(l, "sudo iptables ") {
			arreglo = append(arreglo, strings.TrimPrefix(l, "sudo iptables "))
		}
	}
	if len(arreglo) != 2 {
		t.Fatalf("ArregloForward has %d iptables commands, want 2", len(arreglo))
	}
	if AvisoForward(dockerConKindling+strings.Join(arreglo, "\n")+"\n") != "" {
		t.Error("the proposed fix does not clear the warning")
	}
	// Ninguna orden del arreglo inserta delante (-I): quedaría por delante de
	// las barreras de kindling y las saltaría.
	for _, r := range arreglo {
		if !strings.HasPrefix(r, "-A FORWARD ") {
			t.Errorf("fix rule %q is not appended to FORWARD", r)
		}
	}
}
