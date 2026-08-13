// Package net monta la red de cada microVM.
//
// EL PROBLEMA: un snapshot graba el nombre del dispositivo TAP del host. Si N
// instancias restauran del mismo snapshot dorado, las N piden el mismo TAP y
// chocan. Tampoco vale reasignarlo: Firecracker no permite parchear
// host_dev_name en una interfaz de red.
//
// LA SOLUCIÓN: un namespace de red por microVM. Dentro de cada uno el TAP se
// llama SIEMPRE tap0 y el invitado tiene SIEMPRE la misma IP, así que el
// snapshot es idéntico para todas. Toda la diferenciación ocurre en el host, al
// otro lado de un veth. Es el mismo enfoque que usa AWS Lambda.
//
//	       host                    │  netns kl-<id>        │  microVM
//	vh-<id> 172.30.a.b/30  ◄─veth─►│ vg-<id> 172.30.a.b+1  │
//	                               │ tap0    172.16.0.1/30 ├─ eth0 172.16.0.2
package net

import (
	"fmt"
	"os/exec"
	"strings"
)

const (
	// El invitado ve siempre esta configuración, sea cual sea la instancia. Es
	// lo que hace que un snapshot dorado sirva para todas.
	GuestIP  = "172.16.0.2"
	GuestGW  = "172.16.0.1"
	GuestNM  = "255.255.255.252"
	GuestMAC = "06:00:AC:10:00:02"
	TapName  = "tap0"

	// Rango del host para los enlaces punto a punto con cada namespace.
	hostPrefix = "172.30"
)

// BootArg devuelve la configuración de red que el kernel del invitado aplica
// solo, sin necesitar herramientas dentro de la imagen.
func BootArg() string {
	return fmt.Sprintf("ip=%s::%s:%s::eth0:off", GuestIP, GuestGW, GuestNM)
}

// Net es la red de una microVM concreta.
type Net struct {
	NS     string // namespace
	HostIf string // veth del lado host
	NSIf   string // veth del lado namespace
	HostIP string // IP del host en el enlace
	NSIP   string // IP del namespace en el enlace; por aquí se alcanza la microVM
	Index  int
}

func run(args ...string) error {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// quiet ejecuta ignorando el error; para limpiezas donde "no existe" es válido.
func quiet(args ...string) { _ = exec.Command(args[0], args[1:]...).Run() }

// exec_ok indica si un comando termina bien; se usa para comprobar reglas ya puestas.
func exec_ok(args []string) bool {
	return exec.Command(args[0], args[1:]...).Run() == nil
}

// Plan calcula el direccionamiento de una microVM a partir de su índice.
// Cada máquina consume una /30: 16384 máquinas en 172.30.0.0/16.
func Plan(index int, id string) *Net {
	third := index / 64
	fourth := (index % 64) * 4
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	return &Net{
		NS:     "kl-" + short,
		HostIf: "vh-" + short,
		NSIf:   "vg-" + short,
		HostIP: fmt.Sprintf("%s.%d.%d", hostPrefix, third, fourth+1),
		NSIP:   fmt.Sprintf("%s.%d.%d", hostPrefix, third, fourth+2),
		Index:  index,
	}
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
	if err := ns("ip", "link", "set", TapName, "up"); err != nil {
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
	quiet("ip", "netns", "del", n.NS)
	quiet("ip", "link", "del", n.HostIf)
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
	quiet("ip", "netns", "del", ns)
	quiet("ip", "link", "del", "vh-"+strings.TrimPrefix(ns, "kl-"))
}

// Available indica si el host tiene lo necesario para montar redes.
func Available() error {
	for _, c := range []string{"ip", "iptables"} {
		if _, err := exec.LookPath(c); err != nil {
			return fmt.Errorf("falta %s", c)
		}
	}
	return nil
}
