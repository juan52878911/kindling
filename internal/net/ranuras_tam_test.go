package net

import (
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

func tomas(a *tamAdaptativo, n int, en time.Time) {
	for i := 0; i < n; i++ {
		a.anotar(en)
	}
}

func TestTamAdaptativoCreceConLasTomas(t *testing.T) {
	a := &tamAdaptativo{min: 16, max: 64}
	if got := a.objetivo(t0); got != 16 {
		t.Fatalf("en reposo = %d, quería 16", got)
	}
	tomas(a, 40, t0)
	if got := a.objetivo(t0.Add(3 * time.Second)); got != 40 {
		t.Fatalf("tras 40 tomas = %d, quería 40", got)
	}
	tomas(a, 60, t0.Add(4*time.Second))
	if got := a.objetivo(t0.Add(5 * time.Second)); got != 64 {
		t.Fatalf("tras 100 tomas = %d, quería 64 (máximo)", got)
	}
	// Pocas tomas no bajan del mínimo.
	b := &tamAdaptativo{min: 16, max: 64}
	tomas(b, 5, t0)
	if got := b.objetivo(t0); got != 16 {
		t.Fatalf("tras 5 tomas = %d, quería 16", got)
	}
}

func TestTamAdaptativoSeMantieneUnMinutoYBajaDeDosEnDos(t *testing.T) {
	a := &tamAdaptativo{min: 16, max: 64}
	tomas(a, 100, t0)
	a.objetivo(t0)
	if got := a.encoger(t0.Add(59 * time.Second)); got != 64 {
		t.Fatalf("a los 59 s = %d, quería 64 (se mantiene)", got)
	}
	ahora := t0.Add(61 * time.Second)
	want := 62
	for i := 0; i < 24; i++ {
		if got := a.encoger(ahora); got != want {
			t.Fatalf("vuelta %d = %d, quería %d", i, got, want)
		}
		want -= 2
		ahora = ahora.Add(15 * time.Second)
	}
	if got := a.encoger(ahora); got != 16 {
		t.Fatalf("en el mínimo = %d, quería 16", got)
	}
	if got := a.objetivo(ahora); got != 16 {
		t.Fatalf("objetivo en el mínimo = %d", got)
	}
}

func TestTamAdaptativoSinMargenNoSeMueve(t *testing.T) {
	a := &tamAdaptativo{min: 16, max: 8}
	tomas(a, 100, t0)
	if got := a.objetivo(t0); got != 16 {
		t.Fatalf("máximo < mínimo: objetivo = %d, quería 16", got)
	}
	if got := a.encoger(t0.Add(time.Hour)); got != 16 {
		t.Fatalf("máximo < mínimo: encoger = %d, quería 16", got)
	}
}

func TestTamAdaptativoNoOscilaConElFondoLleno(t *testing.T) {
	// Todas las tomas cuentan, también las que sí encontraron ranura.
	a := &tamAdaptativo{min: 16, max: 64}
	for s := 0; s < 55; s += 5 {
		tomas(a, 5, t0.Add(time.Duration(s)*time.Second))
	}
	if got := a.objetivo(t0.Add(55 * time.Second)); got != 55 {
		t.Fatalf("objetivo = %d, quería 55", got)
	}
}
