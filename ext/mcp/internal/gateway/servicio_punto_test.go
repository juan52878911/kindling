package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
)

// Un servicio con punto en el nombre (de antes de validarlo, o con la etiqueta
// service puesta a mano) no entra en /mcp/_all: ahí "a.b.c" se parte por el
// primer punto, y el servicio "a" con la herramienta "b.c" se haría pasar por
// la herramienta c de "a.b". Su ruta propia (/mcp/a.b) no cambia.
func TestAgregadorSinServiciosConPunto(t *testing.T) {
	snap := func(name, service string) *api.Snapshot {
		return mcp.WithTools(&api.Snapshot{Name: name, Labels: map[string]string{api.LabelService: service}},
			[]mcp.ToolSpec{{Name: "c"}})
	}
	sock := mockDaemon(t, []*api.Snapshot{snap("snap-a", "a"), snap("snap-ab", "a.b")},
		[]*mcp.Link{{Name: "ext", URL: "http://127.0.0.1:1/mcp", Labels: map[string]string{api.LabelService: "x.y"}}})
	a := newAggregator(New(api.NewClient(sock), 5*time.Minute, false, 0, ""), false)
	svcs, err := a.cat.services(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 1 || svcs[0] != "a" {
		t.Fatalf("services = %v, quiero [a]", svcs)
	}
}
