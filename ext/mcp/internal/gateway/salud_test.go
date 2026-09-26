package gateway

import "testing"

// El gateway anota la salud SOLO cuando cambia.
//
// Las dos mitades importan por razones distintas. Anotar de más convierte un
// camino caliente en una escritura a disco por petición atendida. Anotar de menos
// deja el meta mintiendo: es lo que dejó nueve servicios con "not probed" durante
// 26 horas mientras cada llamada devolvía 502.
func TestLaSaludSoloSeAnotaCuandoCambia(t *testing.T) {
	g := &Gateway{}
	var anotadas []bool
	registrar := func(svc string, sano bool) {
		if g.saludCambio(svc, sano) {
			anotadas = append(anotadas, sano)
		}
	}

	registrar("a", true)  // 1ª noticia: se anota
	registrar("a", true)  // repetida: no
	registrar("a", true)  // repetida: no
	registrar("a", false) // se rompe: se anota
	registrar("a", false) // repetida: no
	registrar("a", true)  // se recupera: se anota

	if got := []bool{true, false, true}; len(anotadas) != len(got) {
		t.Fatalf("esperaba %d anotaciones (sano, roto, recuperado), hubo %d: %v",
			len(got), len(anotadas), anotadas)
	}
	for i, w := range []bool{true, false, true} {
		if anotadas[i] != w {
			t.Errorf("anotación %d = %v, esperaba %v", i, anotadas[i], w)
		}
	}
}

// Cada servicio lleva su propia cuenta: que uno esté roto no debe silenciar al
// siguiente ni al revés.
func TestLaSaludSeLlevaPorServicio(t *testing.T) {
	g := &Gateway{}
	if !g.saludCambio("a", false) {
		t.Fatal("la primera noticia de 'a' debería anotarse")
	}
	if !g.saludCambio("b", false) {
		t.Error("'b' es otro servicio: su primera noticia también debería anotarse")
	}
	if g.saludCambio("a", false) {
		t.Error("repetir el estado de 'a' no debería anotarse")
	}
}

// anotarExito y anotarFallo son las dos puertas de entrada, y deben mover el
// estado recordado en direcciones opuestas.
func TestExitoYFalloMuevenElEstado(t *testing.T) {
	g := &Gateway{}
	g.anotarExito("s")
	g.saludMu.Lock()
	sano := g.saludVista["s"]
	g.saludMu.Unlock()
	if !sano {
		t.Error("tras un éxito el servicio debería quedar sano")
	}
	if !g.saludCambio("s", false) {
		t.Error("pasar a roto es un cambio y debería anotarse")
	}
}
