package machine

import (
	"reflect"
	"testing"

	"github.com/juan52878911/kindling/internal/fc"
)

func TestRutasDeConfianza(t *testing.T) {
	ro := func(p string) fc.RepointDrive { return fc.RepointDrive{Path: p, ReadOnly: true} }
	rw := func(p string) fc.RepointDrive { return fc.RepointDrive{Path: p} }
	casos := []struct {
		nombre string
		capa   string
		vols   []discoDeConfianza
		want   map[string]fc.RepointDrive
	}{
		{"sin volúmenes", "", nil, map[string]fc.RepointDrive{"rootfs": ro("/b"), "overlay": rw("/o")}},
		{"dos volúmenes y una copia", "", []discoDeConfianza{
			{"volume0", "/v/a", false}, {"volume1", "/v/b", true}, {"volume2", "/m/share0.ext4", true},
		}, map[string]fc.RepointDrive{"rootfs": ro("/b"), "overlay": rw("/o"),
			"volume0": rw("/v/a"), "volume1": ro("/v/b"), "volume2": ro("/m/share0.ext4")}},
		{"con capa", "/l", nil, map[string]fc.RepointDrive{"rootfs": ro("/b"), "overlay": rw("/o"), "svclayer": ro("/l")}},
		{"volumen legado con los dos ids", "", []discoDeConfianza{{"volume0", "/v/a", false}},
			map[string]fc.RepointDrive{"rootfs": ro("/b"), "overlay": rw("/o"), "volume0": rw("/v/a"), "volume": rw("/v/a")}},
	}
	for _, c := range casos {
		rp := rutasDeConfianza("/k", "/b", c.capa, "/o", c.vols)
		if rp.Kernel != "/k" || !reflect.DeepEqual(rp.Drives, c.want) {
			t.Errorf("%s: %+v", c.nombre, rp)
		}
	}
}
