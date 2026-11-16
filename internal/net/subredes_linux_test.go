package net

import (
	"slices"
	"testing"
)

func comoTexto(ds []DireccionHost) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.If+" "+d.IP.String())
	}
	slices.Sort(out)
	return out
}

// P4: un solo volcado de direcciones, sea cual sea el número de interfaces,
// y las mismas direcciones que interfaz a interfaz.
func TestDireccionesHostUnSoloVolcado(t *testing.T) {
	viejo := volcarDirecciones
	t.Cleanup(func() { volcarDirecciones = viejo })
	volcados := 0
	volcarDirecciones = func() ([]byte, error) { volcados++; return viejo() }

	ds, err := direccionesHost()
	if err != nil {
		t.Fatal(err)
	}
	if volcados != 1 {
		t.Errorf("%d volcados de direcciones, quería 1", volcados)
	}
	ref, err := direccionesPorInterfaz()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := comoTexto(ds), comoTexto(ref); !slices.Equal(got, want) || len(got) == 0 {
		t.Fatalf("direcciones =\n%v\nquería (interfaz a interfaz)\n%v", got, want)
	}
}

// Para medir en un netns con muchas interfaces (el lab, con 500 dummy):
// go test -run x -bench DireccionesHost ./internal/net
func BenchmarkDireccionesHostVolcado(b *testing.B) {
	for b.Loop() {
		if _, err := direccionesHost(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDireccionesHostPorInterfaz(b *testing.B) {
	for b.Loop() {
		if _, err := direccionesPorInterfaz(); err != nil {
			b.Fatal(err)
		}
	}
}
