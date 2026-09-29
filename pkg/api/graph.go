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
	// GraphEdgeShare y GraphEdgeDepends son del diseño y NO de esta versión: se
	// rechazan con un mensaje que dice qué usar en su lugar.
	GraphEdgeShare   = "share"
	GraphEdgeDepends = "depends"
	GraphEdgeMCP     = "mcp"
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
	// IdleFreezeSeconds congela el nodo tras ese tiempo (el TTL de siempre con
	// on_ttl freeze). En esta versión no se renueva por conexión.
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
	// por defecto.
	Port int `json:"port,omitempty"`
	// Env, User y Database: solo credential (la variable del marcador, el rol
	// y la base).
	Env      string `json:"env,omitempty"`
	User     string `json:"user,omitempty"`
	Database string `json:"database,omitempty"`
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
	if _, ok := g.Nodes[e.From]; !ok {
		return fmt.Errorf("%s: node %q doesn't exist", desc, e.From)
	}
	to, ok := g.Nodes[e.To]
	if !ok {
		return fmt.Errorf("%s: node %q doesn't exist", desc, e.To)
	}
	if e.From == e.To {
		return fmt.Errorf("%s: a node can't have an edge to itself", desc)
	}
	switch e.Kind {
	case GraphEdgeLink:
		if e.Env != "" || e.User != "" || e.Database != "" {
			return fmt.Errorf("%s: env, user and database are only for credential edges", desc)
		}
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
		return fmt.Errorf("%s: share edges are not in this version; declare the folder in the node's shares", desc)
	case GraphEdgeDepends:
		return fmt.Errorf("%s: depends edges are not in this version; a lazy node wakes on its first connection", desc)
	case GraphEdgeMCP:
		return fmt.Errorf("%s: mcp edges are not in this version", desc)
	default:
		return fmt.Errorf("%s: unknown kind; use %s or %s", desc, GraphEdgeLink, GraphEdgeCredential)
	}
	if e.Port < 1 || e.Port > 65535 {
		return fmt.Errorf("%s: port %d out of range", desc, e.Port)
	}
	if !contienePuerto(to.Ports, e.Port) {
		return fmt.Errorf("%s: node %s does not expose port %d (add it to its ports)", desc, e.To, e.Port)
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
		k := e.From + "\x00" + e.To + "\x00" + e.Kind + "\x00" + strconv.Itoa(e.Port)
		if vistas[k] {
			return fmt.Errorf("edge %s -> %s (%s, port %d) is declared twice", e.From, e.To, e.Kind, e.Port)
		}
		vistas[k] = true
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
	return nil
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
// (link o credential): las que en macOS no están en esta versión.
func (g *Graph) HasNetworkEdges() bool {
	for _, e := range g.Edges {
		if e.Kind == GraphEdgeLink || e.Kind == GraphEdgeCredential {
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
