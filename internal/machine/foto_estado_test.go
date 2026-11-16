package machine

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/juan52878911/kindling/internal/esquema"
	"github.com/juan52878911/kindling/pkg/api"
)

// fotoComoAntes es el state.json que escribía escribirFoto antes de P4.
func fotoComoAntes(t *testing.T, list []api.Machine, schema int) []byte {
	t.Helper()
	b, err := json.MarshalIndent(struct {
		esquema.Cabecera
		Machines []api.Machine `json:"machines"`
	}{esquema.Cabecera{Schema: schema}, list}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// P4: la foto serializada por máquinas (y en paralelo con muchas) es la
// misma, byte a byte, que la de siempre.
func TestSerializarFotoIgualQueAntesYAlDia(t *testing.T) {
	m := benchManager(3)
	var list []api.Machine
	for _, mc := range m.byID {
		list = append(list, *mc.Clone())
	}
	for _, c := range []struct {
		nombre string
		list   []api.Machine
	}{{"nil", nil}, {"vacía", []api.Machine{}}, {"una", list[:1]}, {"varias", list}} {
		got, err := serializarFoto(c.list, 3)
		if err != nil {
			t.Fatal(err)
		}
		if want := fotoComoAntes(t, c.list, 3); !bytes.Equal(got, want) {
			t.Fatalf("%s: la foto cambió de forma:\n%s\nquería:\n%s", c.nombre, got, want)
		}
	}
	// Con muchas, en paralelo, igual.
	grande := benchManager(1000)
	var muchas []api.Machine
	for _, mc := range grande.byID {
		muchas = append(muchas, *mc.Clone())
	}
	got, err := serializarFoto(muchas, 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := fotoComoAntes(t, muchas, 3); !bytes.Equal(got, want) {
		t.Fatal("con 1000 máquinas la foto cambió de forma")
	}
}
