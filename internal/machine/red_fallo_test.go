package machine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	knet "github.com/juan52878911/kindling/internal/net"
)

// Un Setup que falla a medias (el veth creado, una regla que no entra) dejaba
// el namespace y el veth montados: ni Run ni runFrom ni Thaw los desmontaban,
// y el barrido de huérfanos solo corre al arrancar el daemon. montarRed los
// desmonta él mismo y retira la marca de propiedad.
func TestMontarRedQueFallaNoDejaLaRedAMedias(t *testing.T) {
	m := newTestManager(t)
	var desmontadas []string
	oldM, oldD := montarRedHost, desmontarRedHost
	t.Cleanup(func() { montarRedHost, desmontarRedHost = oldM, oldD })
	montarRedHost = func(n *knet.Net, _ knet.Egress, _ []string, _ int) error {
		// Lo que haría un Setup que se queda a medias: la marca ya está puesta.
		if !m.redEsPropia(n.NS) {
			t.Errorf("la marca de %s no estaba antes de montar", n.NS)
		}
		return errors.New("iptables: no chain")
	}
	desmontarRedHost = func(n *knet.Net) { desmontadas = append(desmontadas, n.NS) }

	id := "c0ffee0000000001"
	n := knet.Plan(7, id)
	if err := m.montarRed(n, id, knet.EgressNone, nil); err == nil {
		t.Fatal("montarRed no devolvió el error de Setup")
	}
	if len(desmontadas) != 1 || desmontadas[0] != n.NS {
		t.Fatalf("desmontadas = %v; quería desmontar %s una vez", desmontadas, n.NS)
	}
	if _, err := os.Stat(filepath.Join(m.dirRedPropia(), n.NS)); !os.IsNotExist(err) {
		t.Fatalf("la marca de %s sigue ahí (%v): el barrido creería que es suya y viva", n.NS, err)
	}
	if _, ok := m.redMontada.Load(id); ok {
		t.Fatal("la red quedó apuntada como montada")
	}
}
