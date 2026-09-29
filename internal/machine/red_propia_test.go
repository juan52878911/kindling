package machine

import (
	stdnet "net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// Dos daemons en el mismo host: el barrido de huérfanos de uno no puede
// llevarse la red de una máquina viva del otro (lo que pasó en el lab: el
// privado borró kl-171ae619 del daemon del sistema).
func TestBarrerNamespacesSoloLosPropios(t *testing.T) {
	sistema := &Manager{root: t.TempDir()}
	privado := &Manager{root: t.TempDir()}

	// El del sistema montó la red de su máquina viva; el privado, la de una
	// máquina que ya no existe (su huérfano) y la de una suya viva.
	sistema.marcarRedPropia("kl-171ae619")
	privado.marcarRedPropia("kl-0000dead")
	privado.marcarRedPropia("kl-0000beef")
	// Y una marca vieja de un namespace que ya no existe.
	privado.marcarRedPropia("kl-00000abc")

	var borrados []string
	antesL, antesB := listarNamespaces, borrarNamespace
	t.Cleanup(func() { listarNamespaces, borrarNamespace = antesL, antesB })
	borrarNamespace = func(ns string) { borrados = append(borrados, ns) }

	// El privado arranca y reconcilia: conoce kl-0000beef y kl-0000cafe, una
	// máquina viva cuya red montó una versión anterior, sin marca.
	listarNamespaces = func() []string {
		return []string{"kl-171ae619", "kl-0000dead", "kl-0000beef", "kl-0000cafe", "kl-legacy01"}
	}
	privado.barrerNamespaces(map[string]bool{"kl-0000beef": true, "kl-0000cafe": true})

	sort.Strings(borrados)
	if len(borrados) != 1 || borrados[0] != "kl-0000dead" {
		t.Fatalf("borrados = %v; solo el huérfano propio (kl-0000dead)", borrados)
	}
	if !sistema.redEsPropia("kl-171ae619") {
		t.Error("la marca del otro daemon no se toca")
	}
	if privado.redEsPropia("kl-0000dead") {
		t.Error("tras barrer un huérfano propio, su marca se va")
	}
	if privado.redEsPropia("kl-00000abc") {
		t.Error("la marca de un namespace que ya no existe se limpia")
	}
	if !privado.redEsPropia("kl-0000beef") {
		t.Error("la marca de una máquina propia viva se queda")
	}
	if !privado.redEsPropia("kl-0000cafe") {
		t.Error("una máquina propia viva sin marca (daemon anterior) la recibe al reconciliar")
	}
	if privado.redEsPropia("kl-legacy01") {
		t.Error("lo que no es de ninguna máquina propia no se marca")
	}
}

func TestMarcaRedNombresValidos(t *testing.T) {
	m := &Manager{root: t.TempDir()}
	m.marcarRedPropia("../../etc/passwd")
	m.marcarRedPropia("kl-../x")
	ents, _ := os.ReadDir(filepath.Join(m.root, "net"))
	if len(ents) != 0 {
		t.Fatalf("nombres no válidos no dejan marca: %v", ents)
	}
	if m.redEsPropia("../net") {
		t.Fatal("un nombre no válido nunca es propio")
	}
}

// Una máquina congelada cuya red se soltó: otro daemon del host tomó su
// subred mientras dormía. Al rehacerla, se muda a otra y se apunta.
func TestRedParaRehacerSeMudaSiOtroDaemonLaTiene(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("en macOS no hay enlaces en el host")
	}
	antesD, antesR := knet.DireccionesHost, knet.DirReservas
	t.Cleanup(func() { knet.DireccionesHost, knet.DirReservas = antesD, antesR })
	knet.DirReservas = filepath.Join(t.TempDir(), "claims")
	ajena := knet.Plan(91, "0badf00d0000") // el veth del otro daemon
	knet.DireccionesHost = func() ([]knet.DireccionHost, error) {
		return []knet.DireccionHost{{If: ajena.HostIf, IP: stdnet.ParseIP(ajena.HostIP)}}, nil
	}

	id := "449cd67c52da0000"
	vieja := knet.Plan(91, id)
	m := &Manager{root: t.TempDir(), byID: map[string]*api.Machine{
		id: {ID: id, Name: "agente", State: api.StateWarm, NetIndex: 91, IP: vieja.NSIP},
	}, netCursor: 91}
	mc := m.byID[id].Clone()

	n, err := m.redParaRehacer(mc)
	if err != nil {
		t.Fatal(err)
	}
	n.SoltarReserva()
	if n.Index == 91 {
		t.Fatal("rehízo la red en la subred del otro daemon")
	}
	if got := m.byID[id]; got.NetIndex != n.Index || got.IP != n.NSIP {
		t.Fatalf("la máquina no apunta su red nueva: index %d ip %s, quiero %d %s", got.NetIndex, got.IP, n.Index, n.NSIP)
	}
	if mc.NetIndex != n.Index {
		t.Fatal("la copia del llamante tampoco")
	}

	// Con la subred libre, conserva la suya.
	knet.DireccionesHost = func() ([]knet.DireccionHost, error) { return nil, nil }
	n2, err := m.redParaRehacer(m.byID[id].Clone())
	if err != nil {
		t.Fatal(err)
	}
	n2.SoltarReserva()
	if n2.Index != n.Index {
		t.Fatalf("con su subred libre se mudó igual: %d → %d", n.Index, n2.Index)
	}
}
