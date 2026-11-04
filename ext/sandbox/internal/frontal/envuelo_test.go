package frontal

import (
	"context"
	"fmt"
	"testing"
)

// enVuelo no guarda contadores a cero: tras reservar y liberar, el mapa queda
// vacío por muchos pares tenant/plantilla que hayan pasado.
func TestEnVueloSeVaciaAlLiberar(t *testing.T) {
	s := &Servidor{enVuelo: map[string]int{}}
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		ten := &Tenant{Nombre: fmt.Sprintf("t%d", i)}
		pl := fmt.Sprintf("pl%d", i)
		if err := s.reservar(ctx, ten, pl); err != nil {
			t.Fatal(err)
		}
		if err := s.reservar(ctx, ten, pl); err != nil {
			t.Fatal(err)
		}
		if s.enVuelo[ten.Nombre] != 2 {
			t.Fatalf("en vuelo %d; quería 2", s.enVuelo[ten.Nombre])
		}
		s.liberar(ten, pl)
		if s.enVuelo[ten.Nombre] != 1 {
			t.Fatalf("en vuelo %d tras liberar una; quería 1", s.enVuelo[ten.Nombre])
		}
		s.liberar(ten, pl)
	}
	if len(s.enVuelo) != 0 {
		t.Fatalf("enVuelo guarda %d claves a cero", len(s.enVuelo))
	}
}
