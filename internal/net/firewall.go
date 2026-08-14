package net

import (
	"context"
	"fmt"
	stdnet "net"
	"os/exec"
	"strings"
	"time"
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
//   - AAAA / IPv6: el resolver solo siembra A (IPv4); el ipset es v4. Ver extractA.
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

	// FORWARD, en orden (la primera que casa gana):
	//   1. DNS hacia nuestro resolver del host: permitido. Tras el DNAT el destino
	//      es n.HostIP:dnsPort (el lado host del veth), así que el paquete cruza el
	//      FORWARD del netns antes de llegar al resolver.
	dnsPortStr := fmt.Sprintf("%d", dnsPort)
	for _, proto := range []string{"udp", "tcp"} {
		if err := ns("iptables", "-A", "FORWARD", "-i", TapName, "-o", n.NSIf,
			"-p", proto, "-d", n.HostIP, "--dport", dnsPortStr, "-j", "ACCEPT"); err != nil {
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

// resolvePublicIPv4 resuelve un dominio a sus IPv4 públicas, usando el mismo
// resolver (DNSResolver) al que se fuerza al invitado, para que lo que sembramos
// coincida con lo que el invitado verá. Devuelve solo direcciones enrutables:
// las privadas/link-local se descartan por seguridad (resolver envenenado).
func resolvePublicIPv4(domain string) []string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return nil
	}
	r := &stdnet.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (stdnet.Conn, error) {
			var d stdnet.Dialer
			return d.DialContext(ctx, network, DNSResolver+":53")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := r.LookupHost(ctx, domain)
	if err != nil {
		return nil // falla CERRADO: sin IP, el dominio simplemente no se permite
	}
	var out []string
	for _, a := range addrs {
		ip := stdnet.ParseIP(a)
		if ip == nil || ip.To4() == nil {
			continue // solo IPv4: las reglas de este fichero son iptables v4
		}
		if isBlockedIP(ip) {
			continue
		}
		out = append(out, ip.String())
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

// HostSubnet es el rango que kindling usa para los enlaces con los namespaces.
const HostSubnet = hostPrefix + ".0.0/16"

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
	return nil
}
