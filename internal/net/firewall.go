package net

import (
	"context"
	"fmt"
	"log"
	stdnet "net"
	"os/exec"
	"strconv"
	"strings"

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
// TODO(allowlist): esto es un resolver recursivo público, no uno propio. Un
// invitado decidido todavía puede filtrar datos codificándolos en subdominios
// que este resolver reenvía a un NS autoritativo del atacante (DNS tunneling).
// Cerrar ese hueco pide un resolver del host que SOLO responda por los dominios
// permitidos —y que de paso siembre el ipset con lo que resuelva—; ese es el
// resolver dinámico que aún falta (ver applyAllowlist).
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
}

// applyEgress instala las reglas de salida dentro del namespace. domains solo se
// usa en modo allowlist; en el resto se ignora.
func (n *Net) applyEgress(e Egress, domains []string) error {
	ns := func(args ...string) error {
		return run(append([]string{"ip", "netns", "exec", n.NS}, args...)...)
	}

	// La barrera IPv6 (applyIPv6Barrier, más abajo en este fichero) NO va aquí:
	// vive en netns_fc.go/Setup, antes de la rama rápida de iptables-restore que
	// evita esta función para none/internet. Si viviera solo aquí se saltaría en
	// ese camino, que es el habitual cuando iptables-restore está instalado.

	// PRIMERO: dejar pasar las respuestas a conexiones ya establecidas.
	//
	// Sin esto se rompe el acceso host->microVM, porque nuestra propia red de
	// enlace (172.30.0.0/16) cae DENTRO de 172.16.0.0/12, que está en la lista de
	// bloqueo. La regla descartaría las respuestas del invitado al host.
	//
	// No abre ningún agujero: una conexión que el invitado INICIE hacia una red
	// privada es ctstate NEW y sigue cayendo en los DROP de abajo.
	if err := ns("iptables", "-A", "FORWARD", "-i", TapName,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return err
	}

	if e == EgressNone {
		// Se descarta todo lo que intente salir por el veth. El invitado sigue
		// pudiendo responder a lo que le llega, porque eso no cruza FORWARD como
		// tráfico nuevo iniciado por él.
		if err := ns("iptables", "-A", "FORWARD", "-i", TapName, "-o", n.NSIf,
			"-m", "conntrack", "--ctstate", "NEW", "-j", "DROP"); err != nil {
			return err
		}
		return nil
	}

	if e == EgressAllowlist {
		return n.applyAllowlist(ns, domains)
	}

	// EgressInternet: primero se cierran las redes privadas, luego se permite el resto.
	// El orden importa: la primera regla que casa, gana.
	for _, cidr := range blocked {
		if err := ns("iptables", "-A", "FORWARD", "-i", TapName, "-d", cidr, "-j", "DROP"); err != nil {
			return err
		}
	}
	return ns("iptables", "-t", "nat", "-A", "POSTROUTING", "-o", n.NSIf, "-j", "MASQUERADE")
}

// applyIPv6Barrier cierra el paso a IPv6 dentro del namespace, en allowlist,
// internet Y none: el filtrado de este fichero (ipset, iptables) es solo
// IPv4, así que sin esto un invitado con salida v6 real (si algún día el host
// reenvía v6, a diferencia de lo medido hoy en el lab) se saltaría el
// allowlist entero por ese camino, y en modo internet o none tendría una
// salida que ningún DROP de aquí cubre. Dos capas independientes, ninguna
// necesita que la otra exista:
//
//  1. sysctl disable_ipv6=1 en tap0, en el veth del namespace y en "all"/
//     "default": la interfaz del invitado no adquiere ninguna dirección v6
//     (ni siquiera link-local) y el kernel del namespace no la enruta. Es la
//     barrera real: no depende de que ip6tables esté instalado.
//  2. ip6tables FORWARD DROP para lo que entre por tap0: cinturón además de
//     tirantes, para el caso de una interfaz futura que se sumara al
//     namespace sin pasar por el paso 1. Si ip6tables no está instalado (no
//     es una dependencia dura de kindling hoy) se avisa por log y se sigue
//     sin fallar: la capa 1 ya cierra el hueco que importa.
//
// Complementa, no sustituye, a ipv6.disable=1 en la línea de arranque del
// invitado (net.go/BootArg): esa apaga el módulo v6 DENTRO del invitado, pero
// solo en un arranque en frío —un snapshot dorado ya congelado no la relee—.
// Esta barrera vive en el namespace del HOST y cubre también esos snapshots.
func (n *Net) applyIPv6Barrier(ns func(...string) error) error {
	for _, iface := range []string{"all", "default", TapName, n.NSIf} {
		clave := fmt.Sprintf("net.ipv6.conf.%s.disable_ipv6=1", iface)
		if err := ns("sysctl", "-w", clave); err != nil {
			// No es fatal: si el namespace no tiene /proc/sys/net/ipv6 (kernel
			// compilado sin IPv6, por ejemplo) no hay nada que apagar y la capa
			// 2 sigue siendo la barrera. Pero si sysctl SÍ existe y falla por
			// otra razón, conviene que quede en el log del daemon.
			log.Printf("net: aviso, no se pudo aplicar %s en %s: %v", clave, n.NS, err)
		}
	}

	if _, err := exec.LookPath("ip6tables"); err != nil {
		log.Printf("net: ip6tables no está instalado; %s queda solo con la barrera de sysctl para IPv6", n.NS)
		return nil
	}
	return ns("ip6tables", "-A", "FORWARD", "-i", TapName, "-j", "DROP")
}

// egressRules son las reglas de salida de none e internet, en el mismo orden
// que las pone applyEgress, como argumentos de iptables (con -t si no es
// filter). Ver applyEgress para el porqué de cada una.
func (n *Net) egressRules(e Egress) [][]string {
	rules := [][]string{{"-A", "FORWARD", "-i", TapName,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"}}
	if e == EgressNone {
		return append(rules, []string{"-A", "FORWARD", "-i", TapName, "-o", n.NSIf,
			"-m", "conntrack", "--ctstate", "NEW", "-j", "DROP"})
	}
	for _, cidr := range blocked {
		rules = append(rules, []string{"-A", "FORWARD", "-i", TapName, "-d", cidr, "-j", "DROP"})
	}
	return append(rules, []string{"-t", "nat", "-A", "POSTROUTING", "-o", n.NSIf, "-j", "MASQUERADE"})
}

// restoreRules añade reglas dentro del namespace con un solo iptables-restore
// --noflush: todas o ninguna, y un proceso en vez de uno por regla.
func (n *Net) restoreRules(rules [][]string) error {
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
	cmd := exec.Command("ip", "netns", "exec", n.NS, "iptables-restore", "--noflush")
	cmd.Stdin = strings.NewReader(b.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables-restore in %s: %v: %s", n.NS, err, strings.TrimSpace(string(out)))
	}
	return nil
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
func (n *Net) applyAllowlist(ns func(...string) error, domains []string) error {
	if _, err := exec.LookPath("ipset"); err != nil {
		return fmt.Errorf("allowlist mode needs ipset and it is not installed: %w", err)
	}

	set := n.setName()
	// hash:ip con la opción timeout ACTIVADA (el "timeout 0" del create solo fija
	// el defecto en "permanente"; lo que importa es que habilita el timeout por
	// entrada). El resolver dinámico añade cada IP con "timeout <ttl-real>", así
	// que caduca sola cuando expira el registro. El sembrado estático de abajo va
	// sin timeout (permanente): es la red de seguridad.
	if err := ns("ipset", "create", set, "hash:ip", "timeout", "0", "-exist"); err != nil {
		return err
	}

	// Sembrado estático: resolvemos en el host, por el MISMO resolver al que
	// forzaremos al invitado, y metemos solo IPv4 públicas. Las privadas se
	// descartan aquí para que un resolver envenenado no cuele 169.254 (metadatos)
	// ni 192.168 (LAN de casa) en la lista de permitidos.
	for _, d := range domains {
		for _, ip := range resolvePublicIPv4(d) {
			_ = ns("ipset", "add", set, ip, "-exist")
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
		if err := ns("iptables", "-t", "nat", "-A", "PREROUTING", "-i", TapName,
			"-p", proto, "--dport", "53", "-j", "DNAT",
			"--to-destination", dnsTarget); err != nil {
			return err
		}
	}

	// Proxy de credenciales (credproxy.go): el resolver contesta con n.HostIP para
	// un dominio con credencial y el invitado conecta a él; los DNAT de
	// credNATRules llevan el 80/443 al proxy HTTP y el resto de puertos TCP al
	// de Postgres. Si la máquina no tiene credenciales no escucha nadie y la
	// conexión se rechaza: las reglas no abren nada que no sean los proxies.
	for _, r := range n.credNATRules() {
		if err := ns(r...); err != nil {
			return err
		}
	}

	// FORWARD, en orden (la primera que casa gana):
	//   1. DNS hacia nuestro resolver del host: permitido. Tras el DNAT el destino
	//      es n.HostIP:dnsPort (el lado host del veth), así que el paquete cruza el
	//      FORWARD del netns antes de llegar al resolver. Lo mismo los proxies
	//      de credenciales en n.HostIP:credPort y n.HostIP:pgPort.
	dnsPortStr := fmt.Sprintf("%d", dnsPort)
	for _, proto := range []string{"udp", "tcp"} {
		if err := ns("iptables", "-A", "FORWARD", "-i", TapName, "-o", n.NSIf,
			"-p", proto, "-d", n.HostIP, "--dport", dnsPortStr, "-j", "ACCEPT"); err != nil {
			return err
		}
	}
	for _, r := range n.credForwardRules() {
		if err := ns(r...); err != nil {
			return err
		}
	}
	//   2. Conexiones NUEVAS cuya IP destino esté en el ipset: permitidas.
	if err := ns("iptables", "-A", "FORWARD", "-i", TapName, "-o", n.NSIf,
		"-m", "conntrack", "--ctstate", "NEW",
		"-m", "set", "--match-set", set, "dst", "-j", "ACCEPT"); err != nil {
		return err
	}
	//   3. Todo lo demás que el invitado inicie: DROP. Esto bloquea IP directa,
	//      dominios no listados y cualquier otro puerto/destino.
	if err := ns("iptables", "-A", "FORWARD", "-i", TapName, "-o", n.NSIf,
		"-m", "conntrack", "--ctstate", "NEW", "-j", "DROP"); err != nil {
		return err
	}

	// El host enmascara hacia internet y vuelve a bloquear las redes privadas como
	// segunda barrera (HostEgressRules), igual que en modo internet.
	if err := ns("iptables", "-t", "nat", "-A", "POSTROUTING", "-o", n.NSIf, "-j", "MASQUERADE"); err != nil {
		return err
	}

	// Arrancar el resolver dinámico del host para este netns. Debe quedar vivo
	// ANTES de devolver: es quien siembra el ipset con las IP reales (y su TTL) que
	// el invitado va a usar. Si no arranca, el DNAT de arriba apunta a un puerto sin
	// nadie escuchando y el invitado se queda sin DNS, así que su fallo es fatal.
	return startDNSResolver(n, domains)
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
func resolvePublicIPv4(domain string) []string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return nil
	}
	return credproxy.LookupPublicIPv4(context.Background(), DNSResolver+":53", domain)
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

// HostEgressRules son las reglas que el host necesita para dar salida a los
// namespaces. Se instalan una sola vez, no por máquina.
//
// Se aplican como segunda barrera: aunque alguien manipulara las reglas de un
// namespace, el host sigue negando el acceso a la red privada.
func HostEgressRules(bridge string) [][]string {
	var rules [][]string
	for _, cidr := range blocked {
		if cidr == "172.16.0.0/12" {
			// En el host hay que dejar pasar el propio rango de kindling, o se
			// cortaría el tráfico legítimo entre host y microVMs.
			continue
		}
		rules = append(rules, []string{
			"iptables", "-A", "FORWARD", "-s", HostSubnet, "-d", cidr, "-j", "DROP",
		})
	}
	rules = append(rules,
		[]string{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", HostSubnet, "!", "-d", HostSubnet, "-j", "MASQUERADE"},
	)
	return rules
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
func HostInputRules() [][]string {
	base := []string{"iptables", "-I", "INPUT", "1", "-i", "vh-+", "-s", HostSubnet}
	regla := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	return [][]string{
		regla("-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"),
		regla("-d", HostSubnet, "-p", "udp", "--dport", strconv.Itoa(dnsPort), "-j", "ACCEPT"),
		regla("-d", HostSubnet, "-p", "tcp", "--dport", strconv.Itoa(dnsPort), "-j", "ACCEPT"),
		regla("-d", HostSubnet, "-p", "tcp", "--dport", strconv.Itoa(credPort), "-j", "ACCEPT"),
		regla("-d", HostSubnet, "-p", "tcp", "--dport", strconv.Itoa(pgPort), "-j", "ACCEPT"),
		regla("-d", HostSubnet, "-p", "tcp", "--dport", strconv.Itoa(myPort), "-j", "ACCEPT"),
		// Los proxies de enlace de los grafos (enlaces_fc.go), también en el
		// lado host de cada veth.
		regla("-d", HostSubnet, "-p", "tcp", "--dport", fmt.Sprintf("%d:%d", linkBasePortRango, linkBasePortRango+linkMaxPortsRango-1), "-j", "ACCEPT"),
		regla("-j", "DROP"),
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
func SetupHost() error {
	if err := run("sh", "-c", "echo 1 > /proc/sys/net/ipv4/ip_forward"); err != nil {
		return err
	}
	for _, r := range HostEgressRules("") {
		// -C comprueba si la regla ya existe, para no duplicarla en cada arranque.
		check := append([]string{}, r...)
		for i, a := range check {
			if a == "-A" {
				check[i] = "-C"
			}
		}
		if exec_ok(check) {
			continue
		}
		if err := run(r...); err != nil {
			return err
		}
	}
	return setupHostInput()
}

// setupHostInput instala HostInputRules al principio de INPUT, delante de lo
// que tenga el host (un ACCEPT de un cortafuegos local no debe adelantarse al
// DROP). Si ya están todas, no se toca nada; si falta alguna, se quitan las que
// haya y se insertan de nuevo en orden, para no dejar el DROP delante de los
// ACCEPT.
func setupHostInput() error {
	reglas := HostInputRules()
	todas := true
	for _, r := range reglas {
		if !exec_ok(sinPosicion(r, "-C")) {
			todas = false
			break
		}
	}
	if todas {
		return nil
	}
	for _, r := range reglas {
		for exec_ok(sinPosicion(r, "-C")) {
			if err := run(sinPosicion(r, "-D")...); err != nil {
				break
			}
		}
	}
	// -I INPUT 1 en orden inverso: la primera de la lista acaba la primera.
	for i := len(reglas) - 1; i >= 0; i-- {
		if err := run(reglas[i]...); err != nil {
			return err
		}
	}
	return nil
}
