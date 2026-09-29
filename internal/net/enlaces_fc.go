//go:build !darwin

package net

// Aristas de un grafo de microVMs en el HOST (docs/grafos.md): lo que hace
// falta en el netns de un nodo para que llegue a otro nodo SIN abrir la red.
//
// NADA DE RED ENTRE NAMESPACES: el FORWARD entre netns sigue cerrado y ningún
// paquete del invitado A llega nunca al netns de B. Lo que hay es:
//
//   - El resolver del nodo (dnsresolver.go) contesta <b>.graph con la IP del
//     host en el veth de A (n.HostIP) y NXDOMAIN a cualquier otro *.graph. En
//     egress none y internet, donde no había resolver propio, se arranca uno
//     (modoSoloGrafo, modoLibre) y un DNAT le lleva el 53 del invitado.
//   - Por cada arista link (A -> B:P), un DNAT en el netns de A lleva
//     n.HostIP:P a n.HostIP:<puerto de enlace>, donde escucha un proxy de
//     enlace (pkg/credproxy/enlace.go) del daemon. En CADA conexión, el proxy
//     pregunta al manager a dónde va (grafo, nodo, arista, puerto; ver
//     internal/machine/grafo.go) y marca a la IP del netns de B, que es lo
//     que ya hace el host para hablar con cualquier invitado.
//   - Las aristas credential son credenciales Postgres de siempre: su nombre
//     (<b>.graph) es un dominio con credencial y el DNAT del resto de puertos
//     de n.HostIP ya lleva al proxy de Postgres. En none e internet se ponen
//     aquí los DNAT y los ACCEPT que en allowlist pone applyAllowlist.
//
// Las reglas de enlace van con -I 1: delante del DNAT "cualquier otro puerto
// TCP de n.HostIP al proxy de Postgres", que si no se las comería.

import (
	"errors"
	"fmt"
	stdnet "net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// linkBasePort es el primer puerto del host de los proxies de enlace de cada
// microVM (n.HostIP:linkBasePort+i). Uno por arista del nodo: como mucho
// linkMaxPorts, que es api.GraphMaxEdges.
const (
	linkBasePort = linkBasePortRango
	linkMaxPorts = linkMaxPortsRango
)

// LinkSpec es una arista link vista desde el nodo de origen.
type LinkSpec struct {
	// Host es el nombre que resuelve el invitado: <nodo>.graph.
	Host string
	// Port es el puerto al que conecta el invitado (el del destino).
	Port int
	// Target identifica el nodo destino para Invalidar (credproxy.LinkOptions).
	Target string
	// Resolve se llama en cada conexión.
	Resolve credproxy.ResolveLinkFunc
}

// GraphSpec es lo que SetGraph monta en el netns de un nodo.
type GraphSpec struct {
	Egress Egress
	// Domains: los de allowlist, por si hubiera que rearrancar el resolver.
	Domains []string
	Links   []LinkSpec
	// AuditPath es el registro de la máquina (el del proxy de credenciales).
	AuditPath string
}

var (
	grafoMu    sync.Mutex
	grafosRed  = map[string]*grafoRed{} // por netns
	errSinSlot = errors.New("too many links on one node")
)

// grafoRed son los proxies de enlace de un netns, por Host:Port.
type grafoRed struct {
	enlaces map[string]*enlaceVivo
}

type enlaceVivo struct {
	slot int
	ln   *stdnet.TCPListener
	e    *credproxy.Enlace
}

func claveEnlace(l LinkSpec) string { return l.Host + ":" + strconv.Itoa(l.Port) }

// validarLinks comprueba lo que llega del manager antes de tocar reglas.
func validarLinks(links []LinkSpec) error {
	if len(links) > linkMaxPorts {
		return fmt.Errorf("%w: %d (the limit is %d)", errSinSlot, len(links), linkMaxPorts)
	}
	vistos := map[int]bool{}
	for _, l := range links {
		if !strings.HasSuffix(l.Host, "."+graphDomain) || strings.ContainsAny(l.Host, " \t\n/") {
			return fmt.Errorf("link host %q is not a graph name", l.Host)
		}
		switch {
		case l.Port < 1 || l.Port > 65535:
			return fmt.Errorf("link %s: port %d out of range", l.Host, l.Port)
		case l.Port == 53 || l.Port == 80 || l.Port == 443:
			return fmt.Errorf("link %s: port %d is reserved on the graph address", l.Host, l.Port)
		case vistos[l.Port]:
			return fmt.Errorf("two links on port %d from the same node", l.Port)
		case l.Resolve == nil:
			return fmt.Errorf("link %s: no resolver", l.Host)
		}
		vistos[l.Port] = true
	}
	return nil
}

// SetGraph monta (o actualiza, es idempotente) lo que las aristas de un nodo
// necesitan en su netns: resolver, reglas y proxies de enlace. Con Links
// vacío deja el resolver y las reglas del DNS y las credenciales (una arista
// credential sola las necesita) y ningún proxy de enlace.
func SetGraph(n *Net, spec GraphSpec) error {
	links := append([]LinkSpec(nil), spec.Links...)
	sort.Slice(links, func(i, j int) bool { return claveEnlace(links[i]) < claveEnlace(links[j]) })
	if err := validarLinks(links); err != nil {
		return err
	}
	ip := stdnet.ParseIP(n.HostIP).To4()
	if ip == nil {
		return fmt.Errorf("graph: invalid host IP %q", n.HostIP)
	}

	// El resolver: en allowlist ya lo hay (Setup, o reconcile tras un
	// reinicio del daemon); si faltara, se rearranca con sus dominios.
	modo := modoAllowlist
	switch spec.Egress {
	case EgressNone, "":
		modo = modoSoloGrafo
	case EgressInternet:
		modo = modoLibre
	}
	if err := startDNSResolverModo(n, spec.Domains, modo); err != nil {
		return err
	}
	if spec.Egress != EgressAllowlist {
		if err := n.asegurarReglas(n.reglasGrafo(spec.Egress)); err != nil {
			return err
		}
	}
	// El proxy de credenciales: su auditor es el de los enlaces, y sus
	// listeners atienden las aristas credential.
	p, err := startCredProxy(n, spec.AuditPath)
	if err != nil {
		return err
	}

	grafoMu.Lock()
	g := grafosRed[n.NS]
	if g == nil {
		g = &grafoRed{enlaces: map[string]*enlaceVivo{}}
		grafosRed[n.NS] = g
	}
	grafoMu.Unlock()

	var hosts []string
	quedan := map[string]bool{}
	for slot, l := range links {
		hosts = append(hosts, l.Host)
		k := claveEnlace(l)
		quedan[k] = true
		grafoMu.Lock()
		ev := g.enlaces[k]
		grafoMu.Unlock()
		if ev != nil && ev.slot == slot {
			ev.e.SetResolve(l.Resolve)
			if err := n.asegurarReglas(n.reglasEnlace(l.Port, slot)); err != nil {
				return err
			}
			continue
		}
		if ev != nil {
			cerrarEnlace(g, k)
		}
		ln, err := stdnet.ListenTCP("tcp4", &stdnet.TCPAddr{IP: ip, Port: linkBasePort + slot})
		if err != nil {
			return fmt.Errorf("graph link %s: could not listen on %s:%d: %w", l.Host, n.HostIP, linkBasePort+slot, err)
		}
		e := credproxy.NuevoEnlace(credproxy.LinkOptions{
			Name:     k,
			Target:   l.Target,
			Resolve:  l.Resolve,
			MaxConns: maxConnsEnlace,
			Auditor:  p.proxy.Auditor(),
			// Sin Logf: cada fallo ya queda en la auditoría de la máquina, y
			// un invitado que reintenta sin parar llenaría el log del daemon.
		})
		go func() { _ = e.Serve(ln) }()
		grafoMu.Lock()
		g.enlaces[k] = &enlaceVivo{slot: slot, ln: ln, e: e}
		grafoMu.Unlock()
		if err := n.asegurarReglas(n.reglasEnlace(l.Port, slot)); err != nil {
			return err
		}
	}
	grafoMu.Lock()
	var sobran []string
	for k := range g.enlaces {
		if !quedan[k] {
			sobran = append(sobran, k)
		}
	}
	grafoMu.Unlock()
	for _, k := range sobran {
		cerrarEnlace(g, k)
	}

	resolversMu.Lock()
	r := resolvers[n.NS]
	resolversMu.Unlock()
	if r == nil {
		return errors.New("graph: the node has no resolver")
	}
	r.setGraphHosts(hosts, ip)
	return nil
}

// maxConnsEnlace es api.GraphMaxConnsPerEdge.
const maxConnsEnlace = 16

func cerrarEnlace(g *grafoRed, k string) {
	grafoMu.Lock()
	ev := g.enlaces[k]
	delete(g.enlaces, k)
	grafoMu.Unlock()
	if ev != nil {
		_ = ev.e.Close()
	}
}

// stopGraph cierra los proxies de enlace de un netns (Teardown).
func stopGraph(ns string) {
	grafoMu.Lock()
	g := grafosRed[ns]
	delete(grafosRed, ns)
	grafoMu.Unlock()
	if g == nil {
		return
	}
	for k := range g.enlaces {
		cerrarEnlace(g, k)
	}
}

// InvalidarEnlaces corta, en los proxies de enlace de todas las máquinas, las
// sesiones hacia cada uno de ids (una máquina o un nodo: ver
// credproxy.Enlace.Invalidar). Devuelve cuántas cortó.
func InvalidarEnlaces(ids ...string) int {
	grafoMu.Lock()
	var es []*credproxy.Enlace
	for _, g := range grafosRed {
		for _, ev := range g.enlaces {
			es = append(es, ev.e)
		}
	}
	grafoMu.Unlock()
	n := 0
	for _, e := range es {
		for _, id := range ids {
			if id != "" {
				n += e.Invalidar(id)
			}
		}
	}
	return n
}

// reglasGrafo pone en el netns de un nodo en egress none o internet lo que en
// allowlist ya puso applyAllowlist: el DNS del invitado al resolver del host
// y los proxies de credenciales, con sus ACCEPT delante de los DROP. Nada de
// esto abre una salida: todo va a n.HostIP, el lado host del propio veth.
func (n *Net) reglasGrafo(e Egress) [][]string {
	dnsTarget := fmt.Sprintf("%s:%d", n.HostIP, dnsPort)
	var reglas [][]string
	for _, proto := range []string{"udp", "tcp"} {
		reglas = append(reglas, []string{"-t", "nat", "-A", "PREROUTING", "-i", TapName,
			"-p", proto, "--dport", "53", "-j", "DNAT", "--to-destination", dnsTarget})
	}
	for _, r := range n.credNATRules() {
		reglas = append(reglas, r[1:]) // sin el "iptables" de delante
	}
	dnsPortStr := strconv.Itoa(dnsPort)
	for _, proto := range []string{"udp", "tcp"} {
		reglas = append(reglas, []string{"-I", "FORWARD", "1", "-i", TapName, "-o", n.NSIf,
			"-p", proto, "-d", n.HostIP, "--dport", dnsPortStr, "-j", "ACCEPT"})
	}
	for _, r := range n.credForwardRules() {
		// {"iptables", "-A", "FORWARD", ...} -> {"-I", "FORWARD", "1", ...}
		reglas = append(reglas, append([]string{"-I", "FORWARD", "1"}, r[3:]...))
	}
	if e == EgressNone {
		// Sin MASQUERADE (none no lo tiene), la respuesta del host iría a
		// 172.16.0.2, que el host no sabe alcanzar. Solo hacia n.HostIP.
		reglas = append(reglas, []string{"-t", "nat", "-A", "POSTROUTING", "-o", n.NSIf,
			"-d", n.HostIP, "-j", "MASQUERADE"})
	}
	return reglas
}

// asegurarReglas pone las que falten, en orden.
func (n *Net) asegurarReglas(reglas [][]string) error {
	for _, r := range reglas {
		if err := n.asegurarRegla(r); err != nil {
			return err
		}
	}
	return nil
}

// reglasEnlace son el DNAT de n.HostIP:port al proxy de enlace del slot y su
// ACCEPT, delante de todo lo demás.
func (n *Net) reglasEnlace(port, slot int) [][]string {
	destino := fmt.Sprintf("%s:%d", n.HostIP, linkBasePort+slot)
	return [][]string{
		{"-t", "nat", "-I", "PREROUTING", "1", "-i", TapName, "-p", "tcp", "-d", n.HostIP,
			"--dport", strconv.Itoa(port), "-j", "DNAT", "--to-destination", destino},
		{"-I", "FORWARD", "1", "-i", TapName, "-o", n.NSIf, "-p", "tcp", "-d", n.HostIP,
			"--dport", strconv.Itoa(linkBasePort + slot), "-j", "ACCEPT"},
	}
}

// asegurarRegla pone r (argumentos de iptables, con -A o -I) dentro del netns
// si no está ya: es lo que hace SetGraph idempotente (thaw con la red
// conservada, reinicio del daemon).
func (n *Net) asegurarRegla(r []string) error {
	check := comprobacionDe(r)
	if exec_ok(append([]string{"ip", "netns", "exec", n.NS, "iptables"}, check...)) {
		return nil
	}
	return run(append([]string{"ip", "netns", "exec", n.NS, "iptables"}, r...)...)
}

// comprobacionDe convierte una regla con -A cadena o -I cadena pos en su -C.
func comprobacionDe(r []string) []string {
	out := make([]string, 0, len(r))
	for i := 0; i < len(r); i++ {
		switch r[i] {
		case "-A":
			out = append(out, "-C", r[i+1])
			i++
		case "-I":
			out = append(out, "-C", r[i+1])
			i++
			if i+1 < len(r) {
				if _, err := strconv.Atoi(r[i+1]); err == nil {
					i++ // la posición no va en -C
				}
			}
		default:
			out = append(out, r[i])
		}
	}
	return out
}
