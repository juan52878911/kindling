package api

// Grafos de microVMs (docs/grafos.md): un conjunto de máquinas con nombre,
// aristas declaradas y ciclo de vida atómico. El sustantivo vive en el núcleo;
// cada arista es una autorización, no "la red": un nodo solo llega a otro por
// una arista que el grafo declara, y el daemon la resuelve en cada conexión.
//
// LO QUE VALIDA ESTE FICHERO es la forma: nombres, tamaños, puertos, que cada
// arista vaya entre nodos que existen a un puerto que el destino expone. Lo
// que depende del host (plantillas, memoria, plataforma) lo mira el daemon.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/share"
)

// Etiquetas que el daemon pone a cada máquina de un grafo. Nadie más puede
// ponerlas ni cambiarlas (ver IsGraphLabel): son la mitad de la comprobación
// de cada arista.
const (
	LabelGraph     = "kling.graph"
	LabelGraphNode = "kling.graph.node"
)

// IsGraphLabel dice si k es una etiqueta reservada a los grafos (kling.graph y
// todo lo que cuelga de kling.graph.). La API las rechaza en run, sandbox,
// fork y PUT labels.
func IsGraphLabel(k string) bool {
	return k == LabelGraph || strings.HasPrefix(k, LabelGraph+".")
}

// ValidateNoGraphLabels rechaza un juego de etiquetas que traiga alguna de
// los grafos.
func ValidateNoGraphLabels(labels map[string]string) error {
	for k := range labels {
		if IsGraphLabel(k) {
			return fmt.Errorf("label %q is reserved: the daemon sets it on the machines of a graph", k)
		}
	}
	return nil
}

// Política de despertar de un nodo.
const (
	// GraphWakeEager: se arranca con `graph up`.
	GraphWakeEager = "eager"
	// GraphWakeLazy: plantilla sin instancia hasta la primera conexión por
	// una arista (o hasta que un `graph thaw` la despierta si ya tuvo una).
	GraphWakeLazy = "lazy"
)

// Tipos de arista.
const (
	// GraphEdgeLink: el nodo From abre TCP al puerto Port de To por el nombre
	// <to>.graph. Solo Linux en esta versión.
	GraphEdgeLink = "link"
	// GraphEdgeCredential: el nodo From recibe un marcador en Env y el proxy de
	// Postgres de su máquina entra en To con la clave real (el attach de
	// `kling db`). Solo Linux en esta versión.
	GraphEdgeCredential = "credential"
	// GraphEdgeShare: From ve la carpeta Mount de To. La carpeta es del grafo
	// (vive en su directorio del almacén y se borra con él); To la monta en
	// lectura y escritura y From con Mode (ro por defecto, o rw). Solo nodos
	// con Image: una carpeta viva se monta al arrancar.
	GraphEdgeShare = "share"
	// GraphEdgeDepends: From no arranca (ni despierta) hasta que To está listo:
	// corriendo y, si la arista lleva Port, con ese puerto contestando. Da el
	// orden de up y thaw (el inverso en freeze y en la pausa de un snapshot);
	// un ciclo se rechaza al validar.
	GraphEdgeDepends = "depends"
	// GraphEdgeMCP NO está en esta versión y se rechaza: el puente MCP solo
	// escucha en el 8080, que es también el agente de invitado (exec,
	// ficheros, volúmenes), y ninguna arista llega nunca a él. Un servidor MCP
	// que hable HTTP en su propio puerto se alcanza con una arista link.
	GraphEdgeMCP = "mcp"
)

// Estados de un grafo (Graph.State).
const (
	GraphStateRunning = "running" // todos los nodos instanciados corren
	GraphStateFrozen  = "frozen"  // ninguno corre
	GraphStatePartial = "partial" // de todo un poco
)

// GraphDomain es el sufijo de los nombres por los que un nodo llega a otro:
// <nodo>.graph. El resolver de un nodo con aristas sirve SOLO los de sus
// aristas; cualquier otro *.graph es NXDOMAIN.
const GraphDomain = "graph"

// Límites. No son de capacidad (el tope de máquinas y la memoria deciden),
// sino de que un fichero no pueda encargar cientos de máquinas o de reglas.
const (
	GraphMaxNodes = 32
	GraphMaxEdges = 64
	// GraphMaxConnsPerEdge: conexiones a la vez por arista link; la siguiente
	// se cierra en el acto.
	GraphMaxConnsPerEdge = 16
	// GraphForkMax: grafos por `graph fork`.
	GraphForkMax = 16
	// graphMaxPorts: puertos expuestos por nodo.
	graphMaxPorts = 16
)

// GraphPGPort es el puerto por defecto de una arista credential.
const GraphPGPort = 5432

var (
	// reGraphName: cabe con el nodo y la generación en un nombre de
	// plantilla (<grafo>-<nodo>-<gen>, 64 como mucho).
	reGraphName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)
	// reGraphNode es una etiqueta DNS: el nodo se alcanza por <nodo>.graph.
	reGraphNode = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$`)
	// reGraphEnv es el nombre de la variable de una arista credential (el
	// mismo patrón que las credenciales de máquina).
	reGraphEnv = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	// reGraphPGName: rol y base de una arista credential. Sin comillas ni
	// espacios: van tal cual al arranque del protocolo.
	reGraphPGName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,62}$`)
)

// Graph es un grafo: sus nodos por nombre, sus aristas y su estado.
type Graph struct {
	// ID lo pone el daemon; en `graph up` se ignora.
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	// Nodes por nombre (una etiqueta DNS: el nodo es <nombre>.graph).
	Nodes map[string]GraphNode `json:"nodes"`
	Edges []GraphEdge          `json:"edges,omitempty"`
	// State lo calcula el daemon al responder: running, frozen o partial.
	State string `json:"state,omitempty"`
	// Generation sube en cada snapshot y en cada fork.
	Generation int       `json:"generation,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
	// ForkOf es el ID del grafo del que salió este (graph fork).
	ForkOf string `json:"fork_of,omitempty"`
}

// GraphNode es una máquina del grafo.
type GraphNode struct {
	// From (plantilla) o Image (arranque en frío): uno de los dos.
	From  string `json:"from,omitempty"`
	Image string `json:"image,omitempty"`
	// VCPUs y MemMiB: 0 = los de la plantilla (o los de siempre en frío).
	VCPUs  int `json:"vcpus,omitempty"`
	MemMiB int `json:"mem_mib,omitempty"`
	// Egress y AllowDomains, como en run. Una arista no abre nada de esto.
	Egress       string   `json:"egress,omitempty"`
	AllowDomains []string `json:"allow_domains,omitempty"`
	// Ports son los puertos que el nodo expone a sus aristas (kling.ports).
	Ports []int `json:"ports,omitempty"`
	// Wake: eager (por defecto) o lazy.
	Wake string `json:"wake,omitempty"`
	// IdleFreezeSeconds congela el nodo tras ese tiempo sin conexiones nuevas:
	// es el TTL de siempre con on_ttl freeze, y cada conexión aceptada por
	// una arista hacia el nodo (o despertarlo) reinicia su reloj.
	IdleFreezeSeconds int                `json:"idle_freeze,omitempty"`
	Volumes           []VolumeAttachment `json:"volumes,omitempty"`
	// Shares: carpetas del host del propio nodo, como en run (solo con Image).
	Shares []ShareSpec       `json:"shares,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	// AllowExec, como en run.
	AllowExec bool `json:"allow_exec,omitempty"`

	// Lo que pone el daemon.

	// MachineID es la máquina del nodo; vacío en un lazy aún sin instancia.
	MachineID string `json:"machine_id,omitempty"`
	// State es el de su máquina al responder ("" sin instancia).
	State string `json:"state,omitempty"`
}

// GraphEdge es una arista dirigida: From llega a To.
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	// Port del destino (tiene que estar en sus Ports). En credential, 5432
	// por defecto; en depends, opcional (el puerto que tiene que contestar);
	// en share, ninguno.
	Port int `json:"port,omitempty"`
	// Env, User y Database: solo credential (la variable del marcador, el rol
	// y la base).
	Env      string `json:"env,omitempty"`
	User     string `json:"user,omitempty"`
	Database string `json:"database,omitempty"`
	// Mount y Mode: solo share. Mount es la ruta de la carpeta en los dos
	// invitados; Mode, cómo la monta From (ro por defecto, o rw).
	Mount string `json:"mount,omitempty"`
	Mode  string `json:"mode,omitempty"`
	// Secret es la clave real de una arista credential. NUNCA se serializa:
	// viaja en GraphRequest.Secrets y el daemon la guarda cifrada.
	Secret string `json:"-"`
}

// Key identifica una arista credential dentro del grafo: su origen y su
// variable ("api/PGPASSWORD"). Es la clave de GraphRequest.Secrets.
func (e GraphEdge) Key() string { return e.From + "/" + e.Env }

// Host es el nombre por el que From llega a To: <to>.graph.
func (e GraphEdge) Host() string { return e.To + "." + GraphDomain }

// GraphRequest es el cuerpo de POST /graphs.
type GraphRequest struct {
	Graph Graph `json:"graph"`
	// Secrets son las claves de las aristas credential, por GraphEdge.Key.
	Secrets map[string]string `json:"secrets,omitempty"`
}

// GraphSnapshotRequest es el cuerpo de POST /graphs/{ref}/snapshot.
type GraphSnapshotRequest struct {
	// Name es el prefijo de las plantillas (<name>-<nodo>-<gen>); por
	// defecto, el nombre del grafo.
	Name string `json:"name,omitempty"`
}

// GraphSnapshot es el resultado de un snapshot del grafo: una plantilla por
// nodo instanciado, todas del mismo instante y la misma generación.
type GraphSnapshot struct {
	Graph      string            `json:"graph"`
	Generation int               `json:"generation"`
	Templates  map[string]string `json:"templates"`
}

// GraphForkRequest es el cuerpo de POST /graphs/{ref}/fork.
type GraphForkRequest struct {
	Count int `json:"count,omitempty"`
}

// GraphForkResult son los grafos nuevos.
type GraphForkResult struct {
	Graphs []*Graph `json:"graphs"`
}

// SortedNodeNames devuelve los nombres de los nodos en orden: el de arranque,
// congelación y listado, para que todo sea reproducible.
func (g *Graph) SortedNodeNames() []string {
	out := make([]string, 0, len(g.Nodes))
	for n := range g.Nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ValidateGraphName comprueba el nombre de un grafo.
func ValidateGraphName(name string) error {
	if !reGraphName.MatchString(name) {
		return fmt.Errorf("graph name %q must be 1-24 lowercase letters, digits or '-', starting with a letter or digit", name)
	}
	return nil
}

// ValidateGraph comprueba la forma de un grafo y la normaliza en el sitio
// (Wake por defecto, puerto por defecto de credential, sin estado ni IDs de
// fuera). No mira el host: plantillas, memoria y plataforma son del daemon.
func ValidateGraph(g *Graph) error {
	if err := ValidateGraphName(g.Name); err != nil {
		return err
	}
	if len(g.Nodes) == 0 {
		return fmt.Errorf("graph %s has no nodes", g.Name)
	}
	if len(g.Nodes) > GraphMaxNodes {
		return fmt.Errorf("graph %s has %d nodes; the limit is %d", g.Name, len(g.Nodes), GraphMaxNodes)
	}
	if len(g.Edges) > GraphMaxEdges {
		return fmt.Errorf("graph %s has %d edges; the limit is %d", g.Name, len(g.Edges), GraphMaxEdges)
	}
	// Lo que pone el daemon no se acepta de fuera.
	g.ID, g.State, g.Generation, g.ForkOf = "", "", 0, ""
	g.CreatedAt = time.Time{}
	for _, name := range g.SortedNodeNames() {
		n := g.Nodes[name]
		if err := validarNodo(name, &n); err != nil {
			return err
		}
		g.Nodes[name] = n
	}
	for i := range g.Edges {
		if err := validarArista(g, &g.Edges[i]); err != nil {
			return err
		}
	}
	return validarAristasJuntas(g)
}

func validarNodo(name string, n *GraphNode) error {
	if !reGraphNode.MatchString(name) {
		return fmt.Errorf("node name %q must be a DNS label: 1-24 lowercase letters, digits or '-' (it is reached as %s.%s)", name, name, GraphDomain)
	}
	n.MachineID, n.State = "", ""
	switch {
	case n.From == "" && n.Image == "":
		return fmt.Errorf("node %s: needs from (a template) or image", name)
	case n.From != "" && n.Image != "":
		return fmt.Errorf("node %s: from and image exclude each other", name)
	}
	switch n.Wake {
	case "":
		n.Wake = GraphWakeEager
	case GraphWakeEager, GraphWakeLazy:
	default:
		return fmt.Errorf("node %s: wake %q: use %s or %s", name, n.Wake, GraphWakeEager, GraphWakeLazy)
	}
	switch n.Egress {
	case "", "none", "internet", "allowlist":
	default:
		return fmt.Errorf("node %s: egress %q: use none, internet or allowlist", name, n.Egress)
	}
	if n.VCPUs < 0 || n.MemMiB < 0 || n.IdleFreezeSeconds < 0 {
		return fmt.Errorf("node %s: vcpus, mem_mib and idle_freeze can't be negative", name)
	}
	if len(n.Ports) > graphMaxPorts {
		return fmt.Errorf("node %s: at most %d ports", name, graphMaxPorts)
	}
	vistos := map[int]bool{}
	for _, p := range n.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("node %s: port %d out of range", name, p)
		}
		if vistos[p] {
			return fmt.Errorf("node %s: port %d listed twice", name, p)
		}
		vistos[p] = true
	}
	sort.Ints(n.Ports)
	if len(n.Shares) > 0 && n.From != "" {
		return fmt.Errorf("node %s: shared folders need a cold boot (image), not a template", name)
	}
	for k := range n.Labels {
		if !KeyPattern.MatchString(k) {
			return fmt.Errorf("node %s: label %q is not valid (lowercase letters, digits, '.', '_', '-')", name, k)
		}
		if IsGraphLabel(k) || k == LabelForkOf || k == LabelPorts {
			return fmt.Errorf("node %s: label %q is reserved (the daemon sets it; ports go in ports)", name, k)
		}
	}
	return nil
}

func validarArista(g *Graph, e *GraphEdge) error {
	desc := fmt.Sprintf("edge %s -> %s (%s)", e.From, e.To, e.Kind)
	from, ok := g.Nodes[e.From]
	if !ok {
		return fmt.Errorf("%s: node %q doesn't exist", desc, e.From)
	}
	to, ok := g.Nodes[e.To]
	if !ok {
		return fmt.Errorf("%s: node %q doesn't exist", desc, e.To)
	}
	if e.From == e.To {
		return fmt.Errorf("%s: a node can't have an edge to itself", desc)
	}
	// El puerto del agente de invitado (y del puente MCP) no se alcanza por una
	// arista: el agente no autentica, confía en que solo el host llega a él, y
	// el proxy de enlace marca desde el host. Una arista a él daría a otro nodo
	// exec, ficheros y volúmenes del destino.
	if e.Port == GuestPort {
		return fmt.Errorf("%s: port %d is the kindling guest agent (exec, files, volumes): a graph edge can never reach it; expose the service on another port", desc, GuestPort)
	}
	if e.Kind != GraphEdgeCredential && (e.Env != "" || e.User != "" || e.Database != "") {
		return fmt.Errorf("%s: env, user and database are only for credential edges", desc)
	}
	if e.Kind != GraphEdgeShare && (e.Mount != "" || e.Mode != "") {
		return fmt.Errorf("%s: mount and mode are only for share edges", desc)
	}
	switch e.Kind {
	case GraphEdgeLink:
		switch e.Port {
		case 53, 80, 443:
			// El nombre <nodo>.graph resuelve a la IP del host en el veth del
			// nodo, donde el 53 es su DNS y el 80/443 el proxy de credenciales.
			return fmt.Errorf("%s: port %d is reserved on the graph address (DNS and the credential proxy); expose the service on another port", desc, e.Port)
		}
	case GraphEdgeCredential:
		if e.Port == 0 {
			e.Port = GraphPGPort
		}
		if !reGraphEnv.MatchString(e.Env) {
			return fmt.Errorf("%s: env %q must match [A-Z_][A-Z0-9_]*", desc, e.Env)
		}
		if !reGraphPGName.MatchString(e.User) {
			return fmt.Errorf("%s: user %q is not a valid role name", desc, e.User)
		}
		if !reGraphPGName.MatchString(e.Database) {
			return fmt.Errorf("%s: database %q is not a valid database name", desc, e.Database)
		}
	case GraphEdgeShare:
		return validarShare(desc, from, to, e)
	case GraphEdgeDepends:
		// Sin puerto, listo es "corriendo"; con puerto, además ese puerto
		// contesta (y tiene que estar entre los que el destino expone).
		if e.Port == 0 {
			return nil
		}
	case GraphEdgeMCP:
		return fmt.Errorf("%s: mcp edges are not in this version: the MCP bridge listens only on port %d, "+
			"which is also the guest agent and no edge can ever reach; if the MCP server speaks HTTP on its own port, use a link edge to that port", desc, GuestPort)
	default:
		return fmt.Errorf("%s: unknown kind; use %s, %s, %s or %s", desc, GraphEdgeLink, GraphEdgeCredential, GraphEdgeShare, GraphEdgeDepends)
	}
	if e.Port < 1 || e.Port > 65535 {
		return fmt.Errorf("%s: port %d out of range", desc, e.Port)
	}
	if !contienePuerto(to.Ports, e.Port) {
		return fmt.Errorf("%s: node %s does not expose port %d (add it to its ports)", desc, e.To, e.Port)
	}
	return nil
}

// validarShare comprueba una arista share y normaliza su modo (ro por
// defecto: quien no es dueño de la carpeta solo la lee si no pide más).
func validarShare(desc string, from, to GraphNode, e *GraphEdge) error {
	if e.Port != 0 {
		return fmt.Errorf("%s: a share edge has no port", desc)
	}
	if err := share.ValidMount(e.Mount); err != nil {
		return fmt.Errorf("%s: %w", desc, err)
	}
	switch e.Mode {
	case "":
		e.Mode = share.ModeRO
	case share.ModeRO, share.ModeRW:
	case share.ModeCopy:
		return fmt.Errorf("%s: mode copy is an upload made once for one machine, not a folder two nodes see; use ro or rw", desc)
	default:
		return fmt.Errorf("%s: mode %q: use ro or rw", desc, e.Mode)
	}
	// Una carpeta viva se monta al arrancar: un nodo que sale de una
	// plantilla ya arrancó (lo mismo que los shares del nodo).
	if from.From != "" || to.From != "" {
		return fmt.Errorf("%s: shared folders need a cold boot (image) on both nodes, not a template", desc)
	}
	return nil
}

// validarAristasJuntas mira lo que depende de varias aristas a la vez.
func validarAristasJuntas(g *Graph) error {
	type clave struct {
		from string
		port int
	}
	puertos := map[clave]string{} // (origen, puerto) -> destino
	envs := map[string]bool{}     // origen/variable
	vistas := map[string]bool{}
	for _, e := range g.Edges {
		k := e.From + "\x00" + e.To + "\x00" + e.Kind + "\x00" + strconv.Itoa(e.Port) + "\x00" + e.Mount
		if vistas[k] {
			return fmt.Errorf("edge %s -> %s (%s, port %d) is declared twice", e.From, e.To, e.Kind, e.Port)
		}
		vistas[k] = true
		if e.Kind != GraphEdgeLink && e.Kind != GraphEdgeCredential {
			continue // share y depends no ocupan un puerto de la dirección del grafo
		}
		// Todas las aristas de un nodo llegan por la misma dirección (la del
		// host en su veth): dos al mismo puerto no se distinguirían.
		if otro, ok := puertos[clave{e.From, e.Port}]; ok {
			return fmt.Errorf("node %s has two edges on port %d (to %s and to %s): a node reaches one node per port in this version",
				e.From, e.Port, otro, e.To)
		}
		puertos[clave{e.From, e.Port}] = e.To
		if e.Kind == GraphEdgeCredential {
			if envs[e.Key()] {
				return fmt.Errorf("node %s has two credential edges with env %s", e.From, e.Env)
			}
			envs[e.Key()] = true
		}
	}
	if err := validarMontajes(g); err != nil {
		return err
	}
	_, err := g.StartOrder()
	return err
}

// validarMontajes comprueba, nodo a nodo, que sus carpetas (las suyas y las
// de sus aristas share) no se pisan: una ruta, una carpeta; ninguna dentro de
// otra; y no más de share.MaxShares.
func validarMontajes(g *Graph) error {
	porNodo := map[string]map[string]string{} // nodo -> montaje -> carpeta
	poner := func(nodo, mount, carpeta string) error {
		ms := porNodo[nodo]
		if ms == nil {
			ms = map[string]string{}
			for i, s := range g.Nodes[nodo].Shares {
				ms[s.Mount] = "own\x00" + strconv.Itoa(i)
			}
			porNodo[nodo] = ms
		}
		if otra, ok := ms[mount]; ok {
			if otra == carpeta {
				return nil
			}
			return fmt.Errorf("node %s mounts two different folders at %s", nodo, mount)
		}
		for mp := range ms {
			if strings.HasPrefix(mount, mp+"/") || strings.HasPrefix(mp, mount+"/") {
				return fmt.Errorf("node %s: %s and %s are nested: one would hide the other", nodo, mount, mp)
			}
		}
		ms[mount] = carpeta
		if len(ms) > share.MaxShares {
			return fmt.Errorf("node %s has more than %d shared folders (its own and its share edges)", nodo, share.MaxShares)
		}
		return nil
	}
	for _, e := range g.Edges {
		if e.Kind != GraphEdgeShare {
			continue
		}
		// La carpeta es la de To en esa ruta: la ven To y todos los que tienen
		// una arista share hacia ella.
		carpeta := "edge\x00" + e.To + "\x00" + e.Mount
		if err := poner(e.To, e.Mount, carpeta); err != nil {
			return err
		}
		if err := poner(e.From, e.Mount, carpeta); err != nil {
			return err
		}
	}
	return nil
}

// Dependencies son los nodos de los que depende nodo (sus aristas depends),
// por nombre y sin repetir.
func (g *Graph) Dependencies(nodo string) []string {
	vistos := map[string]bool{}
	var out []string
	for _, e := range g.Edges {
		if e.Kind == GraphEdgeDepends && e.From == nodo && !vistos[e.To] {
			vistos[e.To] = true
			out = append(out, e.To)
		}
	}
	sort.Strings(out)
	return out
}

// StartOrder es el orden de arranque del grafo: cada nodo después de los que
// depende (aristas depends) y, entre los que pueden ir a la vez, por nombre
// (sin aristas depends es SortedNodeNames). Lo siguen up y thaw. Un ciclo es
// un error.
func (g *Graph) StartOrder() ([]string, error) { return g.ordenDepends(false) }

// StopOrder es el de parada: cada nodo antes de los que depende y, entre los
// que pueden ir a la vez, por nombre. Lo siguen freeze y la pausa de un
// snapshot.
func (g *Graph) StopOrder() ([]string, error) { return g.ordenDepends(true) }

// ordenDepends ordena los nodos por sus aristas depends (Kahn, desempatando
// por nombre para que sea reproducible). inverso: los dependientes primero.
func (g *Graph) ordenDepends(inverso bool) ([]string, error) {
	pendientes := map[string]int{} // nodo -> los que tienen que ir antes
	despues := map[string][]string{}
	for n := range g.Nodes {
		pendientes[n] += 0
		for _, d := range g.Dependencies(n) {
			antes, luego := d, n
			if inverso {
				antes, luego = n, d
			}
			pendientes[luego]++
			despues[antes] = append(despues[antes], luego)
		}
	}
	var listos []string
	for n, k := range pendientes {
		if k == 0 {
			listos = append(listos, n)
		}
	}
	out := make([]string, 0, len(g.Nodes))
	for len(listos) > 0 {
		sort.Strings(listos)
		n := listos[0]
		listos = listos[1:]
		out = append(out, n)
		for _, d := range despues[n] {
			pendientes[d]--
			if pendientes[d] == 0 {
				listos = append(listos, d)
			}
		}
	}
	if len(out) != len(g.Nodes) {
		var ciclo []string
		for n, k := range pendientes {
			if k > 0 {
				ciclo = append(ciclo, n)
			}
		}
		sort.Strings(ciclo)
		return nil, fmt.Errorf("graph %s: depends edges form a cycle (through %s)", g.Name, strings.Join(ciclo, ", "))
	}
	return out, nil
}

func contienePuerto(ps []int, p int) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}

// HasNetworkEdges dice si el grafo tiene aristas que conectan máquinas
// (link o credential): las que pasan por el proxy de enlaces (Linux) o por el
// broker de enlaces (macOS).
func (g *Graph) HasNetworkEdges() bool {
	for _, e := range g.Edges {
		if e.Kind == GraphEdgeLink || e.Kind == GraphEdgeCredential {
			return true
		}
	}
	return false
}

// HasPortDepends dice si alguna arista depends espera a un puerto (y no solo
// a que el nodo corra): el daemon lo comprueba marcando a la IP del netns del
// destino en Linux y preguntando a su kling-vz (KlingProbe) en macOS.
func (g *Graph) HasPortDepends() bool {
	for _, e := range g.Edges {
		if e.Kind == GraphEdgeDepends && e.Port != 0 {
			return true
		}
	}
	return false
}

// ── cliente ─────────────────────────────────────────────────────────────────

// GraphUp crea y arranca un grafo (capacidad "graphs").
func (c *Client) GraphUp(ctx context.Context, req GraphRequest) (*Graph, error) {
	var out Graph
	return &out, c.doWith(c.long, ctx, http.MethodPost, "/graphs", req, &out)
}

// Graphs lista los grafos.
func (c *Client) Graphs(ctx context.Context) ([]*Graph, error) {
	var out []*Graph
	return out, c.do(ctx, http.MethodGet, "/graphs", nil, &out)
}

// Graph devuelve un grafo por ID, prefijo de ID o nombre.
func (c *Client) Graph(ctx context.Context, ref string) (*Graph, error) {
	var out Graph
	return &out, c.do(ctx, http.MethodGet, "/graphs/"+url.PathEscape(ref), nil, &out)
}

// GraphFreeze congela todos los nodos del grafo.
func (c *Client) GraphFreeze(ctx context.Context, ref string) (*Graph, error) {
	var out Graph
	return &out, c.doWith(c.long, ctx, http.MethodPost, "/graphs/"+url.PathEscape(ref)+"/freeze", nil, &out)
}

// GraphThaw despierta todos los nodos instanciados del grafo.
func (c *Client) GraphThaw(ctx context.Context, ref string) (*Graph, error) {
	var out Graph
	return &out, c.doWith(c.long, ctx, http.MethodPost, "/graphs/"+url.PathEscape(ref)+"/thaw", nil, &out)
}

// GraphSnapshot guarda el grafo en un instante consistente.
func (c *Client) GraphSnapshot(ctx context.Context, ref string, req GraphSnapshotRequest) (*GraphSnapshot, error) {
	var out GraphSnapshot
	return &out, c.doWith(c.long, ctx, http.MethodPost, "/graphs/"+url.PathEscape(ref)+"/snapshot", req, &out)
}

// GraphFork ramifica el grafo en req.Count grafos nuevos.
func (c *Client) GraphFork(ctx context.Context, ref string, req GraphForkRequest) (*GraphForkResult, error) {
	var out GraphForkResult
	return &out, c.doWith(c.long, ctx, http.MethodPost, "/graphs/"+url.PathEscape(ref)+"/fork", req, &out)
}

// GraphRemove borra el grafo y sus máquinas.
func (c *Client) GraphRemove(ctx context.Context, ref string) error {
	return c.doWith(c.long, ctx, http.MethodDelete, "/graphs/"+url.PathEscape(ref), nil, nil)
}
