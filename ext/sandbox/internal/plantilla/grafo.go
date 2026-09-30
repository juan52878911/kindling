package plantilla

// PLANTILLAS DE GRAFO: entornos enteros precalentados.
//
// Una plantilla de máquina es una receta que acaba en un snapshot dorado. Una
// plantilla de grafo no construye nada: es la DECLARACIÓN de un grafo de
// kindling (nodos que nacen de plantillas ya construidas o de imágenes, y sus
// aristas), con un nombre y un pool. El fondo levanta instancias enteras
// (`POST /graphs`) y las congela (`POST /graphs/{ref}/freeze`); el frontal
// reclama una como reclama una máquina: la etiqueta con el tenant y la
// despierta.
//
// La declaración vive en el store del daemon (espacio EspacioGrafos), no en un
// snapshot: no hay un solo dorado del que colgarla, y el store es justo el
// sitio para datos de una extensión que no son de ninguna máquina. Como los
// snapshots, el store es de cada host: `kling sbx template apply` la escribe en
// todos.
//
// EL CONTRATO son las etiquetas de las máquinas de cada nodo, igual que con
// las máquinas precalentadas:
//
//	kind=sandbox-graph  template=<plantilla>  [tenant=<dueño>]  kling.graph=<id>
//
// kling.graph la pone el daemon y nadie puede cambiarla (SetLabels la rechaza),
// así que una máquina no puede hacerse pasar por nodo de otro grafo. Un grafo
// es LIBRE si todas sus máquinas existen, son de la misma plantilla y ninguna
// tiene tenant; es DE un tenant si todas las que existen llevan ese tenant.
// Cualquier mezcla es un grafo roto, que nadie reclama y la limpieza del
// frontal borra.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// KindGrafo es el valor de "kind" en el JSON de una plantilla de grafo.
	// Sin "kind" (o con "machine") la plantilla es de máquina, como siempre.
	KindGrafo = "graph"
	// EspacioGrafos es el espacio del store del daemon donde viven las
	// plantillas de grafo, una clave por plantilla.
	EspacioGrafos = "sandbox.graph-templates"
	// KindNodoGrafo marca en api.LabelKind las máquinas de los nodos. Distinto
	// de api.KindSandbox a propósito: ni el pool de máquinas ni las rutas de
	// sandboxes deben confundir un nodo con un sandbox suelto.
	KindNodoGrafo = "sandbox-graph"
	// PrefijoGrafo es el principio del nombre de cada instancia en el daemon.
	PrefijoGrafo = "sbxg-"
	// EtiquetaTenant es el dueño, la misma etiqueta que en los sandboxes.
	EtiquetaTenant = "tenant"
	// EtiquetaReclamado es cuándo se reclamó (segundos Unix): el reloj de la
	// limpieza de grafos abandonados.
	EtiquetaReclamado = "sandbox.claimed-at"
	// MaxPoolGrafo acota el pool de una plantilla de grafo: cada instancia son
	// varias máquinas y hasta 32 nodos.
	MaxPoolGrafo = 16
)

// PlantillaGrafo es la declaración de un grafo precalentable.
type PlantillaGrafo struct {
	Kind   string `json:"kind"`
	Nombre string `json:"name"`
	// Pool son las instancias congeladas que el fondo mantiene listas.
	Pool  int                      `json:"pool,omitempty"`
	Nodes map[string]api.GraphNode `json:"nodes"`
	Edges []api.GraphEdge          `json:"edges,omitempty"`
}

// EsGrafo dice si un JSON de plantilla es de grafo (mira solo "kind").
func EsGrafo(b []byte) bool {
	var k struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(b, &k)
	return k.Kind == KindGrafo
}

// etiquetaReservada: las que pone el sandbox (y las del núcleo). Un nodo que
// las trajera podría pasar por libre o por de otro tenant.
func etiquetaReservada(k string) bool {
	switch k {
	case api.LabelKind, EtiquetaTenant, EtiquetaPlantilla, EtiquetaReclamado:
		return true
	}
	return strings.HasPrefix(k, "kling.") || strings.HasPrefix(k, "sandbox.")
}

// ValidarGrafo comprueba la plantilla y la normaliza en el sitio (lo mismo que
// normaliza api.ValidateGraph: wake por defecto, puertos ordenados).
//
// Además de lo que exige el núcleo, rechaza lo que no se puede repartir entre
// tenants sin fugas:
//   - nodos lazy: una instancia congelada tiene que estar entera, y un nodo que
//     naciera después no llevaría la etiqueta de su dueño;
//   - aristas credential: su clave viajaría en la plantilla y la verían todas
//     las instancias (y el store);
//   - volúmenes y carpetas del host en los nodos: serían los MISMOS en todas las
//     instancias, un canal entre tenants. Las aristas share sí valen: su carpeta
//     es de cada grafo.
func ValidarGrafo(p *PlantillaGrafo) error {
	if p.Kind != KindGrafo {
		return fmt.Errorf("template kind %q is not %q", p.Kind, KindGrafo)
	}
	if !reNombre.MatchString(p.Nombre) {
		return fmt.Errorf("template name %q is not valid (lowercase letters, digits, '-', '_')", p.Nombre)
	}
	if p.Pool < 0 || p.Pool > MaxPoolGrafo {
		return fmt.Errorf("template %s: pool must be between 0 and %d", p.Nombre, MaxPoolGrafo)
	}
	g := api.Graph{Name: PrefijoGrafo + "00000000", Nodes: p.Nodes, Edges: p.Edges}
	if err := api.ValidateGraph(&g); err != nil {
		return fmt.Errorf("template %s: %w", p.Nombre, err)
	}
	p.Nodes, p.Edges = g.Nodes, g.Edges
	for _, nombre := range g.SortedNodeNames() {
		n := g.Nodes[nombre]
		if n.Wake != api.GraphWakeEager {
			return fmt.Errorf("template %s: node %s: prewarmed graphs can't have lazy nodes (the frozen instance has to be whole)", p.Nombre, nombre)
		}
		if len(n.Volumes) > 0 || len(n.Shares) > 0 {
			return fmt.Errorf("template %s: node %s: volumes and host folders would be shared by every instance (every tenant); use a share edge", p.Nombre, nombre)
		}
		for k := range n.Labels {
			if etiquetaReservada(k) {
				return fmt.Errorf("template %s: node %s: label %q is reserved", p.Nombre, nombre, k)
			}
		}
	}
	for _, e := range g.Edges {
		if e.Kind == api.GraphEdgeCredential {
			return fmt.Errorf("template %s: credential edges are not allowed in a graph template (the secret would travel with it)", p.Nombre)
		}
	}
	return nil
}

// NombreInstancia es un nombre nuevo para una instancia en el daemon. Los
// nombres de grafo son únicos por daemon y de 24 caracteres como mucho, así
// que no caben el de la plantilla: eso va en las etiquetas.
func NombreInstancia() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return PrefijoGrafo + hex.EncodeToString(b)
}

// PeticionGrafo es el `graph up` de una instancia de p, con extra (el tenant,
// si se crea ya reclamada) en las etiquetas de cada nodo.
func PeticionGrafo(p PlantillaGrafo, nombre string, extra map[string]string) api.GraphRequest {
	g := api.Graph{Name: nombre, Nodes: map[string]api.GraphNode{}, Edges: append([]api.GraphEdge(nil), p.Edges...)}
	for k, n := range p.Nodes {
		et := map[string]string{api.LabelKind: KindNodoGrafo, EtiquetaPlantilla: p.Nombre}
		for ek, ev := range extra {
			et[ek] = ev
		}
		n.Labels = api.MergeLabels(n.Labels, et)
		g.Nodes[k] = n
	}
	return api.GraphRequest{Graph: g}
}

// ── en el daemon ────────────────────────────────────────────────────────────

// GuardarGrafo valida la plantilla y la escribe en el store del host.
func GuardarGrafo(ctx context.Context, c *api.Client, p PlantillaGrafo) error {
	if err := ValidarGrafo(&p); err != nil {
		return err
	}
	return c.PutStore(ctx, EspacioGrafos, p.Nombre, p)
}

// Grafos lee las plantillas de grafo del host. Una que no valide (escrita a
// mano, o de otra versión) se salta: el fondo no debe levantar lo que el
// frontal no aceptaría.
func Grafos(ctx context.Context, c *api.Client) ([]PlantillaGrafo, error) {
	claves, err := c.StoreKeys(ctx, EspacioGrafos)
	if err != nil {
		if api.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Strings(claves)
	var out []PlantillaGrafo
	for _, k := range claves {
		var p PlantillaGrafo
		if err := c.GetStore(ctx, EspacioGrafos, k, &p); err != nil {
			if api.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if p.Nombre != k || ValidarGrafo(&p) != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// Grafo lee una plantilla de grafo del host. (nil, nil) si no está.
func Grafo(ctx context.Context, c *api.Client, nombre string) (*PlantillaGrafo, error) {
	if !reNombre.MatchString(nombre) {
		return nil, nil
	}
	var p PlantillaGrafo
	if err := c.GetStore(ctx, EspacioGrafos, nombre, &p); err != nil {
		if api.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if p.Nombre != nombre || ValidarGrafo(&p) != nil {
		return nil, nil
	}
	return &p, nil
}

// BorrarGrafo quita la plantilla del store del host y borra sus instancias
// LIBRES (las reclamadas son de sus tenants y siguen hasta que las suelten).
// Devuelve cuántas instancias borró.
func BorrarGrafo(ctx context.Context, c *api.Client, nombre string) (int, error) {
	if err := c.DeleteStore(ctx, EspacioGrafos, nombre); err != nil {
		return 0, err
	}
	gs, err := c.Graphs(ctx)
	if err != nil {
		return 0, err
	}
	ms, err := c.List(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, in := range Instancias(gs, ms) {
		if in.Plantilla == nombre && in.Libre() {
			if err := c.GraphRemove(ctx, in.Grafo.ID); err != nil && !api.IsNotFound(err) {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// FabricarGrafo levanta una instancia libre de p y la congela. Si no llega a
// congelarse, la borra: una instancia a medias no vuelve al fondo.
func FabricarGrafo(ctx context.Context, c *api.Client, p PlantillaGrafo) (*api.Graph, error) {
	g, err := c.GraphUp(ctx, PeticionGrafo(p, NombreInstancia(), nil))
	if err != nil {
		return nil, err
	}
	out, err := c.GraphFreeze(ctx, g.ID)
	if err != nil {
		_ = c.GraphRemove(context.WithoutCancel(ctx), g.ID)
		return nil, fmt.Errorf("freezing graph %s: %w", g.Name, err)
	}
	return out, nil
}

// ── instancias ──────────────────────────────────────────────────────────────

// Instancia es un grafo del pool visto a través de las etiquetas de sus
// máquinas.
type Instancia struct {
	Grafo *api.Graph
	// Plantilla y Tenant son los comunes a todas sus máquinas existentes.
	Plantilla string
	Tenant    string
	// Maquinas por nodo; falta el nodo cuya máquina ya no existe.
	Maquinas map[string]*api.Machine
	// Completa: todos los nodos tienen máquina.
	Completa bool
	// Mezclada: sus máquinas no coinciden en plantilla o tenant (una
	// reclamación que chocó con otra, o etiquetas tocadas a mano).
	Mezclada bool
}

// DelFrontal dice si lo que llevan estas etiquetas lo creó el frontal (o un
// admin): sin kling.owner. Con política de autorización, lo que crea un
// inquilino del daemon lleva su nombre ahí y no lo puede quitar; kind,
// template y tenant sí los puede poner él, así que por sí solos no prueban
// nada. Lo que no sea del frontal no se reparte, no se reclama, no se cuenta
// en el fondo ni se limpia.
func DelFrontal(labels map[string]string) bool { return labels[api.LabelOwner] == "" }

// Libre dice si la instancia se puede reclamar.
func (in Instancia) Libre() bool { return in.Completa && !in.Mezclada && in.Tenant == "" }

// De dice si la instancia es del tenant t.
func (in Instancia) De(t string) bool {
	return t != "" && !in.Mezclada && in.Tenant == t && len(in.Maquinas) > 0
}

// Rota dice si la instancia no la puede usar nadie: ni reclamarla ni verla.
func (in Instancia) Rota() bool { return in.Mezclada || !in.Completa }

// ReclamadaEn es el instante de la reclamación (segundos Unix; 0 si no se sabe).
func (in Instancia) ReclamadaEn() int64 {
	var max int64
	for _, mc := range in.Maquinas {
		var v int64
		if _, err := fmt.Sscan(mc.Labels[EtiquetaReclamado], &v); err == nil && v > max {
			max = v
		}
	}
	return max
}

// Instancias clasifica los grafos del pool (nombre con PrefijoGrafo) con las
// máquinas del host. Un grafo con alguna máquina que no lleve el contrato
// (kind y kling.graph de ese grafo, y sin kling.owner: ver DelFrontal) no es
// del pool y no aparece: ni se
// reclama ni se limpia.
func Instancias(gs []*api.Graph, ms []*api.Machine) []Instancia {
	porID := make(map[string]*api.Machine, len(ms))
	for _, mc := range ms {
		porID[mc.ID] = mc
	}
	var out []Instancia
	for _, g := range gs {
		if !strings.HasPrefix(g.Name, PrefijoGrafo) || len(g.Nodes) == 0 {
			continue
		}
		in := Instancia{Grafo: g, Maquinas: map[string]*api.Machine{}, Completa: true}
		ajeno, primera := false, true
		for _, nombre := range g.SortedNodeNames() {
			n := g.Nodes[nombre]
			mc := porID[n.MachineID]
			if n.MachineID == "" || mc == nil {
				in.Completa = false
				continue
			}
			if mc.Labels[api.LabelKind] != KindNodoGrafo || mc.Labels[api.LabelGraph] != g.ID ||
				mc.Labels[api.LabelGraphNode] != nombre || !DelFrontal(mc.Labels) {
				ajeno = true
				break
			}
			in.Maquinas[nombre] = mc
			tpl, ten := mc.Labels[EtiquetaPlantilla], mc.Labels[EtiquetaTenant]
			if primera {
				in.Plantilla, in.Tenant, primera = tpl, ten, false
			} else if tpl != in.Plantilla || ten != in.Tenant {
				in.Mezclada = true
			}
		}
		if ajeno {
			continue
		}
		if in.Mezclada {
			in.Plantilla, in.Tenant = "", ""
		}
		out = append(out, in)
	}
	return out
}
