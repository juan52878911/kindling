package net

import "fmt"

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
)

func ParseEgress(s string) (Egress, error) {
	switch Egress(s) {
	case EgressNone, EgressInternet:
		return Egress(s), nil
	case "":
		return EgressNone, nil
	default:
		return "", fmt.Errorf("política de salida desconocida: %q (usa none o internet)", s)
	}
}

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

// applyEgress instala las reglas de salida dentro del namespace.
func (n *Net) applyEgress(e Egress) error {
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

	// EgressInternet: primero se cierran las redes privadas, luego se permite el resto.
	// El orden importa: la primera regla que casa, gana.
	for _, cidr := range blocked {
		if err := ns("iptables", "-A", "FORWARD", "-i", TapName, "-d", cidr, "-j", "DROP"); err != nil {
			return err
		}
	}
	return ns("iptables", "-t", "nat", "-A", "POSTROUTING", "-o", n.NSIf, "-j", "MASQUERADE")
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
