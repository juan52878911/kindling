package gateway

import "testing"

// La fórmula de capacidad debe coincidir con deriveMaxSessions del puente: si
// diverge, el gateway escala de más o de menos (el 400 lo salva, pero mal).
func TestGwMaxSessions(t *testing.T) {
	cases := []struct{ mem, want int }{
		{0, 1}, {-5, 1}, {128, 1}, {192, 1}, {256, 1},
		{320, 2}, {512, 5}, {1024, 13}, {2240, 32}, {4096, 32},
	}
	for _, c := range cases {
		if got := gwMaxSessions(c.mem); got != c.want {
			t.Errorf("gwMaxSessions(%d) = %d, quería %d", c.mem, got, c.want)
		}
	}
}

// La contabilidad del pool (primaria + réplicas) es lo que hace que una sesión
// pegajosa a una réplica vuelva a ella y que el segador/evict no se equivoquen de
// instancia. Se prueba con estructuras montadas a mano, sin daemon.
func TestScaleOutBookkeeping(t *testing.T) {
	g := &Gateway{
		services: map[string]*entry{},
		extra:    map[string][]*entry{},
		routes:   map[string]*sessionRoute{},
	}
	prim := &entry{machineID: "m-prim", maxSessions: 1}
	r1 := &entry{machineID: "m-r1", maxSessions: 1}
	r2 := &entry{machineID: "m-r2", maxSessions: 1}
	g.services["svc"] = prim
	g.extra["svc"] = []*entry{r1, r2}
	g.routes["s1"] = &sessionRoute{service: "svc", machineID: "m-prim"}
	g.routes["s2"] = &sessionRoute{service: "svc", machineID: "m-r1"}

	// entriesLocked = primaria + réplicas.
	if got := len(g.entriesLocked("svc")); got != 3 {
		t.Errorf("entriesLocked = %d, quería 3", got)
	}
	// Encuentra una réplica por machineID (lo que arregla la sesión sticky a réplica).
	if e := g.entryByMachineLocked("svc", "m-r2"); e == nil || e.machineID != "m-r2" {
		t.Error("entryByMachineLocked no encontró la réplica r2")
	}
	if e := g.entryByMachineLocked("svc", "no-existe"); e != nil {
		t.Error("entryByMachineLocked encontró una máquina inexistente")
	}
	// Cuenta de sesiones por instancia, derivada de las rutas.
	if n := g.sessionCountLocked("m-prim"); n != 1 {
		t.Errorf("sessionCount(m-prim) = %d, quería 1", n)
	}
	if n := g.sessionCountLocked("m-r2"); n != 0 {
		t.Errorf("sessionCount(m-r2) = %d, quería 0", n)
	}

	// Quitar una réplica: desaparece del pool y la lista encoge.
	g.removeEntryLocked("svc", "m-r1")
	if e := g.entryByMachineLocked("svc", "m-r1"); e != nil {
		t.Error("r1 no se quitó del pool")
	}
	if got := len(g.extra["svc"]); got != 1 {
		t.Errorf("réplicas tras quitar r1 = %d, quería 1", got)
	}
	// Quitar la última réplica: la clave del mapa se borra (no queda slice vacío).
	g.removeEntryLocked("svc", "m-r2")
	if _, ok := g.extra["svc"]; ok {
		t.Error("g.extra['svc'] debería borrarse al quedar sin réplicas")
	}
	// Quitar la primaria por machineID.
	g.removeEntryLocked("svc", "m-prim")
	if _, ok := g.services["svc"]; ok {
		t.Error("la primaria no se quitó de g.services")
	}
	// Quitar algo que no existe no debe entrar en pánico.
	g.removeEntryLocked("svc", "fantasma")
}
