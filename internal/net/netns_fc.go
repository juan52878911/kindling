//go:build !darwin

package net

// Lo que monta y desmonta la red de una microVM en el HOST: namespaces, veth,
// TAP e iptables. Solo existe fuera de macOS; allí la red es de espacio de
// usuario dentro de kling-vz (netns_vz.go).

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// nsIP es la IP del lado namespace del enlace: por ella alcanza el host a la
// microVM.
func nsIP(third, fourth int) string {
	return fmt.Sprintf("%s.%d.%d", hostPrefix, third, fourth+2)
}

// Setup crea el namespace, el veth, el TAP y las reglas de salida.
//
// owner es el UID que podrá abrir el TAP. Firecracker corre sin privilegios, y
// abrir un tap ajeno exige CAP_NET_ADMIN: creándolo ya a su nombre, no le hace
// falta ninguna capacidad.
// domains solo se usa cuando egress es EgressAllowlist: son los dominios a los
// que se deja salir. En los demás modos se ignora.
func (n *Net) Setup(egress Egress, domains []string, owner int) error {
	n.Teardown() // restos de una ejecución anterior

	if err := run("ip", "netns", "add", n.NS); err != nil {
		return err
	}
	if err := run("ip", "link", "add", n.HostIf, "type", "veth", "peer", "name", n.NSIf); err != nil {
		return err
	}
	if err := run("ip", "link", "set", n.NSIf, "netns", n.NS); err != nil {
		return err
	}

	// lado host
	if err := run("ip", "addr", "add", n.HostIP+"/30", "dev", n.HostIf); err != nil {
		return err
	}
	if err := run("ip", "link", "set", n.HostIf, "up"); err != nil {
		return err
	}

	ns := func(args ...string) error {
		return run(append([]string{"ip", "netns", "exec", n.NS}, args...)...)
	}

	// lado namespace
	if err := ns("ip", "addr", "add", n.NSIP+"/30", "dev", n.NSIf); err != nil {
		return err
	}
	if err := ns("ip", "link", "set", n.NSIf, "up"); err != nil {
		return err
	}
	if err := ns("ip", "link", "set", "lo", "up"); err != nil {
		return err
	}

	// TAP con nombre e IP fijos: idénticos en todos los namespaces
	tap := []string{"ip", "tuntap", "add", TapName, "mode", "tap"}
	if owner > 0 {
		tap = append(tap, "user", fmt.Sprintf("%d", owner))
	}
	if err := ns(tap...); err != nil {
		return err
	}
	if err := ns("ip", "addr", "add", GuestGW+"/30", "dev", TapName); err != nil {
		return err
	}
	if err := ns("ip", "link", "set", TapName, "address", TapMAC, "up"); err != nil {
		return err
	}
	if err := ns("ip", "route", "add", "default", "via", n.HostIP); err != nil {
		return err
	}
	if err := ns("sh", "-c", "echo 1 > /proc/sys/net/ipv4/ip_forward"); err != nil {
		return err
	}

	// Entrada: lo que llegue a la IP del namespace va a la microVM. Así el host
	// alcanza cada máquina por una IP distinta aunque todas usen la misma dentro.
	if err := ns("iptables", "-t", "nat", "-A", "PREROUTING",
		"-d", n.NSIP, "-j", "DNAT", "--to-destination", GuestIP); err != nil {
		return err
	}
	return n.applyEgress(egress, domains)
}

// Teardown deshace todo. El veth del host desaparece al borrar el namespace,
// pero lo intentamos por si el namespace ya no está.
func (n *Net) Teardown() {
	// Primero el resolver dinámico (goroutine del daemon), si este netns tenía uno
	// en modo allowlist. Es no-op para none/internet.
	stopResolver(n.NS)
	quiet("ip", "netns", "del", n.NS)
	quiet("ip", "link", "del", n.HostIf)
}

// Exists dice si el namespace y el veth del lado del host siguen ahí: lo que
// deja montado un Setup. Solo mira el sistema de ficheros, sin procesos.
func (n *Net) Exists() bool {
	if _, err := os.Stat("/var/run/netns/" + n.NS); err != nil {
		return false
	}
	_, err := os.Stat("/sys/class/net/" + n.HostIf)
	return err == nil
}

// StartAllowlistResolver revive el resolver dinámico de una microVM en modo
// allowlist tras un reinicio del daemon, SIN re-montar la red: el netns y sus
// reglas sobreviven al daemon (la microVM también), pero el resolver es una
// goroutine del daemon y muere con él. Sin esto, el DNAT del invitado apuntaría a
// un puerto sin nadie escuchando y se quedaría sin DNS hasta el próximo Setup. Es
// idempotente. Solo tiene sentido llamarlo para máquinas con egress allowlist.
func (n *Net) StartAllowlistResolver(domains []string) error {
	return startDNSResolver(n, domains)
}

// Wrap antepone lo necesario para ejecutar un comando dentro del namespace.
func (n *Net) Wrap(cmd string, args ...string) []string {
	return append([]string{"ip", "netns", "exec", n.NS, cmd}, args...)
}

// ListNamespaces devuelve los namespaces creados por kindling.
func ListNamespaces() []string {
	out, err := exec.Command("ip", "netns", "list").Output()
	if err != nil {
		return nil
	}
	var res []string
	for _, line := range strings.Split(string(out), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if strings.HasPrefix(name, "kl-") {
			res = append(res, name)
		}
	}
	return res
}

// TeardownNamespace borra un namespace y su veth por nombre.
func TeardownNamespace(ns string) {
	stopResolver(ns)
	quiet("ip", "netns", "del", ns)
	quiet("ip", "link", "del", "vh-"+strings.TrimPrefix(ns, "kl-"))
}

// Available indica si el host tiene lo necesario para montar redes.
func Available() error {
	for _, c := range []string{"ip", "iptables"} {
		if _, err := exec.LookPath(c); err != nil {
			return fmt.Errorf("missing %s", c)
		}
	}
	return nil
}
