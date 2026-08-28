package report

import (
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

func maq(nombre, from, servicio string) *api.Machine {
	m := &api.Machine{ID: nombre + "-id", Name: nombre, From: from, State: api.StateRunning}
	if servicio != "" {
		m.Labels = map[string]string{api.LabelService: servicio}
	}
	return m
}

// El informe agrupa por SERVICIO, no por snapshot: una réplica de scale-out y su
// primaria son el mismo servicio y deben salir juntas, no como dos entradas.
func TestBuildAgrupaPorServicioYNoDuplica(t *testing.T) {
	snaps := []*api.Snapshot{
		{Name: "memory", Labels: map[string]string{api.LabelService: "memory"}, MemBytes: 1 << 20},
		{Name: "dormido", Labels: map[string]string{api.LabelService: "dormido"}},
	}
	machines := []*api.Machine{
		maq("memory-b", "memory", "memory"),
		maq("memory-a", "memory", "memory"),
		maq("suelta", "", ""),
	}
	links := []*api.Link{
		{Name: "engram", URL: "http://x/mcp", Labels: map[string]string{api.LabelService: "engram"}},
	}

	gs := BuildWith(machines, snaps, links)

	porNombre := map[string]Group{}
	for _, g := range gs {
		if _, dup := porNombre[g.Service]; dup {
			t.Errorf("el servicio %q sale dos veces", g.Service)
		}
		porNombre[g.Service] = g
	}

	// Las dos instancias de memory, en un solo grupo y ordenadas.
	g := porNombre["memory"]
	if len(g.Machines) != 2 {
		t.Fatalf("memory tiene %d máquinas, esperaba 2", len(g.Machines))
	}
	if g.Machines[0].Name != "memory-a" {
		t.Errorf("las máquinas no están ordenadas: %s va primero", g.Machines[0].Name)
	}
	if g.Snapshot == nil || g.RAMShared != 1<<20 {
		t.Errorf("el grupo no cogió su snapshot ni su RAM compartida: %+v", g.Snapshot)
	}

	// Un snapshot sin instancias sigue siendo inventario.
	if _, ok := porNombre["dormido"]; !ok {
		t.Error("un snapshot sin instancias desapareció del informe")
	}
	// Un enlace externo es un servicio sin máquinas.
	if e := porNombre["engram"]; e.Link == nil || len(e.Machines) != 0 {
		t.Errorf("el enlace externo no salió como servicio sin máquinas: %+v", e)
	}
	// Una máquina sin servicio ni origen no se pierde.
	if _, ok := porNombre["(ungrouped)"]; !ok {
		t.Error("una máquina suelta desapareció del informe")
	}
	// Y el orden es estable.
	for i := 1; i < len(gs); i++ {
		if gs[i-1].Service > gs[i].Service {
			t.Errorf("los grupos no están ordenados: %q antes de %q", gs[i-1].Service, gs[i].Service)
		}
	}
}

// Un enlace cuyo servicio ya existe se ADJUNTA, no crea un grupo aparte: si no,
// el mismo servicio aparecería dos veces según cómo se sirva.
func TestUnEnlaceSobreUnServicioExistenteSeAdjunta(t *testing.T) {
	snaps := []*api.Snapshot{{Name: "memory", Labels: map[string]string{api.LabelService: "memory"}}}
	links := []*api.Link{{Name: "memory", Labels: map[string]string{api.LabelService: "memory"}}}

	gs := BuildWith(nil, snaps, links)
	if len(gs) != 1 {
		t.Fatalf("salieron %d grupos, esperaba 1", len(gs))
	}
	if gs[0].Link == nil || gs[0].Snapshot == nil {
		t.Errorf("el grupo debería llevar snapshot Y enlace: %+v", gs[0])
	}
}

// EL IMPORTANTE. Las descripciones y los nombres de las herramientas los escribe
// el SERVIDOR MCP, que este proyecto declara hostil por diseño. Viajan a la
// pagina dentro de un JSON embebido en un <script>, asi que la propiedad que hay
// que sostener es concreta: nada de lo que escriba el invitado puede CERRAR ese
// bloque ni abrir una etiqueta.
//
// Go lo da gratis —json.Marshal escapa < > & a \u003c por defecto— y justamente
// por eso hay que atarlo: basta un SetEscapeHTML(false) puesto para "que el JSON
// se lea mejor" para abrir un agujero en el panel que mira su dueño.
func TestLoQueEscribeElInvitadoNoPuedeRomperLaPagina(t *testing.T) {
	snaps := []*api.Snapshot{{
		Name:   "malicioso",
		Labels: map[string]string{api.LabelService: "malicioso"},
		Tools: []api.ToolSpec{{
			Name:        `</script><script>alert("xss")</script>`,
			Description: `<img src=x onerror="alert(1)">`,
		}},
	}}

	html := RenderMap(&api.Info{}, BuildWith(nil, snaps, nil), "http://x", time.Now(), "")

	// La pagina abre y cierra sus propios bloques. Si el invitado pudiera cerrar
	// uno, habria MAS cierres que aperturas.
	if aperturas, cierres := strings.Count(html, "<script"), strings.Count(html, "</script>"); cierres > aperturas {
		t.Errorf("hay %d cierres de <script> para %d aperturas: el invitado cerró el bloque", cierres, aperturas)
	}
	// Y su payload tiene que estar, escapado: ocultarlo perdería información.
	if !strings.Contains(html, `\u003c/script\u003e`) {
		t.Error("el nombre de la herramienta no aparece escapado; o se perdió, o no se escapó")
	}
	for _, crudo := range []string{`<script>alert`, `<img src=x`} {
		if strings.Contains(html, crudo) {
			t.Errorf("aparece %q SIN escapar", crudo)
		}
	}
}

func TestHumanYAgeDicenAlgoLegible(t *testing.T) {
	for _, c := range []struct {
		b      int64
		quiero string
	}{{0, "0"}, {512, "512"}, {1 << 20, "1"}, {3 << 30, "3"}} {
		if got := human(c.b); !strings.Contains(got, c.quiero) {
			t.Errorf("human(%d) = %q, esperaba que contuviera %q", c.b, got, c.quiero)
		}
	}
	ahora := time.Now()
	if got := age(ahora.Add(-90*time.Minute), ahora); got == "" {
		t.Error("age devolvió vacío")
	}
}

func TestTruncNoParteAMediasSinDecirlo(t *testing.T) {
	largo := strings.Repeat("x", 200)
	got := trunc(largo, 20)
	if len(got) > 25 {
		t.Errorf("trunc no recortó: %d caracteres", len(got))
	}
	if corto := trunc("corto", 20); corto != "corto" {
		t.Errorf("trunc tocó algo que cabía: %q", corto)
	}
}
