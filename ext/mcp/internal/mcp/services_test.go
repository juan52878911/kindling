package mcp

import (
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/von"
)

// Los dorados que construye `kling ai` viven en el mismo daemon que los
// servicios MCP, pero no lo son: no deben salir en `kling mcp ls` ni en
// /services con 0 herramientas.
func TestServicesDejaFueraLosDoradosDeLaIA(t *testing.T) {
	snaps := []*api.Snapshot{
		{Name: "fs", Labels: map[string]string{api.LabelService: "fs"}},
		{Name: "chispa-room", Labels: map[string]string{api.LabelService: "chispa-room", chispa.LabelTask: "chispa-room"}},
		{Name: "x86-qwen15-q4", Labels: map[string]string{von.LabelModel: "qwen2.5-1.5b-instruct:q4_k_m"}},
		{Name: "x86-enc-e5", Labels: map[string]string{von.LabelModel: "e5-small:q8_0", von.LabelKind: von.KindEmbed}},
		{Name: "sin-etiquetas"},
	}
	got := Services(snaps)
	if len(got) != 2 || got[0].Name != "fs" || got[1].Name != "sin-etiquetas" {
		t.Fatalf("Services = %v", names(got))
	}
	if IsService(nil) {
		t.Fatal("nil no es un servicio")
	}
	if len(Services(nil)) != 0 {
		t.Fatal("sin snapshots, sin servicios")
	}
}

func names(snaps []*api.Snapshot) []string {
	var out []string
	for _, s := range snaps {
		out = append(out, s.Name)
	}
	return out
}
