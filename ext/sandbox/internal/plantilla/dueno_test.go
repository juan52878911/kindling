package plantilla

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un inquilino del daemon puede poner kind, kling.graph, template y tenant a
// sus máquinas y llamar a su grafo sbxg-*: sin kling.owner vacío, el frontal
// lo daba por libre y se lo entregaba a un cliente suyo.
func TestInstanciasIgnoraMaquinasConDueno(t *testing.T) {
	l := func(nodo string, owner string) map[string]string {
		m := map[string]string{api.LabelKind: KindNodoGrafo, api.LabelGraph: "g1", api.LabelGraphNode: nodo, EtiquetaPlantilla: "t"}
		if owner != "" {
			m[api.LabelOwner] = owner
		}
		return m
	}
	g := &api.Graph{ID: "g1", Name: PrefijoGrafo + "trampa", Nodes: map[string]api.GraphNode{
		"a": {MachineID: "m1"}, "b": {MachineID: "m2"},
	}}
	ms := []*api.Machine{{ID: "m1", Labels: l("a", "a")}, {ID: "m2", Labels: l("b", "a")}}
	for _, in := range Instancias([]*api.Graph{g}, ms) {
		t.Fatalf("un grafo de un inquilino del daemon salió en el fondo: %+v (libre=%v)", in, in.Libre())
	}
	// El mismo grafo sin dueño sí es del fondo.
	ms = []*api.Machine{{ID: "m1", Labels: l("a", "")}, {ID: "m2", Labels: l("b", "")}}
	if ins := Instancias([]*api.Graph{g}, ms); len(ins) != 1 || !ins[0].Libre() {
		t.Fatalf("el grafo del frontal no salió libre: %+v", ins)
	}
}

// Un snapshot sbx-<plantilla> con la receta correcta pero hecho por un
// inquilino (kling.owner) no se da por bueno: se reconstruye encima.
func TestReconciliarReconstruyeSnapshotConDueno(t *testing.T) {
	p := plantillaDemo()
	sn := conReceta(t, p, HashReceta(p), time.Now().Add(-time.Hour))
	sn.Labels = map[string]string{api.LabelOwner: "a"}
	d := &daemonFalso{snapshots: map[string]*api.Snapshot{SnapshotDe(p.Nombre): sn}}
	c := arrancar(t, d)

	_, construido, err := Reconciliar(t.Context(), c, p)
	if err != nil {
		t.Fatalf("Reconciliar: %v", err)
	}
	if !construido {
		t.Fatal("dio por bueno el snapshot de un inquilino con el nombre de la plantilla")
	}
}
