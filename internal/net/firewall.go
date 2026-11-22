package net

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	stdnet "net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// Egress define qué puede alcanzar una microVM hacia fuera.
//
// El modelo de amenaza de kindling es que el código de dentro es HOSTIL: no
// sabemos qué servidor MCP se va a alojar. Por eso la política por defecto es
// no tener salida, y abrirla es una decisión explícita por máquina.
type Egress string

const (
	// EgressNone: sin salida. La microVM solo habla con quien la invoca.
	// Es el valor por defecto.
	EgressNone Egress = "none"

	// EgressInternet: salida a internet, pero NUNCA a redes privadas. Impide
	// que una herramienta comprometida pivote hacia la LAN de casa, el host de
	// Proxmox, otras microVMs o los metadatos del cloud.
	EgressInternet Egress = "internet"

	// EgressAllowlist: solo salen los dominios declarados; todo lo demás DROP.
	// Es el modo más estricto con salida: parte de que el invitado es HOSTIL y
	// tratará de escaparse (IP directa, resolver ajeno, dominio no listado), así
	// que el filtro no confía en nombres sino en IPs concretas metidas en un
	// ipset, y fuerza el DNS del invitado a un resolver bajo control. Ver
	// applyAllowlist para qué protege de verdad y qué queda pendiente.
	EgressAllowlist Egress = "allowlist"
)

func ParseEgress(s string) (Egress, error) {
	switch Egress(s) {
	case EgressNone, EgressInternet, EgressAllowlist:
		return Egress(s), nil
	case "":
		return EgressNone, nil
	default:
		return "", fmt.Errorf("unknown egress policy: %q (use none, internet, or allowlist)", s)
	}
}

// DNSResolver es el resolver al que se fuerza TODO el DNS del invitado en modo
// allowlist. Redirigir el puerto 53 a un único resolver bajo nuestra elección
// quita al invitado la opción de usar un resolver ajeno como canal de fuga, y es
// también el resolver con el que el host siembra el ipset, para que las IP que
// ve el invitado y las que permitimos coincidan.
//
// El invitado no habla con él directamente: su 53 va al resolver dinámico de
// cada microVM (dnsresolver.go), que responde REFUSED a los dominios no
// permitidos y reenvía aquí el resto. Lo que sigue abierto es el tunneling por
// subdominios de un dominio permitido cuyo NS autoritativo controle el
// atacante: cerrarlo pide un resolver autoritativo propio (ver dnsresolver.go).
const DNSResolver = "1.1.1.1"

// blocked son los destinos que una microVM no debe alcanzar jamás, ni siquiera
// con salida a internet habilitada.
var blocked = []string{
	"10.0.0.0/8",     // privada
	"172.16.0.0/12",  // privada (incluye nuestra propia 172.16/30 y 172.30/16)
	"192.168.0.0/16", // privada: la LAN de casa vive aquí
	"169.254.0.0/16", // link-local y metadatos de cloud
	"127.0.0.0/8",    // loopback del host
	"100.64.0.0/10",  // CGNAT
	"0.0.0.0/8",      // "esta red": hacia 0.0.0.0 el host se llama a sí mismo
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reservada y broadcast
}

// applyEgress instala las reglas de salida dentro del namespace. domains solo se
// usa en modo allowlist; en el resto se ignora.
func (n *Net) applyEgress(e Egress, domains []string) error {
	// ejecutar y no run: es lo que deja a los tests ver las reglas sin netns.
	ns := func(args ...string) error {
		return ejecutar(append([]string{"ip", "netns", "exec", n.NS}, args...)...)
	}

	// La barrera IPv6 (applyIPv6Barrier, más abajo en este fichero) NO va aquí:
	// vive en netns_fc.go/Setup, antes de la rama rápida de iptables-restore que
	// evita esta función para none/internet. Si viviera solo aquí se saltaría en
	// ese camino, que es el habitual cuando iptables-restore está instalado.

	if e == EgressAllowlist {
		return n.applyAllowlist(ns, domains, nil, false)
	}

	// none e internet: las mismas reglas que pone de un golpe restoreRules (ver
	// egressRules), aquí una a una para cuando no hay iptables-restore.
	for _, r := range n.egressRules(e) {
		if err := ns(append([]string{"iptables"}, r...)...); err != nil {
			return err
		}
	}
	return nil
}

// forwardBase son las primeras reglas del FORWARD del netns en los tres
// modos, como argumentos de iptables:
//
//  1. DROP de lo que conntrack marca INVALID (un TCP con solo RST o FIN y
//     carga, por ejemplo). Ni el ACCEPT de ESTABLISHED ni un DROP de NEW lo
//     casan, así que sin esta regla caía en la política ACCEPT del FORWARD:
//     en none era un canal de salida y, falsificando el origen hacia el NSIP
//     de otra microVM, un canal entre microVMs.
//
//  2. ACCEPT de las respuestas a conexiones ya establecidas. Sin esto se rompe
//     el acceso host->microVM, porque nuestra propia red de enlace
//     (172.30.0.0/16) cae DENTRO de 172.16.0.0/12, que está en la lista de
//     bloqueo. No abre ningún agujero: una conexión que el invitado INICIE
//     hacia una red privada es ctstate NEW y sigue cayendo en los DROP de
//     después.
//
//  3. DROP hacia cada IPv4 del host (reglasIPsHost). El host ya las corta en
//     su INPUT (HostInputRules), pero un `ufw reload` o un `firewalld
//     --reload` borra esas reglas y, hasta que el daemon las repone, la IP
//     pública del host quedaba a un SYN del invitado.
//
// Las que reglasGrafo y reglasEnlace insertan con -I FORWARD 1 quedan delante
// de estas: van todas a n.HostIP, el lado host del propio veth.
func (n *Net) forwardBase() [][]string {
	return append([][]string{
		{"-A", "FORWARD", "-i", TapName, "-m", "conntrack", "--ctstate", "INVALID", "-j", "DROP"},
		{"-A", "FORWARD", "-i", TapName, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
	}, n.reglasIPsHost()...)
}

// reglasIPsHost son los DROP de lo que entre por tap0 hacia una IPv4 del
// host, una por dirección, en orden. Quedan fuera n.HostIP (el resolver y los
// proxies de esta máquina viven ahí), el loopback (un invitado no lo enruta)
// y las demás del rango de enlaces: son las HostIP de otras máquinas, que ya
// cortan el 172.16.0.0/12 del modo internet y el ipset (que no admite
// privadas) del allowlist, y que irían y vendrían con cada máquina.
//
// Se leen en cada Setup: una dirección que el host gane después (DHCP) no la
// cubre esta regla, sino la de INPUT.
func (n *Net) reglasIPsHost() [][]string {
	var ips []string
	for _, ip := range ipsDelHost() {
		ip4 := ip.To4()
		if ip4 == nil || ip4[0] == 127 || IndiceDe(ip4) != 0 || ip4.String() == n.HostIP {
			continue
		}
		ips = append(ips, ip4.String())
	}
	slices.Sort(ips)
	var rules [][]string
	for _, ip := range slices.Compact(ips) {
		rules = append(rules, []string{"-A", "FORWARD", "-i", TapName, "-d", ip, "-j", "DROP"})
	}
	return rules
}

// ipsDelHost son las IPv4 de las interfaces del host. Variable para los
// tests: las reglas no deben depender de la máquina que los corre.
var ipsDelHost = func() []stdnet.IP {
	dirs, err := DireccionesHost()
	if err != nil {
		log.Printf("net: WARNING: couldn't list the host addresses: %v", err)
		return nil
	}
	ips := make([]stdnet.IP, 0, len(dirs))
	for _, d := range dirs {
		ips = append(ips, d.IP)
	}
	return ips
}

// forwardCierre es la última regla del FORWARD del netns en none y allowlist:
// DROP de todo lo que entre por tap0 y no haya casado antes, sin mirar el
// ctstate. Antes el cierre era un DROP de --ctstate NEW, y lo que no fuera NEW
// ni ESTABLISHED (INVALID, UNTRACKED) caía en la política ACCEPT. Va como
// regla y no como -P FORWARD DROP para no tocar lo que entra por el veth hacia
// la microVM (host->invitado), que no lleva -i tap0. Sin -o: en el netns solo
// hay tap0, el veth y lo, y nada que salga por tap0 tiene por qué volver a
// salir por otro sitio que no sea el veth.
func forwardCierre() []string {
	return []string{"-A", "FORWARD", "-i", TapName, "-j", "DROP"}
}

// La barrera IPv6 cierra el paso a IPv6 dentro del namespace, en allowlist,
// internet Y none: el filtrado de este fichero (ipset, iptables) es solo
// IPv4, así que sin esto un invitado con salida v6 real (si algún día el host
// reenvía v6, a diferencia de lo medido hoy en el lab) se saltaría el
// allowlist entero por ese camino, y en modo internet o none tendría una
// salida que ningún DROP de aquí cubre. Dos capas independientes, ninguna
// necesita que la otra exista:
//
//  1. sysctlsIPv6: disable_ipv6=1 en tap0, en el veth del namespace y en
//     "all"/"default": la interfaz del invitado no adquiere ninguna dirección
//     v6 (ni siquiera link-local) y el kernel del namespace no la enruta. Es
//     la barrera real: no depende de que ip6tables esté instalado. Va en el
//     mismo sysctl que el resto del netns (aplicarSysctls).
//  2. applyIPv6Barrier: ip6tables FORWARD DROP para lo que entre por tap0:
//     cinturón además de tirantes, para el caso de una interfaz futura que se
//     sumara al namespace sin pasar por el paso 1. Si ip6tables no está
//     instalado (no es una dependencia dura de kindling hoy) se avisa por log
//     y se sigue sin fallar: la capa 1 ya cierra el hueco que importa.
//
// Complementa, no sustituye, a ipv6.disable=1 en la línea de arranque del
// invitado (net.go/BootArg): esa apaga el módulo v6 DENTRO del invitado, pero
// solo en un arranque en frío —un snapshot dorado ya congelado no la relee—.
// Esta barrera vive en el namespace del HOST y cubre también esos snapshots.
func sysctlsIPv6(nsIf string) []string {
	var s []string
	for _, iface := range []string{"all", "default", TapName, nsIf} {
		s = append(s, fmt.Sprintf("net.ipv6.conf.%s.disable_ipv6=1", iface))
	}
	return s
}

// applyIPv6Barrier es la capa 2 de la barrera IPv6 (ver sysctlsIPv6).
func (n *Net) applyIPv6Barrier(ns func(...string) error) error {
	if _, err := exec.LookPath("ip6tables"); err != nil {
		log.Printf("net: ip6tables no está instalado; %s queda solo con la barrera de sysctl para IPv6", n.NS)
		return nil
	}
	return ns("ip6tables", "-A", "FORWARD", "-i", TapName, "-j", "DROP")
}

// sysctlsImprescindibles son los del netns sin los que la máquina no tiene red
// que valga: el reenvío, y el ARP del tap0 reintentando cada 50 ms en vez de
// cada 1 s (ver Setup). ip_forward va antes que sysctlsNetns, porque
// reescribe accept_redirects.
func sysctlsImprescindibles() []string {
	return []string{"net.ipv4.ip_forward=1", "net.ipv4.neigh." + TapName + ".retrans_time_ms=50"}
}

// aplicarSysctls pone todos los sysctl del netns de una máquina en UN proceso:
// los imprescindibles, el endurecimiento (sysctlsNetns) y la capa 1 de la
// barrera IPv6 (sysctlsIPv6). Eran seis (un sh y cinco sysctl).
//
// sysctl aplica las demás claves aunque una falle (un kernel sin conntrack
// cargado no tiene net.netfilter, uno sin IPv6 no tiene net.ipv6), así que un
// fallo solo es fatal si es de un imprescindible: para saberlo se repiten
// esos solos. Lo demás queda en el log y la máquina arranca con lo que se
// pudo, como antes.
func aplicarSysctls(ns func(...string) error, nsName, nsIf string) error {
	claves := append(append(sysctlsImprescindibles(), sysctlsNetns(nsIf)...), sysctlsIPv6(nsIf)...)
	err := ns(append([]string{"sysctl", "-q", "-w"}, claves...)...)
	if err == nil {
		return nil
	}
	if err := ns(append([]string{"sysctl", "-q", "-w"}, sysctlsImprescindibles()...)...); err != nil {
		return err
	}
	log.Printf("net: WARNING: some sysctls failed in %s: %v", nsName, err)
	return nil
}

// egressRules son las reglas de salida de none e internet como argumentos de
// iptables (con -t si no es filter), en orden: las usan tanto restoreRules
// (de un golpe) como applyEgress (una a una). Las dos primeras, forwardBase.
//
// none cierra con forwardCierre: se descarta todo lo que intente salir por el
// veth. El invitado sigue pudiendo responder a lo que le llega (ESTABLISHED).
//
// internet: primero se cierran las redes privadas, sin mirar el ctstate (así
// que también lo INVALID), luego se permite el resto. El orden importa: la
// primera regla que casa, gana.
func (n *Net) egressRules(e Egress) [][]string {
	rules := n.forwardBase()
	if e == EgressNone {
		return append(rules, forwardCierre(), topeTotalNuevas(), topeConexiones())
	}
	for _, cidr := range blocked {
		rules = append(rules, []string{"-A", "FORWARD", "-i", TapName, "-d", cidr, "-j", "DROP"})
	}
	rules = append(rules, topeNuevas())
	return append(rules, []string{"-t", "nat", "-A", "POSTROUTING", "-o", n.NSIf, "-j", "MASQUERADE"},
		topeTotalNuevas(), topeConexiones())
}

// topeNuevasPorSegundo es cuántos paquetes NEW por segundo y por IP destino
// deja salir una microVM (internet y allowlist). Para UDP, todo lo que el
// invitado manda sin respuesta sigue siendo NEW: sin este tope, un invitado
// podía usar el host para inundar una IP permitida (o cualquiera, en
// internet) a todo lo que dé el limitador de Firecracker.
const topeNuevasPorSegundo = 300

// topeNuevas es el DROP de lo que pase de topeNuevasPorSegundo, delante del
// ACCEPT de las conexiones nuevas. La ráfaga es la misma cifra: un npm install
// que abre decenas de conexiones de golpe al mismo registro no pierde SYN
// (con la ráfaga por defecto, 5, el resto esperaría al reintento de 1 s). La
// tabla del hashlimit es por netns, así que el nombre puede ser fijo.
func topeNuevas() []string {
	tope := strconv.Itoa(topeNuevasPorSegundo)
	return []string{"-A", "FORWARD", "-i", TapName, "-m", "conntrack", "--ctstate", "NEW",
		"-m", "hashlimit", "--hashlimit-above", tope + "/s", "--hashlimit-burst", tope,
		"--hashlimit-mode", "dstip", "--hashlimit-name", "kl-nuevas", "-j", "DROP"}
}

// topeTotalNuevasPorSegundo es cuántas conexiones nuevas por segundo, a
// cualquier destino, deja abrir una microVM en total; la ráfaga es el doble.
// topeNuevas cuenta por IP destino y no acota nada si cada paquete va a una IP
// distinta: con UDP de un byte a miles de IPs, cada uno es una entrada de
// conntrack en el kernel del host. Con este tope y los plazos de
// sysctlsNetns, una máquina tiene a lo sumo unas 30 000 entradas a medio abrir
// (~10 MiB), en vez de cientos de miles.
const topeTotalNuevasPorSegundo = 1000

// topeTotalNuevas es el DROP de lo que pase de topeTotalNuevasPorSegundo, en
// los tres modos. En mangle PREROUTING y no en FORWARD: conntrack ya ha
// clasificado el paquete (va antes que mangle) pero no ha confirmado la
// entrada, así que lo descartado no llega a ocupar la tabla; y queda delante
// de los DNAT (DNS, proxies), de los -I FORWARD 1 de reglasGrafo y de lo que
// va al INPUT del netns. Sin --hashlimit-mode: un solo cubo para toda la
// máquina, aunque el invitado falsifique el origen. Usa xt_hashlimit, que
// topeNuevas ya exige.
func topeTotalNuevas() []string {
	return []string{"-t", "mangle", "-A", "PREROUTING", "-i", TapName, "-m", "conntrack", "--ctstate", "NEW",
		"-m", "hashlimit", "--hashlimit-above", strconv.Itoa(topeTotalNuevasPorSegundo) + "/s",
		"--hashlimit-burst", strconv.Itoa(2 * topeTotalNuevasPorSegundo),
		"--hashlimit-name", "kl-total", "-j", "DROP"}
}

// topeConexionesVivas es cuántas conexiones a la vez (entradas de conntrack)
// deja tener abiertas una microVM, a cualquier destino y en los tres modos.
// topeTotalNuevas acota el ritmo, no el total: una conexión TCP establecida
// que el invitado deja abierta vive 5 días en conntrack, así que a 1000 por
// segundo una sola máquina llenaba nf_conntrack_max (262 144 en un host de
// 16 GiB) en unos minutos, y con la tabla llena el kernel tira las conexiones
// nuevas de todas las máquinas y del propio host. Además el host rastrea cada
// flujo otra vez, con sus plazos por defecto (los de sysctlsNetns solo valen
// en el netns): con este tope, lo que una máquina deja en la tabla del host
// queda en unas 16 000 entradas en el peor caso (SYN a destinos muertos, que
// el host guarda 120 s y el netns 30 s). Un npm install o un navegador abren
// decenas o pocos cientos.
const topeConexionesVivas = 4096

// topeConexiones es el DROP de lo NEW cuando la máquina ya tiene
// topeConexionesVivas entradas en el conntrack de su netns. Junto a
// topeTotalNuevas en mangle PREROUTING, por lo mismo: va antes que cualquier
// ACCEPT del FORWARD y que los DNAT, y lo descartado no llega a confirmarse
// en la tabla. --connlimit-mask 0: un solo contador para la máquina, aunque
// el invitado falsifique el origen.
func topeConexiones() []string {
	return []string{"-t", "mangle", "-A", "PREROUTING", "-i", TapName, "-m", "conntrack", "--ctstate", "NEW",
		"-m", "connlimit", "--connlimit-above", strconv.Itoa(topeConexionesVivas), "--connlimit-mask", "0",
		"-j", "DROP"}
}

// conntrackMinimo es el nf_conntrack_max por debajo del cual SetupHost avisa:
// con menos, cuatro máquinas en su tope (topeConexiones, con los plazos más
// largos del host) llenan la tabla del host.
const conntrackMinimo = 4 * 4 * topeConexionesVivas

// avisoConntrack es el aviso de SetupHost si nf_conntrack_max (contenido de
// /proc/sys/net/netfilter/nf_conntrack_max) no da para varias máquinas en su
// tope, o "" si da o no se puede leer.
func avisoConntrack(contenido string) string {
	max, err := strconv.Atoi(strings.TrimSpace(contenido))
	if err != nil || max >= conntrackMinimo {
		return ""
	}
	return fmt.Sprintf("net: WARNING: nf_conntrack_max is %d: a few machines at their limit of %d connections "+
		"can fill the host's connection tracking table and new connections would be dropped for everyone; "+
		"raise it to at least %d (sysctl -w net.netfilter.nf_conntrack_max=%d)",
		max, topeConexionesVivas, conntrackMinimo, conntrackMinimo)
}

// sysctlsNetns son los sysctl de endurecimiento del netns de una microVM, como
// argumentos de `sysctl -w`. Setup los aplica DESPUÉS de ip_forward=1: activar
// el reenvío reescribe accept_redirects en todas las interfaces.
//
//   - rp_filter=1 (estricto): lo que entre por tap0 con un origen que no se
//     enrutaría de vuelta por tap0 (origen falsificado) se descarta antes de
//     enrutarlo.
//   - accept_source_route=0, accept_redirects=0, send_redirects=0: el netns
//     es un enrutador de dos patas y no tiene por qué obedecer ni emitir
//     redirecciones ni rutas de origen.
//   - arp_ignore=1: solo se contesta ARP por una IP de la interfaz que lo
//     recibe; el invitado no descubre por tap0 la IP del veth.
//   - nf_conntrack_udp_timeout=10 y nf_conntrack_tcp_timeout_syn_sent=30: lo
//     que el invitado manda sin respuesta (UDP a mil IPs, SYN a destinos
//     muertos) ocupa la tabla de conntrack, que es memoria del kernel del host
//     fuera de mem_mib, 30 s y 120 s por defecto. Con topeTotalNuevas acotan
//     las entradas vivas por máquina. Son por netns. nf_conntrack_max no: fuera
//     del netns inicial el kernel lo deja de solo lectura.
//
// "all" y la interfaz concreta, porque el kernel combina los dos (máximo en
// rp_filter y arp_ignore, Y u O lógico en el resto); "default" para cualquier
// interfaz que se cree después en el netns.
func sysctlsNetns(nsIf string) []string {
	var s []string
	for _, iface := range []string{"all", "default", TapName, nsIf} {
		p := "net.ipv4.conf." + iface + "."
		s = append(s, p+"rp_filter=1", p+"accept_source_route=0", p+"accept_redirects=0",
			p+"send_redirects=0", p+"arp_ignore=1")
	}
	return append(s, "net.netfilter.nf_conntrack_udp_timeout=10",
		"net.netfilter.nf_conntrack_tcp_timeout_syn_sent=30")
}

// restoreRules añade reglas dentro del namespace con un solo iptables-restore
// --noflush: todas o ninguna, y un proceso en vez de uno por regla.
//
// Con iptables >= 1.6.2, iptables-restore lleva --wait: espera al cerrojo de
// xtables dentro del propio proceso, sin pausas nuestras (ejecutarRestore).
// Con una versión más vieja (rechazaría la opción) o si aun así devuelve
// "cerrojo ocupado", queda el bucle: se reintenta hasta esperaRestore con
// retroceso. iptables-restore aplica cada tabla de una vez, así que un
// intento rechazado no deja nada a medias.
func (n *Net) restoreRules(rules [][]string) error {
	entrada := restoreInput(rules)
	limite := time.Now().Add(esperaRestore)
	pausa := 20 * time.Millisecond
	for {
		out, err := ejecutarRestore(n.NS, entrada)
		if err == nil {
			return nil
		}
		if !cerrojoOcupado(err, out) || time.Now().After(limite) {
			return fmt.Errorf("iptables-restore in %s: %v: %s", n.NS, err, strings.TrimSpace(string(out)))
		}
		dormir(pausa)
		pausa = min(2*pausa, 500*time.Millisecond)
	}
}

// dormir es time.Sleep detrás de una variable, para probar que restoreRules
// no duerme cuando el kernel espera por él.
var dormir = time.Sleep

// esperaRestore es lo que restoreRules espera al cerrojo de xtables, como el
// -w esperaXtables de los demás iptables.
const esperaRestore = 5 * time.Second

// ejecutarRestore lanza iptables-restore --noflush en el netns ns con entrada
// por stdin. Sustituible en tests.
var ejecutarRestore = func(ns, entrada string) ([]byte, error) {
	args := []string{"netns", "exec", ns, "iptables-restore"}
	if restoreSoportaWait() {
		args = append(args, "--wait="+esperaXtables)
	}
	cmd := exec.Command("ip", append(args, "--noflush")...)
	cmd.Stdin = strings.NewReader(entrada)
	return cmd.CombinedOutput()
}

// restoreSoportaWait dice si el iptables-restore instalado entiende --wait
// (iptables >= 1.6.2). Se mira una vez; ante la duda, que no.
var restoreSoportaWait = sync.OnceValue(func() bool {
	out, err := exec.Command("iptables-restore", "--version").CombinedOutput()
	return err == nil && versionRestoreConWait(string(out))
})

// versionRestoreConWait interpreta "iptables-restore v1.8.7 (nf_tables)".
func versionRestoreConWait(salida string) bool {
	i := strings.Index(salida, " v")
	if i < 0 {
		return false
	}
	var mayor, menor, parche int
	if k, _ := fmt.Sscanf(salida[i+2:], "%d.%d.%d", &mayor, &menor, &parche); k < 2 {
		return false
	}
	return mayor > 1 || (mayor == 1 && (menor > 6 || (menor == 6 && parche >= 2)))
}

// cerrojoOcupado dice si iptables(-restore) falló porque otro tenía el
// cerrojo de xtables: código 4 (RESOURCE_PROBLEM) o su mensaje.
func cerrojoOcupado(err error, out []byte) bool {
	var salida *exec.ExitError
	if errors.As(err, &salida) && salida.ExitCode() == 4 {
		return true
	}
	return strings.Contains(string(out), "xtables lock")
}

// restoreInput es la entrada de iptables-restore para rules, agrupadas por
// tabla en el orden en que aparece cada una.
func restoreInput(rules [][]string) string {
	tablas := map[string][]string{}
	var orden []string
	for _, r := range rules {
		tabla := "filter"
		if len(r) >= 2 && r[0] == "-t" {
			tabla, r = r[1], r[2:]
		}
		if _, ok := tablas[tabla]; !ok {
			orden = append(orden, tabla)
		}
		tablas[tabla] = append(tablas[tabla], strings.Join(r, " "))
	}
	var b strings.Builder
	for _, t := range orden {
		fmt.Fprintf(&b, "*%s\n%s\nCOMMIT\n", t, strings.Join(tablas[t], "\n"))
	}
	return b.String()
}

// setName es el nombre del ipset de dominios permitidos de este namespace. El
// ipset es POR namespace (los comandos corren dentro de él), así que basta con
// un nombre estable; al borrar el netns en Teardown, su ipset se va con él.
func (n *Net) setName() string {
	return setNameFromNS(n.NS)
}

// setNameFromNS deriva el nombre del ipset a partir del nombre del netns, para
// poder sembrarlo desde el resolver sin tener el *Net completo delante.
func setNameFromNS(ns string) string {
	return "klaw-" + strings.TrimPrefix(ns, "kl-")
}

// applyAllowlist monta el modo allowlist dentro del namespace.
//
// QUÉ PROTEGE DE VERDAD (con el invitado tratado como hostil):
//   - IP directa: el invitado NO puede conectar a una IP que no esté en el ipset,
//     aunque se salte el DNS por completo. El filtro es por IP, no por nombre.
//   - Resolver ajeno: TODO el tráfico al puerto 53 se redirige (DNAT) al resolver
//     bajo nuestro control, así que el invitado no puede hablar con un resolver
//     suyo ni tunelar por uno arbitrario.
//   - Dominio no listado: aunque el invitado lo resuelva, la IP resultante no
//     está en el ipset y la conexión se descarta.
//   - Redes privadas: el host (HostEgressRules) sigue bloqueándolas como segunda
//     barrera, y al sembrar el ipset se descartan las IP privadas que devuelva un
//     resolver envenenado.
//
// IP VOLÁTIL (CDN / DNS rotatorio): lo resuelve el resolver dinámico del host,
// en dnsresolver.go. El 53 del invitado se DNATea a un resolver por microVM que
// corre en el host; ese resolver siembra en el ipset, con el TTL real del
// registro, la IP que va a devolver JUSTO ANTES de responder. Así la IP que el
// invitado usará siempre está permitida, aunque cambie en cada consulta. El
// sembrado estático de abajo queda como arranque en caliente y red de seguridad
// por si el resolver tuviera un tropiezo.
//
// QUÉ SIGUE PENDIENTE / TODO:
//   - AAAA / IPv6: el resolver solo siembra A (IPv4); el ipset es v4. Ver
//     extractA. Esto YA NO es el hueco que parece: applyIPv6Barrier (más abajo
//     en este fichero) apaga IPv6 en el namespace entero antes de llegar a esta
//     función, así que el invitado no tiene forma de usar una AAAA aunque el
//     resolver se la sirviera. Sigue sin sembrarse por ser trabajo sin efecto,
//     no por el hueco de seguridad que este TODO describía antes.
//   - Tunneling por SUBDOMINIOS de un dominio permitido: se reenvían (un CDN los
//     necesita), así que un dominio permitido con NS autoritativo del atacante
//     sigue siendo un canal. Ver dnsresolver.go.
//
// previas son reglas que van delante (el DNAT de entrada de Setup). Con
// restore, todas van en UN iptables-restore, como none e internet: una a una
// eran ~25 procesos de iptables que releen el conjunto entero cada uno. Sin
// restore (no está instalado), una a una.
func (n *Net) applyAllowlist(ns func(...string) error, domains []string, previas [][]string, restore bool) error {
	if _, err := exec.LookPath("ipset"); err != nil {
		return fmt.Errorf("allowlist mode needs ipset and it is not installed: %w", err)
	}

	set := n.setName()
	// hash:ip con la opción timeout ACTIVADA (el "timeout 0" del create solo fija
	// el defecto en "permanente"; lo que importa es que habilita el timeout por
	// entrada). El resolver dinámico añade cada IP con "timeout <ttl-real>", así
	// que caduca sola cuando expira el registro. El sembrado estático de abajo es
	// la red de seguridad, con semillaTTL (ver sembrarSemillas).
	// maxelem acota la memoria del set en el kernel (por defecto 65536): un
	// invitado que llena el set recibe HASH_FULL y no siembra más, sin afectar
	// a las demás máquinas.
	if err := ns("ipset", "create", set, "hash:ip", "timeout", "0", "maxelem", strconv.Itoa(ipsetMaxElem), "-exist"); err != nil {
		return err
	}

	// Sembrado estático: resolvemos en el host, por el MISMO resolver al que
	// forzaremos al invitado, y metemos solo IPv4 públicas. Las privadas se
	// descartan aquí para que un resolver envenenado no cuele 169.254 (metadatos)
	// ni 192.168 (LAN de casa) en la lista de permitidos.
	// Se resuelve mientras se ponen las reglas: son independientes, y el
	// set ya existe para el --match-set.
	semillas := make(chan []string, 1)
	go func() { semillas <- resolverSemillas(domains) }()

	reglas := append(append([][]string{}, previas...), n.reglasAllowlist()...)
	var errReglas error
	if restore {
		errReglas = n.restoreRules(reglas)
	} else {
		for _, r := range reglas {
			if errReglas = ns(append([]string{"iptables"}, r...)...); errReglas != nil {
				break
			}
		}
	}
	ips := <-semillas
	if errReglas != nil {
		return errReglas
	}
	sembrarSemillas(n.NS, set, ips)

	// Arrancar el resolver dinámico del host para este netns. Debe quedar vivo
	// ANTES de devolver: es quien siembra el ipset con las IP reales (y su TTL) que
	// el invitado va a usar. Si no arranca, el DNAT de arriba apunta a un puerto sin
	// nadie escuchando y el invitado se queda sin DNS, así que su fallo es fatal.
	return startDNSResolver(n, domains)
}

// semillaTTL (segundos) es la vida de una IP del sembrado estático: la del
// techo del resolver (dnsMaxTTL). Antes iban sin timeout, y una IP que el
// dominio dejaba de usar (un CDN que rota, un dominio que cambia de dueño)
// seguía abierta toda la vida del netns. Si el invitado la sigue usando, la
// renueva al preguntar por el dominio: el resolver la añade con su TTL real.
// Lo ya establecido no depende del set (ESTABLISHED, forwardBase).
const semillaTTL = dnsMaxTTL

// anadirSemilla es anadirIPSet; una variable para los tests.
var anadirSemilla = anadirIPSet

// sembrarSemillas mete en el set las IPs resueltas al arrancar, con semillaTTL.
func sembrarSemillas(ns, set string, ips []string) {
	for _, ip := range ips {
		_ = anadirSemilla(ns, set, ip, semillaTTL)
	}
}

// reglasAllowlist son las reglas del modo allowlist como argumentos de
// iptables (con -t si no es filter), en orden: las pone applyAllowlist de un
// golpe con iptables-restore o una a una.
func (n *Net) reglasAllowlist() [][]string {
	set := n.setName()
	rules := n.forwardBase()
	sinIptables := func(rs [][]string) {
		for _, r := range rs {
			rules = append(rules, r[1:])
		}
	}

	// Forzar el DNS del invitado a NUESTRO resolver del host: cualquier salida al
	// puerto 53 (UDP y TCP) se reescribe hacia n.HostIP:dnsPort, el resolver
	// dinámico que corre en el host atado a ESTE netns (ver dnsresolver.go). Antes
	// se DNATeaba a DNSResolver (1.1.1.1) directo, que resolvía pero no sembraba el
	// ipset ni cerraba el tunneling; ahora pasa primero por nuestro resolver, que
	// solo responde por los dominios permitidos y siembra la IP antes de devolverla.
	dnsTarget := fmt.Sprintf("%s:%d", n.HostIP, dnsPort)
	for _, proto := range []string{"udp", "tcp"} {
		rules = append(rules, []string{"-t", "nat", "-A", "PREROUTING", "-i", TapName,
			"-p", proto, "--dport", "53", "-j", "DNAT",
			"--to-destination", dnsTarget})
	}

	// Proxy de credenciales (credproxy.go): el resolver contesta con n.HostIP para
	// un dominio con credencial y el invitado conecta a él; los DNAT de
	// credNATRules llevan el 80/443 al proxy HTTP y el resto de puertos TCP al
	// de Postgres. Si la máquina no tiene credenciales no escucha nadie y la
	// conexión se rechaza: las reglas no abren nada que no sean los proxies.
	sinIptables(n.credNATRules())

	// FORWARD, en orden (la primera que casa gana):
	//   1. DNS hacia nuestro resolver del host: permitido. Tras el DNAT el destino
	//      es n.HostIP:dnsPort (el lado host del veth), así que el paquete cruza el
	//      FORWARD del netns antes de llegar al resolver. Lo mismo los proxies
	//      de credenciales en n.HostIP:credPort y n.HostIP:pgPort.
	dnsPortStr := fmt.Sprintf("%d", dnsPort)
	for _, proto := range []string{"udp", "tcp"} {
		rules = append(rules, []string{"-A", "FORWARD", "-i", TapName, "-o", n.NSIf,
			"-p", proto, "-d", n.HostIP, "--dport", dnsPortStr, "-j", "ACCEPT"})
	}
	sinIptables(n.credForwardRules())
	//   2. Conexiones NUEVAS cuya IP destino esté en el ipset: permitidas, con
	//      el tope de topeNuevas delante.
	rules = append(rules, topeNuevas())
	rules = append(rules, []string{"-A", "FORWARD", "-i", TapName, "-o", n.NSIf,
		"-m", "conntrack", "--ctstate", "NEW",
		"-m", "set", "--match-set", set, "dst", "-j", "ACCEPT"})
	//   3. Todo lo demás que entre por tap0: DROP (forwardCierre), sea NEW o no.
	//      Esto bloquea IP directa, dominios no listados y cualquier otro
	//      puerto/destino.
	rules = append(rules, forwardCierre())

	// El host enmascara hacia internet y vuelve a bloquear las redes privadas como
	// segunda barrera (HostEgressRules), igual que en modo internet.
	return append(rules, []string{"-t", "nat", "-A", "POSTROUTING", "-o", n.NSIf, "-j", "MASQUERADE"},
		topeTotalNuevas(), topeConexiones())
}

// credNATRules son los DNAT del proxy de credenciales, en orden (el primero que
// casa gana; los del DNS van antes, en applyAllowlist):
//   - 80 y 443 de n.HostIP al proxy HTTP (credPort). El 443 a propósito: un
//     SDK que insista en https://dominio se encuentra un servidor HTTP plano
//     (o nada, si la máquina no tiene credenciales) y su handshake TLS muere en
//     el acto con un error que el operador ve, en vez de esperar al plazo del
//     SDK contra el DROP. No abre nada: el proxy no habla TLS. Se hace así y no
//     con REJECT --reject-with tcp-reset porque ese target necesita xt_REJECT
//     y en un CT sin él la regla falla y la máquina no arranca (visto en el lab).
//   - El 3306 de n.HostIP al listener de MySQL (myPort): el mismo proxy de
//     bases de datos, pero sabiendo que el invitado marcó el 3306 (con
//     credenciales Postgres y MySQL en la máquina, eso es lo que dice que es
//     MySQL; ver pkg/credproxy/mysql.go). Con solo credenciales Postgres, lo
//     que llegue ahí es Postgres igual.
//   - Cualquier otro puerto TCP de n.HostIP al proxy de Postgres (pgPort): el
//     invitado usa el puerto de su cadena de conexión (5432 o el que sea) y no
//     hace falta saberlo de antemano. Es una regla fija y no una por
//     credencial, sin multiport: el orden deja fuera el 53, el 80 y el 443. No
//     abre nada nuevo: todo lo que llegue ahí lo atiende (y lo rechaza, si no
//     trae un marcador) el proxy. El listener existe en cuanto la máquina
//     tiene cualquier credencial, pero sin una de Postgres cierra cada
//     conexión sin leerla (ServePG), así que el analizador no queda expuesto.
func (n *Net) credNATRules() [][]string {
	var rules [][]string
	for _, puerto := range []string{"80", "443"} {
		rules = append(rules, []string{"iptables", "-t", "nat", "-A", "PREROUTING", "-i", TapName,
			"-p", "tcp", "-d", n.HostIP, "--dport", puerto, "-j", "DNAT",
			"--to-destination", fmt.Sprintf("%s:%d", n.HostIP, credPort)})
	}
	rules = append(rules, []string{"iptables", "-t", "nat", "-A", "PREROUTING", "-i", TapName,
		"-p", "tcp", "-d", n.HostIP, "--dport", strconv.Itoa(credproxy.MySQLDefaultPort), "-j", "DNAT",
		"--to-destination", fmt.Sprintf("%s:%d", n.HostIP, myPort)})
	return append(rules, []string{"iptables", "-t", "nat", "-A", "PREROUTING", "-i", TapName,
		"-p", "tcp", "-d", n.HostIP, "-j", "DNAT",
		"--to-destination", fmt.Sprintf("%s:%d", n.HostIP, pgPort)})
}

// credForwardRules dejan pasar por el FORWARD del netns lo que los DNAT de
// credNATRules llevan a los proxies.
func (n *Net) credForwardRules() [][]string {
	var rules [][]string
	for _, puerto := range []string{credPortStr, pgPortStr, myPortStr} {
		rules = append(rules, []string{"iptables", "-A", "FORWARD", "-i", TapName, "-o", n.NSIf,
			"-p", "tcp", "-d", n.HostIP, "--dport", puerto, "-j", "ACCEPT"})
	}
	return rules
}

// resolvePublicIPv4 resuelve un dominio a sus IPv4 públicas, usando el mismo
// resolver (DNSResolver) al que se fuerza al invitado, para que lo que sembramos
// coincida con lo que el invitado verá. Devuelve solo direcciones enrutables:
// las privadas/link-local se descartan por seguridad (resolver envenenado).
// Solo IPv4: las reglas de este fichero son iptables v4. Sin IP falla CERRADO:
// el dominio simplemente no se permite.
func resolvePublicIPv4(ctx context.Context, domain string) []string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return nil
	}
	return buscarIPv4(ctx, DNSResolver+":53", domain)
}

// buscarIPv4 es credproxy.LookupPublicIPv4 detrás de una variable, para los
// tests.
var buscarIPv4 = credproxy.LookupPublicIPv4

// plazoSemillas es lo que puede tardar, EN TOTAL, el sembrado estático del
// ipset al montar la red. Antes se resolvía dominio a dominio sin plazo: cada
// uno de 20 a 70 ms medidos en el lab, y hasta 3 × 2 s si el resolver perdía
// datagramas, todo en el camino del arranque. El sembrado es la red de
// seguridad: el resolver dinámico siembra de todos modos cada IP que el
// invitado pregunte, antes de contestarle. Un dominio que no llegue a tiempo
// se queda sin semilla, no sin acceso.
const plazoSemillas = 500 * time.Millisecond

// resolverSemillas resuelve los dominios A LA VEZ, con plazoSemillas para
// todos, y devuelve las IPv4 públicas que hayan llegado, sin repetir y en el
// orden de los dominios.
func resolverSemillas(domains []string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), plazoSemillas)
	defer cancel()
	res := make([][]string, len(domains))
	var wg sync.WaitGroup
	for i, d := range domains {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i] = resolvePublicIPv4(ctx, d)
		}()
	}
	wg.Wait()
	visto := map[string]bool{}
	var out []string
	for _, ips := range res {
		for _, ip := range ips {
			if !visto[ip] {
				visto[ip] = true
				out = append(out, ip)
			}
		}
	}
	return out
}

// isBlockedIP dice si una IP cae en alguno de los rangos que jamás se permiten,
// para no sembrar el ipset con una privada que devuelva un resolver hostil.
func isBlockedIP(ip stdnet.IP) bool {
	for _, cidr := range blocked {
		if _, n, err := stdnet.ParseCIDR(cidr); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// InalcanzableDesdeUnInvitado dice si una direccion, escrita como texto, es de
// las que una microVM no puede alcanzar jamas.
//
// Es el mismo criterio que filtra lo que entra en el ipset, expuesto para quien
// necesite comprobar una CONFIGURACION en vez de un destino: un resolv.conf que
// apunte a 127.0.0.53 o a la IP privada del router es inservible dentro de un
// invitado, y saberlo no requiere tocar la red.
func InalcanzableDesdeUnInvitado(dir string) bool {
	ip := stdnet.ParseIP(strings.TrimSpace(dir))
	if ip == nil {
		return true // lo que no es una IP no lleva a ninguna parte
	}
	return isBlockedIP(ip)
}

// privada172SinHostSubnet es 172.16.0.0/12 menos HostSubnet (172.30.0.0/16):
// 172.16-23, 172.24-27, 172.28-29 y 172.31. iptables no admite dos -d en una
// regla, y un ACCEPT o RETURN para HostSubnet delante adelantaría la política
// del host a lo que tenga detrás. Si cambia hostPrefix, el test lo recalcula.
var privada172SinHostSubnet = []string{
	"172.16.0.0/13",
	"172.24.0.0/14",
	"172.28.0.0/15",
	"172.31.0.0/16",
}

// HostEgressRules son las reglas que el host necesita para dar salida a los
// namespaces. Se instalan una sola vez, no por máquina.
//
// Se aplican como segunda barrera: aunque alguien manipulara las reglas de un
// namespace, el host sigue negando el acceso a la red privada. Todas las de
// FORWARD son DROP, así que su orden no importa y añadir una nueva con -A en
// un host que ya tenía las viejas no deja ninguna detrás de un ACCEPT nuestro.
//
//   - El agente (8080) desde fuera del host: un vecino de la LAN que enrute
//     HostSubnet por aquí, un contenedor con red puente u otra máquina. No lo
//     abre nadie legítimo; el daemon y el CLI llegan desde el propio host, por
//     OUTPUT, y eso lo decide reglasAcceso. Va en mangle PREROUTING y al
//     principio, no en FORWARD: ahí Docker (DOCKER-USER y DOCKER-FORWARD, que
//     reinserta arriba cada vez que arranca), ufw o libvirt aceptan antes que
//     cualquier -A nuestro, y un ACCEPT de FORWARD termina el recorrido de esa
//     cadena. Lo que se tira en PREROUTING ya no llega a FORWARD.
//   - Nada de un veth de kindling a otro: el host no reenvía entre namespaces
//     (los enlaces de los grafos van por proxies en INPUT, ver enlaces_fc.go).
//   - Lo que entre por un veth con origen fuera de HostSubnet es falsificado
//     (todo lo legítimo sale del netns con el NSIP o enmascarado al veth), y
//     lo INVALID no lo crea ninguna conexión legítima. Las dos cubren también
//     a un netns con las reglas de antes de forwardBase/forwardCierre.
//   - Las redes privadas de blocked, salvo HostSubnet: 172.16.0.0/12 va
//     partida en los trozos que la componen sin 172.30.0.0/16, para que una
//     LAN en 172.16-172.29 o 172.31 tenga también esta segunda barrera.
func HostEgressRules(bridge string) [][]string {
	rules := [][]string{
		{"iptables", "-A", "FORWARD", "-i", "vh-+", "-o", "vh-+", "-j", "DROP"},
		{"iptables", "-A", "FORWARD", "-i", "vh-+", "!", "-s", HostSubnet, "-j", "DROP"},
		{"iptables", "-A", "FORWARD", "-i", "vh-+", "-m", "conntrack", "--ctstate", "INVALID", "-j", "DROP"},
	}
	for _, cidr := range blocked {
		destinos := []string{cidr}
		if cidr == "172.16.0.0/12" {
			destinos = privada172SinHostSubnet
		}
		for _, d := range destinos {
			rules = append(rules, []string{
				"iptables", "-A", "FORWARD", "-s", HostSubnet, "-d", d, "-j", "DROP",
			})
		}
	}
	rules = append(rules,
		[]string{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", HostSubnet, "!", "-d", HostSubnet, "-j", "MASQUERADE"},
		reglaAgenteDesdeFuera(),
	)
	return rules
}

// reglaAgenteDesdeFuera es la del agente en mangle PREROUTING (ver
// HostEgressRules). Sin conntrack: lo que no es NEW tampoco tiene por qué
// llegar de fuera al 8080 de una máquina.
func reglaAgenteDesdeFuera() []string {
	return []string{"iptables", "-t", "mangle", "-I", "PREROUTING", "1", "-d", HostSubnet,
		"-p", "tcp", "--dport", strconv.Itoa(puertoAgente), "-j", "DROP"}
}

// centinelaForward es la última DROP de FORWARD de HostEgressRules: si un
// reload del cortafuegos vació FORWARD, falta.
func centinelaForward() []string {
	var ultima []string
	for _, r := range HostEgressRules("") {
		if slices.Contains(r, "FORWARD") {
			ultima = r
		}
	}
	return ultima
}

// paraComprobar convierte una regla con -A o -I (con posición) en su -C.
func paraComprobar(r []string) []string {
	if slices.Contains(r, "-I") {
		return sinPosicion(r, "-C")
	}
	return aComprobar(r)
}

// montarEgresoHost instala las HostEgressRules que falten, y la del agente
// en mangle, además, la primera (ponerPrimera).
func montarEgresoHost() error {
	agente := reglaAgenteDesdeFuera()
	for _, r := range HostEgressRules("") {
		if slices.Equal(r, agente) {
			if err := ponerPrimera(r); err != nil {
				return err
			}
			continue
		}
		esta, err := comprobarEstado(paraComprobar(r))
		if err != nil {
			return err
		}
		if esta {
			continue
		}
		if err := ejecutar(r...); err != nil {
			return err
		}
	}
	return nil
}

// linkBasePortRango y linkMaxPortsRango son el rango de los proxies de enlace
// (enlaces_fc.go: linkBasePort, linkMaxPorts). Aquí aparte porque este
// fichero también compila en macOS, donde no hay enlaces.
const (
	linkBasePortRango = 5400
	linkMaxPortsRango = 64
)

// HostSubnet es el rango que kindling usa para los enlaces con los namespaces.
const HostSubnet = hostPrefix + ".0.0/16"

// puertoAgente es el puerto del agente del invitado (api.GuestPort; aquí
// aparte para no depender de pkg/api, y un test comprueba que coinciden).
const puertoAgente = 8080

// cadenaAcceso es la cadena del OUTPUT del host que decide qué procesos del
// host abren conexiones hacia las máquinas (reglasAcceso).
const cadenaAcceso = "KL-ACCESO"

// AccesoAgente son, además del propio daemon (su uid y su gid efectivos), los
// usuarios y grupos que pueden abrir conexiones al agente de las máquinas
// desde el host: los del socket del daemon cuando se cede (socketOwner).
//
// Dueno identifica al daemon que los pide (su raíz): sus permisos llevan una
// etiqueta propia en cadenaAcceso, y así puede quitar los que ya no da (el
// socket pasó a otro usuario) sin tocar los de otro daemon del mismo host.
// Vacío, sin etiqueta y sin limpieza (lo de antes).
type AccesoAgente struct {
	UIDs, GIDs []int
	Dueno      string
}

// etiqueta es el comentario que marca los permisos de a en cadenaAcceso.
func (a AccesoAgente) etiqueta() string {
	if a.Dueno == "" {
		return ""
	}
	h := sha256.Sum256([]byte(a.Dueno))
	return "kling:" + hex.EncodeToString(h[:6])
}

// reglasAcceso son las reglas de cadenaAcceso y el salto a ella desde OUTPUT.
//
// El agente no autentica (pkg/guest/origen_host.go): da por host lo que le
// llega por su ruta por defecto, y en Linux todo proceso del host llega así a
// 172.30.X.2:8080 por la DNAT del netns. Un usuario local sin acceso al
// socket ejecutaba como root en cualquier máquina con exec sin pasar por el
// daemon ni por su autorización. Lo mismo valía para el lado host de cada
// veth (172.30.X.1): las reglas INPUT solo miran lo que entra por un veth, y
// un proceso local llegaba a los proxies de las aristas de grafo (que pueden
// apuntar al agente de otra máquina), al resolver y a los proxies de
// credenciales.
//
// Ahora, hacia cualquier destino de HostSubnet:
//   - root, el daemon y quien puede usar su socket (los uids y gids de a,
//     estos con --suppl-groups) abren lo que quieran: ya mandan sobre el
//     daemon, así que llegar al agente no les da nada que no tengan. El
//     gateway de ext/mcp y el CLI entran por ahí;
//   - cualquier otro llega a los puertos de servicio de la IP de cada máquina
//     (los de `kling db`, `kling.ports`), cuya frontera no es la red sino la
//     clave de cada servicio (docs/db.md); al agente (8080) y a las
//     direcciones del propio host en HostSubnet (los lados host de los veth),
//     no: REJECT.
//
// El gid 0 nunca: con el daemon como root su egid es 0, y un permiso para el
// gid 0 con --suppl-groups abría el agente a cualquier usuario del grupo root
// (que no es root). root ya entra por su uid.
//
// Las respuestas no se tocan (solo --ctstate NEW): un servicio del host que
// contesta a un invitado no abre una conexión nueva.
func reglasAcceso(a AccesoAgente, euid, egid int) (permisos, cierre [][]string, salto []string) {
	uids := append([]int{0, euid}, a.UIDs...)
	gids := append([]int{egid}, a.GIDs...)
	slices.Sort(uids)
	slices.Sort(gids)
	var marca []string
	if e := a.etiqueta(); e != "" {
		marca = []string{"-m", "comment", "--comment", e}
	}
	for _, u := range slices.Compact(uids) {
		r := []string{"iptables", "-I", cadenaAcceso, "1", "-m", "owner", "--uid-owner", strconv.Itoa(u)}
		permisos = append(permisos, append(append(r, marca...), "-j", "RETURN"))
	}
	for _, g := range slices.Compact(gids) {
		if g == 0 {
			continue
		}
		r := []string{"iptables", "-I", cadenaAcceso, "1", "-m", "owner", "--gid-owner", strconv.Itoa(g), "--suppl-groups"}
		permisos = append(permisos, append(append(r, marca...), "-j", "RETURN"))
	}
	// REJECT con su ICMP por defecto y no tcp-reset: en OUTPUT el RST sale
	// hacia la máquina en vez de volver al socket local, y el connect esperaba
	// a su plazo (visto en el lab); el ICMP sí le llega y falla al momento.
	cierre = [][]string{
		{"iptables", "-A", cadenaAcceso, "-p", "tcp", "--dport", strconv.Itoa(puertoAgente),
			"-m", "conntrack", "--ctstate", "NEW", "-j", "REJECT"},
		{"iptables", "-A", cadenaAcceso, "-m", "addrtype", "--dst-type", "LOCAL",
			"-m", "conntrack", "--ctstate", "NEW", "-j", "REJECT"},
	}
	salto = []string{"iptables", "-I", "OUTPUT", "1", "-d", HostSubnet, "-j", cadenaAcceso}
	return permisos, cierre, salto
}

// permisoGrupoRootViejo es el permiso que versiones anteriores daban al gid 0
// con el daemon como root (ver reglasAcceso): setupAcceso lo quita.
var permisoGrupoRootViejo = []string{"iptables", "-D", cadenaAcceso, "-m", "owner", "--gid-owner", "0", "--suppl-groups", "-j", "RETURN"}

// sinSuppl es la regla sin --suppl-groups.
func sinSuppl(r []string) []string {
	return slices.DeleteFunc(slices.Clone(r), func(x string) bool { return x == "--suppl-groups" })
}

// setupAcceso monta reglasAcceso sin vaciar la cadena: en un host con dos
// daemons (uno de systemd y otro privado, cada uno con el dueño de su socket)
// es de los dos, así que cada uno añade lo suyo si falta y solo quita lo suyo
// que sobra (lo de su etiqueta, ver AccesoAgente.Dueno).
//
// Cierra antes de abrir: primero los REJECT (en una cadena a la que aún no
// salta nadie no cambian nada), luego los permisos con -I 1, delante de los
// REJECT, y el salto el último. Si un permiso no entra, el salto se pone
// igual y se devuelve el error: quien no tiene permiso se queda fuera, nunca
// al revés. Con un iptables sin --suppl-groups (anterior a la 1.8.3) el
// permiso de cada grupo vale solo para su grupo primario.
func setupAcceso(a AccesoAgente, euid, egid int) error {
	permisos, cierre, salto := reglasAcceso(a, euid, egid)
	existe, err := comprobarEstado([]string{"iptables", "-n", "-L", cadenaAcceso})
	if err != nil {
		return err
	}
	if !existe {
		if err := ejecutar("iptables", "-N", cadenaAcceso); err != nil {
			return err
		}
	}
	for _, r := range cierre {
		if err := ponerSiFalta(r); err != nil {
			return err
		}
	}
	var errs []error
	for _, r := range permisos {
		err := ponerSiFalta(r)
		if err == nil || !slices.Contains(r, "--suppl-groups") {
			errs = append(errs, err)
			continue
		}
		if err2 := ponerSiFalta(sinSuppl(r)); err2 != nil {
			errs = append(errs, fmt.Errorf("%w; without --suppl-groups: %v", err, err2))
			continue
		}
		log.Printf("net: WARNING: iptables without --suppl-groups (%v): only processes whose primary group is %s reach the machines' agent",
			err, valorOpcion(r, "--gid-owner"))
	}
	// También su variante sin --suppl-groups, la que quedaba con un iptables
	// sin esa opción (ver arriba): abría el agente a quien tuviera el grupo
	// root como primario.
	for _, viejo := range [][]string{permisoGrupoRootViejo, sinSuppl(permisoGrupoRootViejo)} {
		comprobar := slices.Clone(viejo)
		comprobar[1] = "-C" // viejo es un -D
		for comprobarRegla(comprobar) {
			if ejecutar(viejo...) != nil {
				break
			}
		}
	}
	errs = append(errs, podarAcceso(a, permisos), ponerPrimera(salto))
	return errors.Join(errs...)
}

// ponerPrimera deja r (un -I CADENA 1) como la primera regla de su cadena.
//
// -C dice si una regla está, no dónde, y las dos que van así (el salto de
// OUTPUT a cadenaAcceso y la del agente en mangle PREROUTING) solo valen
// delante: otro cortafuegos que inserte arriba un salto a sus cadenas (ufw,
// Calico) con un ACCEPT dentro se las saltaba con las reglas en su sitio
// según -C. Si está pero no la primera, se quita y se vuelve a insertar; el
// instante sin ella no abre nada que no abriera ya ir detrás.
func ponerPrimera(r []string) error {
	primera, err := esPrimera(r)
	if err != nil || primera {
		return err
	}
	borrar := sinPosicion(r, "-D")
	if comprobarRegla(sinPosicion(r, "-C")) {
		log.Printf("net: %s is no longer the first rule of its chain; moving it back to the top", strings.Join(r[1:], " "))
	}
	for comprobarRegla(sinPosicion(r, "-C")) {
		if err := ejecutar(borrar...); err != nil {
			return err
		}
	}
	return ejecutar(r...)
}

// esPrimera dice si r (un -I CADENA 1, con su -t si no es filter) es la
// primera regla de su cadena, según `iptables -S`.
//
// Parte cada línea por espacios: r no puede llevar argumentos con espacios
// (un --comment "a b"), o nunca saldría igual y ponerPrimera la movería en
// cada vuelta del vigilante.
func esPrimera(r []string) (bool, error) {
	tabla, cadena, espec := partirRegla(r)
	lineas, err := listarTabla(tabla, cadena)
	if err != nil {
		return false, err
	}
	quiere := normalizarRegla(espec)
	for _, l := range lineas {
		f := strings.Fields(l)
		if len(f) < 2 || f[0] != "-A" {
			continue // el -P de la política, el -N de la cadena
		}
		return slices.Equal(normalizarRegla(f[2:]), quiere), nil
	}
	return false, nil
}

// partirRegla separa una regla "iptables [-t T] -I CADENA 1 espec..." en su
// tabla ("filter" si no lleva), su cadena y su especificación.
func partirRegla(r []string) (tabla, cadena string, espec []string) {
	a := r[1:]
	tabla = "filter"
	if len(a) > 1 && a[0] == "-t" {
		tabla, a = a[1], a[2:]
	}
	return tabla, a[1], a[3:]
}

// normalizarRegla quita lo que `iptables -S` añade al listar (el -m tcp o
// -m udp implícito del --dport de un -p tcp o -p udp) y ordena lo que queda:
// -S también reordena las opciones (origen y destino antes que la interfaz,
// por ejemplo), y las dos reglas que se comparan así no se confunden con
// otra por el orden.
func normalizarRegla(f []string) []string {
	// Se ordenan cláusulas, no tokens: cada opción va con sus valores (y con
	// el "!" que la niega), así "-s A -d B" no se confunde con "-s B -d A".
	var claus [][]string
	for _, t := range quitarMatchImplicito(f) {
		nueva := len(claus) == 0 || t == "!" ||
			(strings.HasPrefix(t, "-") && !(len(claus[len(claus)-1]) == 1 && claus[len(claus)-1][0] == "!"))
		if nueva {
			claus = append(claus, []string{t})
			continue
		}
		claus[len(claus)-1] = append(claus[len(claus)-1], t)
	}
	slices.SortFunc(claus, func(a, b []string) int { return slices.Compare(a, b) })
	out := make([]string, 0, len(f))
	for _, c := range claus {
		out = append(out, c...)
	}
	return out
}

// quitarMatchImplicito es f sin los -m tcp y -m udp, y sin comillas.
func quitarMatchImplicito(f []string) []string {
	out := make([]string, 0, len(f))
	for i := 0; i < len(f); i++ {
		if f[i] == "-m" && i+1 < len(f) && (f[i+1] == "tcp" || f[i+1] == "udp") {
			i++
			continue
		}
		out = append(out, strings.Trim(f[i], `"`))
	}
	return out
}

// listarTabla devuelve `iptables -t tabla -S cadena`, una regla por línea.
var listarTabla = func(tabla, cadena string) ([]string, error) {
	out, err := exec.Command("iptables", "-w", esperaXtables, "-t", tabla, "-S", cadena).Output()
	if err != nil {
		return nil, fmt.Errorf("iptables -t %s -S %s: %w", tabla, cadena, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

// ponerSiFalta ejecuta r (-A o -I) si su -C dice que no está.
func ponerSiFalta(r []string) error {
	esta, err := comprobarEstado(paraComprobar(r))
	if err != nil || esta {
		return err
	}
	return ejecutar(r...)
}

// podarAcceso quita de cadenaAcceso los permisos con la etiqueta de a que ya
// no están en permisos: los de un dueño anterior del socket.
func podarAcceso(a AccesoAgente, permisos [][]string) error {
	e := a.etiqueta()
	if e == "" {
		return nil
	}
	lineas, err := listarCadena(cadenaAcceso)
	if err != nil {
		return err
	}
	vigentes := map[string]bool{}
	for _, r := range permisos {
		for _, v := range [][]string{r, sinSuppl(r)} {
			vigentes[strings.Join(sinPosicion(v, "-D")[3:], " ")] = true
		}
	}
	var errs []error
	for _, l := range lineas {
		f := strings.Fields(l)
		for i := range f {
			f[i] = strings.Trim(f[i], `"`)
		}
		if len(f) < 3 || f[0] != "-A" || f[1] != cadenaAcceso || valorOpcion(f, "--comment") != e {
			continue
		}
		if !vigentes[strings.Join(f[2:], " ")] {
			errs = append(errs, ejecutar(append([]string{"iptables", "-D", cadenaAcceso}, f[2:]...)...))
		}
	}
	return errors.Join(errs...)
}

// listarCadena devuelve `iptables -S cadena`, una regla por línea.
var listarCadena = func(cadena string) ([]string, error) {
	out, err := exec.Command("iptables", "-w", esperaXtables, "-S", cadena).Output()
	if err != nil {
		return nil, fmt.Errorf("iptables -S %s: %w", cadena, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

// aComprobar es la regla con -A convertida en -C.
func aComprobar(r []string) []string {
	out := append([]string{}, r...)
	for i, x := range out {
		if x == "-A" {
			out[i] = "-C"
			break
		}
	}
	return out
}

// HostInputRules son las reglas de la cadena INPUT del host para lo que llega
// desde los namespaces (por los veth vh-*), en el orden en que tienen que
// quedar.
//
// Sin ellas, un invitado con salida (internet o allowlist) llegaba a los
// servicios del propio host atados a 0.0.0.0 —sshd, un gateway, la interfaz de
// Proxmox— por la IP PÚBLICA del host: dentro del namespace el destino no está
// en ningún rango bloqueado, el paquete se enmascara, y en el host el destino es
// local, así que va a INPUT, donde nadie lo paraba. En allowlist bastaba un
// dominio permitido que resolviera a esa IP. La red privada del host ya la
// cortan las reglas del namespace; esto cubre sus IPs públicas.
//
// Pasa solo lo legítimo: las respuestas a conexiones que abre el host (agente,
// exec, carpetas compartidas, resync), el resolver DNS del modo allowlist, el
// proxy de credenciales y los proxies de enlace de los grafos, todos en el
// lado host de cada veth.
//
// Y solo los de SU veth: `-m addrtype --dst-type LOCAL --limit-iface-in` exige
// que el destino sea una dirección del interfaz por el que entra el paquete.
// Con `-d HostSubnet` a secas, la máquina A llegaba al resolver, al proxy de
// credenciales, al de Postgres o a las aristas de la máquina B por la HostIP
// de B (el host es "weak host": entrega en local cualquier dirección suya
// venga por donde venga). Una regla para todas, sin una por veth que montar y
// desmontar.
func HostInputRules() [][]string { return hostInputRules(true) }

// reglasInputViejas son las de versiones anteriores (sin el addrtype). Se
// quitan al arrancar: si se quedaran, sus ACCEPT seguirían abriendo la HostIP
// de cualquier máquina a todas las demás.
func reglasInputViejas() [][]string {
	r := hostInputRules(false)
	return r[1 : len(r)-2] // las respuestas y los dos DROP no han cambiado
}

func hostInputRules(soloSuVeth bool) [][]string {
	base := []string{"iptables", "-I", "INPUT", "1", "-i", "vh-+", "-s", HostSubnet}
	regla := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	servicio := func(proto, puerto string) []string {
		r := regla("-d", HostSubnet)
		if soloSuVeth {
			r = append(r, "-m", "addrtype", "--dst-type", "LOCAL", "--limit-iface-in")
		}
		return append(r, "-p", proto, "--dport", puerto, "-j", "ACCEPT")
	}
	return [][]string{
		regla("-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"),
		servicio("udp", strconv.Itoa(dnsPort)),
		servicio("tcp", strconv.Itoa(dnsPort)),
		servicio("tcp", strconv.Itoa(credPort)),
		servicio("tcp", strconv.Itoa(pgPort)),
		servicio("tcp", strconv.Itoa(myPort)),
		// Los proxies de enlace de los grafos (enlaces_fc.go), también en el
		// lado host de cada veth.
		servicio("tcp", fmt.Sprintf("%d:%d", linkBasePortRango, linkBasePortRango+linkMaxPortsRango-1)),
		regla("-j", "DROP"),
		// Los lados host de los veth desde fuera del host (la LAN, un
		// contenedor): nada legítimo llega así. Lo del propio host entra por
		// lo con origen local, y eso lo filtra reglasAcceso en OUTPUT.
		{"iptables", "-I", "INPUT", "1", "!", "-i", "vh-+", "-d", HostSubnet,
			"-m", "addrtype", "!", "--src-type", "LOCAL", "-j", "DROP"},
	}
}

// sinPosicion es la regla con -I INPUT 1 convertida a la acción dada (-C para
// comprobarla, -D para borrarla), que no llevan posición.
func sinPosicion(r []string, accion string) []string {
	out := make([]string, 0, len(r))
	for i := 0; i < len(r); i++ {
		if r[i] == "-I" {
			out = append(out, accion, r[i+1])
			i += 2 // la cadena ya va; se salta la posición
			continue
		}
		out = append(out, r[i])
	}
	return out
}

// SetupHost prepara el host una sola vez: reenvío y reglas de barrera.
// acceso son los usuarios y grupos que, además del daemon, pueden abrir
// conexiones al agente de las máquinas (reglasAcceso).
//
// Si algo falla sigue con lo demás (cada barrera que entra cierra algo) y
// devuelve el error. Mientras no haya entrado todo, Setup no monta la red de
// ninguna máquina: lo reintenta antes de cada una (asegurarHostInput) y, si
// sigue fallando, la máquina no arranca. Antes el daemon seguía con un
// WARNING y las máquinas salían con el host abierto.
func SetupHost(acceso AccesoAgente) error {
	accesoPedido.Store(&acceso)
	err := errors.Join(
		activarReenvio(),
		montarEgresoHost(),
		setupHostInput(),
		setupAcceso(acceso, os.Geteuid(), os.Getegid()),
	)
	if err != nil {
		inputMontado.Store(false)
		return err
	}
	accesoMontado.Store(&acceso)
	inputMontado.Store(true)
	if b, err := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_max"); err == nil {
		if aviso := avisoConntrack(string(b)); aviso != "" {
			log.Print(aviso)
		}
	}
	return nil
}

// activarReenvio enciende ip_forward en el host. Sustituible en tests.
var activarReenvio = func() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644)
}

// ErrHostSinBarreras: SetupHost no ha podido montar las reglas del host, y
// sin ellas ninguna máquina recibe red.
var ErrHostSinBarreras = errors.New("the host firewall rules for the machines are not installed")

// accesoPedido es el AccesoAgente con el que se llamó a SetupHost, haya ido
// bien o no: con él se reintenta.
var accesoPedido atomic.Pointer[AccesoAgente]

// accesoMontado es el AccesoAgente con el que SetupHost montó reglasAcceso,
// para reponerlas igual (asegurarHostInput).
var accesoMontado atomic.Pointer[AccesoAgente]

// inputMontado dice que SetupHost puso todas las reglas del host: desde
// entonces, cada Setup comprueba que siguen ahí (asegurarHostInput).
var inputMontado atomic.Bool

// inputMu evita que dos Setup a la vez repongan las reglas los dos.
var inputMu sync.Mutex

// centinelas son las comprobaciones de las reglas del host cuya falta delata
// un reload del cortafuegos (`ufw reload`, `firewalld --reload`), una por
// juego: la primera regla de INPUT (la última que entra: si una reposición
// se quedó a medias, falta), el salto de OUTPUT a cadenaAcceso, el REJECT del
// agente dentro de esa cadena, la última DROP de FORWARD y la del agente en
// mangle. El salto y la de mangle, además, en su sitio: la primera de su
// cadena (ponerPrimera).
func centinelas(acceso AccesoAgente, snap *instantaneaTablas) []func() (bool, error) {
	reglas := HostInputRules()
	_, cierre, salto := reglasAcceso(acceso, os.Geteuid(), os.Getegid())
	// Con la instantánea, "está" se da por bueno solo si la regla sale en
	// ella; si no sale (o no hay instantánea), se pregunta al iptables de
	// siempre: la copia solo ahorra procesos cuando todo está en su sitio, y
	// nunca da por puesta una regla que no lo está.
	comprobar := func(r []string) func() (bool, error) {
		return func() (bool, error) {
			if snap.tiene(r) {
				return true, nil
			}
			return comprobarEstado(paraComprobar(r))
		}
	}
	primera := func(r []string) func() (bool, error) {
		return func() (bool, error) {
			if snap.esPrimera(r) {
				return true, nil
			}
			return esPrimera(r)
		}
	}
	return []func() (bool, error){
		comprobar(reglas[0]),
		primera(salto),
		comprobar(cierre[0]),
		comprobar(centinelaForward()),
		primera(reglaAgenteDesdeFuera()),
	}
}

// instantaneaTablas es la salida de UN iptables-save: tabla → cadena → reglas
// (la especificación tras "-A CADENA", normalizada) en orden.
type instantaneaTablas map[string]map[string][][]string

// guardarTablas lee las tablas del host con un solo proceso. Sustituible en
// tests. Sin -w: iptables-save no espera el cerrojo en todas las versiones, y
// un fallo solo lleva al camino lento.
var guardarTablas = func() (string, error) {
	out, err := exec.Command("iptables-save").Output()
	return string(out), err
}

func leerInstantanea() *instantaneaTablas {
	out, err := guardarTablas()
	if err != nil || out == "" {
		return nil
	}
	snap := instantaneaTablas{}
	tabla := ""
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 1 && strings.HasPrefix(f[0], "*"):
			tabla = f[0][1:]
		case len(f) >= 2 && f[0] == "-A" && tabla != "":
			if snap[tabla] == nil {
				snap[tabla] = map[string][][]string{}
			}
			snap[tabla][f[1]] = append(snap[tabla][f[1]], normalizarRegla(f[2:]))
		}
	}
	return &snap
}

// destinoRegla separa una regla "-A CADENA espec" o "-I CADENA [n] espec"
// (con su -t si no es filter) en tabla, cadena y especificación.
func destinoRegla(r []string) (tabla, cadena string, espec []string) {
	if slices.Contains(r, "-I") {
		return partirRegla(r)
	}
	a := r[1:]
	tabla = "filter"
	if len(a) > 1 && a[0] == "-t" {
		tabla, a = a[1], a[2:]
	}
	return tabla, a[1], a[2:]
}

// tiene dice si la regla r sale en la instantánea. Una instantánea nil nunca
// la tiene.
func (s *instantaneaTablas) tiene(r []string) bool {
	if s == nil {
		return false
	}
	tabla, cadena, espec := destinoRegla(r)
	quiere := normalizarRegla(espec)
	for _, o := range (*s)[tabla][cadena] {
		if slices.Equal(o, quiere) {
			return true
		}
	}
	return false
}

// esPrimera dice si r es la primera regla de su cadena en la instantánea.
func (s *instantaneaTablas) esPrimera(r []string) bool {
	if s == nil {
		return false
	}
	tabla, cadena, espec := destinoRegla(r)
	reglas := (*s)[tabla][cadena]
	return len(reglas) > 0 && slices.Equal(reglas[0], normalizarRegla(espec))
}

// vueloHost junta las comprobaciones del host: una que EMPIEZA después de que
// llegara mi petición me vale (vió lo que había cuando yo llegué o más
// tarde), pero una que ya estaba en marcha no, porque pudo leer antes de un
// reload. Así N peticiones simultáneas comparten una sola comprobación nueva
// (como mucho dos en total: la que ya corría y la siguiente), y ninguna se
// queda con un resultado anterior a su llegada. No es una caché por tiempo.
type vueloHost struct {
	mu        sync.Mutex
	corriendo bool
	siguiente *rondaHost // la que arrancará al acabar la actual
}

type rondaHost struct {
	hecha chan struct{}
	err   error
}

func (v *vueloHost) hacer(f func() error) error {
	v.mu.Lock()
	if !v.corriendo {
		v.corriendo = true
		r := &rondaHost{hecha: make(chan struct{})}
		v.mu.Unlock()
		go v.bucle(r, f)
		<-r.hecha
		return r.err
	}
	if v.siguiente == nil {
		v.siguiente = &rondaHost{hecha: make(chan struct{})}
	}
	r := v.siguiente
	v.mu.Unlock()
	<-r.hecha
	return r.err
}

// bucle ejecuta rondas mientras haya peticiones que no cubra la actual.
func (v *vueloHost) bucle(r *rondaHost, f func() error) {
	for {
		func() {
			defer close(r.hecha)
			defer func() {
				if p := recover(); p != nil {
					r.err = fmt.Errorf("host check panicked: %v", p)
				}
			}()
			r.err = f()
		}()
		v.mu.Lock()
		r = v.siguiente
		v.siguiente = nil
		if r == nil {
			v.corriendo = false
			v.mu.Unlock()
			return
		}
		v.mu.Unlock()
	}
}

var vuelo vueloHost

// AsegurarHost es asegurarHostInput para quien no monta una red: el
// vigilante, con las máquinas ya en marcha, y el thaw que conserva la red del
// freeze. Sin él, tras un reload del cortafuegos las máquinas vivas seguían
// sin barreras hasta que otra montara la suya. Si nadie pidió SetupHost (sin
// red en el host, macOS), no hay nada que asegurar.
func AsegurarHost() error {
	if accesoPedido.Load() == nil {
		return nil
	}
	return asegurarHostInput()
}

// asegurarHostInput repone las reglas del host si un reload del cortafuegos
// las borró después de SetupHost: sin ellas, un invitado con salida llega a
// los servicios del host por su IP pública, y un contenedor o un usuario
// local, al agente. Mira un centinela por juego (centinelas), los cinco a
// la vez, y reinstala el juego al que le falta el suyo.
//
// Si SetupHost no llegó a montarlo todo, lo reintenta; si tampoco puede,
// devuelve el error y Setup no monta la red. Una reposición que falla a
// medias cuenta igual: la siguiente vuelve a montarlo todo con SetupHost.
func asegurarHostInput() error {
	return vuelo.hacer(comprobarYReponerHost)
}

func comprobarYReponerHost() error {
	if !inputMontado.Load() {
		inputMu.Lock()
		defer inputMu.Unlock()
		if inputMontado.Load() {
			return nil
		}
		a := accesoPedido.Load()
		if a == nil {
			return ErrHostSinBarreras
		}
		if err := SetupHost(*a); err != nil {
			return fmt.Errorf("%w: %v", ErrHostSinBarreras, err)
		}
		log.Printf("net: the host firewall rules for the machines are now installed")
		return nil
	}
	acceso := AccesoAgente{}
	if a := accesoMontado.Load(); a != nil {
		acceso = *a
	}
	estan, err := comprobarALaVez(centinelas(acceso, leerInstantanea()))
	if err != nil {
		return err
	}
	if !slices.Contains(estan, false) {
		return nil
	}
	inputMu.Lock()
	defer inputMu.Unlock()
	if err := reponerHost(estan, acceso); err != nil {
		inputMontado.Store(false)
		return fmt.Errorf("%w: %v", ErrHostSinBarreras, err)
	}
	return nil
}

// reponerHost reinstala los juegos de reglas cuyo centinela falta (estan, en
// el orden de centinelas).
func reponerHost(estan []bool, acceso AccesoAgente) error {
	if !estan[0] {
		log.Printf("net: the host INPUT rules for the machines are gone (a firewall reload?); reinstalling them")
		if err := setupHostInput(); err != nil {
			return err
		}
	}
	if !estan[1] || !estan[2] {
		log.Printf("net: the host OUTPUT rules for the machines are gone or out of place (a firewall reload?); reinstalling them")
		if err := setupAcceso(acceso, os.Geteuid(), os.Getegid()); err != nil {
			return err
		}
	}
	if !estan[3] || !estan[4] {
		log.Printf("net: the host FORWARD rules for the machines are gone or out of place (a firewall reload?); reinstalling them")
		return montarEgresoHost()
	}
	return nil
}

// comprobarALaVez lanza las comprobaciones en paralelo: son procesos
// independientes que solo leen, y así cuestan lo que el más lento y no la
// suma.
func comprobarALaVez(comprobaciones []func() (bool, error)) ([]bool, error) {
	estan := make([]bool, len(comprobaciones))
	errs := make([]error, len(comprobaciones))
	var wg sync.WaitGroup
	for i, c := range comprobaciones {
		wg.Add(1)
		go func() {
			defer wg.Done()
			estan[i], errs[i] = c()
		}()
	}
	wg.Wait()
	return estan, errors.Join(errs...)
}

// setupHostInput instala HostInputRules al principio de INPUT, delante de lo
// que tenga el host (un ACCEPT de un cortafuegos local no debe adelantarse al
// DROP). Si ya están todas, no se toca nada; si falta alguna, se quitan las que
// haya y se insertan de nuevo en orden, para no dejar el DROP delante de los
// ACCEPT.
func setupHostInput() error {
	quitadas := 0
	quitar := func(r []string) {
		for comprobarRegla(sinPosicion(r, "-C")) {
			if err := ejecutar(sinPosicion(r, "-D")...); err != nil {
				break
			}
			quitadas++
		}
	}
	for _, r := range reglasInputViejas() {
		quitar(r)
	}
	reglas := HostInputRules()
	// -C dice si una regla está, no dónde. Si había reglas viejas, otra
	// versión del daemon reinsertó las suyas arriba —con el DROP, que es el
	// mismo— y las nuestras pueden haber quedado detrás de él: se reinsertan
	// todas, en orden, aunque estén.
	todas := quitadas == 0
	for _, r := range reglas {
		if !todas {
			break
		}
		todas = comprobarRegla(sinPosicion(r, "-C"))
	}
	if todas {
		return nil
	}
	for _, r := range reglas {
		quitar(r)
	}
	// -I INPUT 1 en orden inverso: la primera de la lista acaba la primera.
	for i := len(reglas) - 1; i >= 0; i-- {
		if err := ejecutar(reglas[i]...); err != nil {
			return err
		}
	}
	return nil
}

// comprobarEstado es comprobarIptables detrás de una variable, para los tests.
var comprobarEstado = comprobarIptables

// comprobarRegla dice si la regla está; un error cuenta como que no.
func comprobarRegla(args []string) bool {
	ok, _ := comprobarEstado(args)
	return ok
}

// reglasEnlaceSobrantes recibe lo que lista `iptables -S` en el netns (nat
// PREROUTING y FORWARD) y devuelve, listas para pasar a iptables, las -D de las
// reglas de enlace (las de reglasEnlace) que no corresponden a puertos, el puerto de
// cada arista en el orden de sus slots. Una regla
// de enlace es la que va a n.HostIP:<puerto del rango de enlaces>: el DNAT que
// lleva ahí el puerto de la arista y el ACCEPT de ese puerto. Las demás (DNS,
// credenciales, egreso) no se tocan.
func (n *Net) reglasEnlaceSobrantes(nat, forward []string, puertos []int) [][]string {
	// Puerto de la arista -> slot, y slots en uso.
	quiero := make(map[int]int, len(puertos))
	for slot, p := range puertos {
		quiero[p] = slot
	}
	slotDe := func(p int) (int, bool) {
		if p < linkBasePortRango || p >= linkBasePortRango+linkMaxPortsRango {
			return 0, false
		}
		return p - linkBasePortRango, true
	}
	esHostIP := func(d string) bool { return d == n.HostIP || d == n.HostIP+"/32" }

	var sobran [][]string
	for _, linea := range nat {
		f := strings.Fields(linea)
		if len(f) < 2 || f[0] != "-A" || f[1] != "PREROUTING" || valorOpcion(f, "-j") != "DNAT" {
			continue
		}
		host, puerto, err := stdnet.SplitHostPort(valorOpcion(f, "--to-destination"))
		if err != nil || host != n.HostIP {
			continue
		}
		p, err := strconv.Atoi(puerto)
		if err != nil {
			continue
		}
		slot, ok := slotDe(p)
		if !ok {
			continue
		}
		dport, err := strconv.Atoi(valorOpcion(f, "--dport"))
		if s, sigue := quiero[dport]; err == nil && sigue && s == slot {
			continue
		}
		sobran = append(sobran, borrarLineaS([]string{"-t", "nat"}, f))
	}
	for _, linea := range forward {
		f := strings.Fields(linea)
		if len(f) < 2 || f[0] != "-A" || f[1] != "FORWARD" || valorOpcion(f, "-j") != "ACCEPT" || !esHostIP(valorOpcion(f, "-d")) {
			continue
		}
		p, err := strconv.Atoi(valorOpcion(f, "--dport")) // un rango (a:b) no es de un enlace
		if err != nil {
			continue
		}
		if slot, ok := slotDe(p); !ok || slot < len(puertos) {
			continue
		}
		sobran = append(sobran, borrarLineaS(nil, f))
	}
	return sobran
}

// valorOpcion devuelve el argumento que sigue a opt en una línea de
// `iptables -S` ya partida, o "".
func valorOpcion(f []string, opt string) string {
	for i := 0; i+1 < len(f); i++ {
		if f[i] == opt {
			return f[i+1]
		}
	}
	return ""
}

// borrarLineaS convierte una línea de -S (-A cadena ...) en su -D, con pre
// (p. ej. "-t", "nat") delante.
func borrarLineaS(pre []string, f []string) []string {
	out := append(append([]string{}, pre...), "-D")
	return append(out, f[1:]...)
}

// reglasCredSobrantes es reglasEnlaceSobrantes para las aristas credential:
// recibe lo que lista `iptables -S` en el netns (nat PREROUTING y FORWARD) y
// devuelve las -D de las reglas de los proxies de credenciales (las de
// credNATRules y credForwardRules) que haya. SetGraph la usa cuando el nodo ya
// no tiene aristas credential en egress none o internet: sin quitarlas, el
// DNAT de todo el TCP hacia n.HostIP seguiría llevando al invitado a un proxy
// que ya no le sirve, y con otra arista que reutilice el puerto quedaría
// tapada. Se reconocen por el destino, no por el texto: el DNAT va a
// n.HostIP:<credPort|pgPort|myPort> y el ACCEPT a n.HostIP con uno de esos
// puertos de destino. Las de DNS y enlaces van a otros puertos.
func (n *Net) reglasCredSobrantes(nat, forward []string) [][]string {
	esCred := func(p string) bool { return p == credPortStr || p == pgPortStr || p == myPortStr }
	esHostIP := func(d string) bool { return d == n.HostIP || d == n.HostIP+"/32" }

	var sobran [][]string
	for _, linea := range nat {
		f := strings.Fields(linea)
		if len(f) < 2 || f[0] != "-A" || f[1] != "PREROUTING" || valorOpcion(f, "-j") != "DNAT" {
			continue
		}
		host, puerto, err := stdnet.SplitHostPort(valorOpcion(f, "--to-destination"))
		if err != nil || host != n.HostIP || !esCred(puerto) {
			continue
		}
		sobran = append(sobran, borrarLineaS([]string{"-t", "nat"}, f))
	}
	for _, linea := range forward {
		f := strings.Fields(linea)
		if len(f) < 2 || f[0] != "-A" || f[1] != "FORWARD" || valorOpcion(f, "-j") != "ACCEPT" ||
			!esHostIP(valorOpcion(f, "-d")) || !esCred(valorOpcion(f, "--dport")) {
			continue
		}
		sobran = append(sobran, borrarLineaS(nil, f))
	}
	return sobran
}
