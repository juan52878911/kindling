//go:build !darwin

package net

// Lo que monta y desmonta la red de una microVM en el HOST: namespaces, veth,
// TAP e iptables. Solo existe fuera de macOS; allí la red es de espacio de
// usuario dentro de kling-vz (netns_vz.go).

import (
	"fmt"
	stdnet "net"
	"os"
	"os/exec"
	"strings"
)

// redEnHost: las /30 de los enlaces viven en el host y pueden chocar con las
// de otro daemon (ver subredes.go).
const redEnHost = true

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
func (n *Net) Setup(egress Egress, domains []string, owner int) (err error) {
	tr := nuevaTrazaSetup(n.NS)
	defer func() { tr.fin(err) }()
	// Antes de abrir la salida a otra máquina, las reglas INPUT del host:
	// si un reload del cortafuegos las borró, se reponen; si no se puede,
	// la máquina no arranca con el host abierto.
	if err := asegurarHost(); err != nil {
		return fmt.Errorf("host INPUT rules: %w", err)
	}
	tr.fase("host_check")
	// Una ranura (ranuras_fc.go) llega con la base montada: solo se añade lo
	// propio de la petición. Se consume aquí: un segundo Setup del mismo *Net
	// ya no la encuentra y monta todo, como cualquier red sin ranura.
	if n.baseLista {
		n.baseLista = false
		return n.aplicarPropio(tr, egress, domains, true)
	}
	// Restos de una ejecución anterior se derriban; lo de otro daemon, no. Si
	// el veth existe con otra IP, no es un resto nuestro: es la red de otra
	// máquina (otro daemon con un id que empieza igual), y Teardown la dejaría
	// corriendo e incomunicada.
	if existeRed(n) {
		if ips := ipsVeth(n.HostIf); len(ips) > 0 && !contiene(ips, n.HostIP) {
			return fmt.Errorf("%w: %s has %s, planned %s", ErrRedAjena, n.HostIf, strings.Join(ips, ","), n.HostIP)
		}
	}
	n.Teardown()
	tr.fase("teardown")
	// Sin ranura, el montaje completo hace cola: a lo sumo 4 a la vez.
	montajesCompletos <- struct{}{}
	err = n.montarBase(tr, owner, false)
	<-montajesCompletos
	if err != nil {
		return err
	}
	return n.aplicarPropio(tr, egress, domains, false)
}

// nsEn ejecuta una orden dentro del netns de n. `ip` entra solo (`ip -n NS`,
// un proceso); lo demás (iptables, sysctl) lo lanza ordenEnNetns con setns, sin el
// `ip netns exec` de antes.
func (n *Net) nsEn(args ...string) error {
	if args[0] == "ip" {
		return ejecutar(append([]string{"ip", "-n", n.NS}, args[1:]...)...)
	}
	return ordenEnNetns(n.NS, args...)
}

// reglaDNAT: lo que llegue a la IP del namespace va a la microVM. Así el host
// alcanza cada máquina por una IP distinta aunque todas usen la misma dentro.
func (n *Net) reglaDNAT() []string {
	return []string{"-t", "nat", "-A", "PREROUTING", "-d", n.NSIP, "-j", "DNAT", "--to-destination", GuestIP}
}

// montarBase monta lo que no depende de la petición: netns, veth, tap, sysctl,
// barrera IPv6 y, si conDNAT, la entrada DNAT. Sin ninguna regla de salida:
// es lo más restrictivo, y aplicarPropio solo añade. Sin conDNAT, el DNAT va
// en el iptables-restore de aplicarPropio (un proceso menos en Setup).
func (n *Net) montarBase(tr *trazaSetup, owner int, conDNAT bool) error {
	// Los 11 procesos de ip del enlace son 3: el netns y un `ip -batch`
	// por cada lado. `ip netns add` va aparte: en batch, según la versión de
	// iproute2, el resto de líneas acabaría dentro del netns nuevo.
	if err := ejecutar("ip", "netns", "add", n.NS); err != nil {
		return err
	}
	tr.fase("netns")
	// El extremo del namespace nace ya dentro de él. Crearlo fuera y moverlo
	// después (`ip link set ... netns`) espera un periodo de gracia de RCU en
	// el kernel: 14 ms medidos en un i7-8700T, un tercio de todo el montaje.
	if err := ipBatch("",
		"link add "+n.HostIf+" type veth peer name "+n.NSIf+" netns "+n.NS,
		"addr add "+n.HostIP+"/30 dev "+n.HostIf,
		"link set "+n.HostIf+" up",
	); err != nil {
		return err
	}
	tr.fase("veth")
	// Lado namespace, con el TAP de nombre e IP fijos: idénticos en todos los
	// namespaces.
	tap := "tuntap add " + TapName + " mode tap"
	if owner > 0 {
		tap += fmt.Sprintf(" user %d", owner)
	}
	if err := ipBatch(n.NS,
		"addr add "+n.NSIP+"/30 dev "+n.NSIf,
		"link set "+n.NSIf+" up",
		"link set lo up",
		tap,
		"addr add "+GuestGW+"/30 dev "+TapName,
		"link set "+TapName+" address "+TapMAC+" up",
		"route add default via "+n.HostIP,
	); err != nil {
		return err
	}
	tr.fase("tap")

	// Los sysctl del netns, en un solo proceso (aplicarSysctls): el reenvío,
	// el endurecimiento de sysctlsNetns y la capa 1 de la barrera IPv6.
	//
	// El ARP del tap0 reintenta cada 50 ms en vez de cada 1 s. El daemon sondea
	// al agente mientras el invitado aún arranca: la primera resolución queda
	// incompleta y, con el valor por defecto, el kernel no la reintenta hasta
	// 1 s después por muchos connect nuevos que se lancen. Junto con los
	// intentos cortos de waitPort (el SYN perdido tampoco se retransmite antes
	// de 1 s), un sandbox en frío pasó de 1,07 s a 0,44 s en el lab; cada uno
	// por separado no movía nada.
	//
	// NO una entrada ARP permanente para el invitado, aunque su MAC sea fija:
	// los dorados congelados cuando la MAC del tap0 aún era aleatoria guardan
	// la vieja en su caché, y lo que se la corrige es justo la petición ARP del
	// host (despertar.md, palanca 8). Con la entrada permanente el host no
	// pregunta y esas restauraciones se quedaban sin red.
	if err := aplicarSysctls(n.nsEn, n.NS, n.NSIf); err != nil {
		return err
	}
	tr.fase("sysctl")

	// Capa 2 de la barrera IPv6 (firewall.go): va en la base y no dentro de
	// applyEgress. Esa rama devuelve sin pasar por applyEgress para
	// none/internet cuando hay iptables-restore, así que si esta llamada
	// viviera solo dentro de applyEgress se saltaría en el camino más común.
	// Se aplica igual en los tres modos, por eso no depende de egress.
	if err := n.applyIPv6Barrier(n.nsEn); err != nil {
		return err
	}
	tr.fase("ip6")
	if conDNAT {
		if err := n.restoreRules([][]string{n.reglaDNAT()}); err != nil {
			return err
		}
		tr.fase("dnat")
	}
	return nil
}

// aplicarPropio añade lo que depende de la petición sobre la base montada:
// las reglas de salida (en un solo iptables-restore), el resolver de none o
// el de allowlist, y el registro de nft. dnatHecho dice que montarBase ya
// puso el DNAT.
func (n *Net) aplicarPropio(tr *trazaSetup, egress Egress, domains []string, dnatHecho bool) error {
	ns := n.nsEn
	var dnat [][]string
	if !dnatHecho {
		dnat = [][]string{n.reglaDNAT()}
	}
	// Los tres modos van en UN iptables-restore con todas sus reglas: cada
	// iptables por separado es un proceso que relee el conjunto de reglas (~3
	// ms cada uno). allowlist además crea su ipset y arranca su resolver.
	if hayRestore() {
		if egress == EgressAllowlist {
			if err := n.applyAllowlist(ns, domains, dnat, true); err != nil {
				return err
			}
			return n.vecinoInvitado()
		}
		reglas := append(dnat, n.egressRules(egress)...)
		if egress == EgressNone {
			// El DNS del invitado pasa por nuestro resolver (NXDOMAIN a
			// todo): así sabemos QUÉ nombre quiso y el aviso de red
			// cortada puede decirlo (bloqueos.go).
			reglas = append(reglas, n.reglasGrafo(EgressNone, false)...)
		}
		if err := n.restoreRules(reglas); err != nil {
			return err
		}
		tr.fase("restore")
		if err := n.arrancarRegistroNone(egress); err != nil {
			return err
		}
		return n.vecinoInvitado()
	}
	if !dnatHecho {
		if err := ns(append([]string{"iptables"}, n.reglaDNAT()...)...); err != nil {
			return err
		}
	}
	if err := n.applyEgress(egress, domains); err != nil {
		return err
	}
	if egress == EgressNone {
		if err := n.asegurarReglas(n.reglasGrafo(EgressNone, false)); err != nil {
			return err
		}
	}
	if err := n.arrancarRegistroNone(egress); err != nil {
		return err
	}
	return n.vecinoInvitado()
}

// vecinoInvitado deja, si Net.VecinoInvitado, la entrada de vecino
// GuestIP → GuestMAC en tap0 como STALE, no permanente: el host no tiene que
// preguntar por el invitado en el primer paquete, pero si el invitado cambia
// de MAC (un dorado viejo) la primera sonda la corrige. Ver el comentario de
// montarBase sobre por qué NO es permanente.
func (n *Net) vecinoInvitado() error {
	if !n.VecinoInvitado {
		return nil
	}
	return ejecutar("ip", "-n", n.NS, "neigh", "replace", GuestIP, "lladdr", GuestMAC, "dev", TapName, "nud", "stale")
}

// arrancarRegistroNone monta, en egress none, lo que ve los destinos cortados:
// el resolver que contesta NXDOMAIN y el set de nft de IP:puerto. No-op en los
// demás modos.
func (n *Net) arrancarRegistroNone(egress Egress) error {
	if egress != EgressNone {
		return nil
	}
	if err := startDNSResolverModo(n, nil, modoSoloGrafo); err != nil {
		return err
	}
	n.instalarRegistroIP()
	return nil
}

// Teardown deshace todo. El veth del host desaparece al borrar el namespace,
// pero lo intentamos por si el namespace ya no está.
func (n *Net) Teardown() {
	// Primero el resolver dinámico (goroutine del daemon), si este netns tenía uno
	// en modo allowlist. Es no-op para none/internet.
	stopResolver(n.NS)
	// Los enlaces antes que el proxy de credenciales: escriben en su auditor.
	stopGraph(n.NS)
	stopCredProxy(n.NS)
	cerrarIPSet(n.NS)
	borrarEnlace(n.NS, n.HostIf)
}

// borrarEnlace borra el netns y el veth del host, solo si están: Setup
// empieza siempre por Teardown, y en el caso corriente (una máquina nueva) no
// hay nada, así que eran dos procesos que fallaban. Mirarlo es un stat.
func borrarEnlace(ns, hostIf string) {
	if _, err := os.Stat("/var/run/netns/" + ns); err == nil {
		quiet("ip", "netns", "del", ns)
	}
	// Después del netns: borrarlo se lleva el veth, pero el kernel lo hace en
	// diferido y puede seguir ahí; `ip link del` lo quita en el acto.
	if _, err := os.Stat("/sys/class/net/" + hostIf); err == nil {
		quiet("ip", "link", "del", hostIf)
	}
}

// ipBatch ejecuta varias órdenes de ip en un solo proceso (`ip -batch -`),
// dentro del netns ns si no está vacío. Para en la primera que falla.
func ipBatchReal(ns string, ordenes ...string) error {
	args := []string{"ip"}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	args = append(args, "-batch", "-")
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(strings.Join(ordenes, "\n") + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s (%s): %v: %s", strings.Join(args, " "), strings.Join(ordenes, "; "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Sustituibles en tests: lanzar procesos de verdad pide root e `ip`.
var (
	asegurarHost = asegurarHostInput
	ipBatch      = ipBatchReal
)

// Sustituibles en tests: mirar la red de verdad pide /sys y un veth.
var (
	existeRed = (*Net).Exists
	ipsVeth   = func(ifname string) []string {
		ifc, err := stdnet.InterfaceByName(ifname)
		if err != nil {
			return nil
		}
		addrs, _ := ifc.Addrs()
		var ips []string
		for _, a := range addrs {
			if ipn, ok := a.(*stdnet.IPNet); ok && ipn.IP.To4() != nil {
				ips = append(ips, ipn.IP.String())
			}
		}
		return ips
	}
)

func contiene(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
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
	stopGraph(ns)
	stopCredProxy(ns)
	cerrarIPSet(ns)
	borrarEnlace(ns, "vh-"+strings.TrimPrefix(ns, "kl-"))
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
