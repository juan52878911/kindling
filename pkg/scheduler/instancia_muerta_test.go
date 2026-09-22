package scheduler

import (
	"testing"
)

// Medido en fc-test: el recolector de disco del daemon retira instancias
// dormidas para hacer sitio, y el gateway se quedaba con la entrada apuntando a
// una IP muerta PARA SIEMPRE. Todas las peticiones siguientes daban
// "dial tcp 172.30.1.74:8080: i/o timeout" y nada lo arreglaba.
func TestOlvidarInstanciaDejaSitioParaQueSeCreeOtra(t *testing.T) {
	g := &Scheduler{
		services: map[string]*entry{},
		extra:    map[string][]*entry{},
	}
	g.services["memory"] = &entry{machineID: "m1", ip: "172.30.1.74"}
	g.extra["memory"] = []*entry{{machineID: "m2"}, {machineID: "m3"}}

	g.olvidarInstancia("memory", "m1")
	if g.services["memory"] != nil {
		t.Error("la primaria muerta sigue ahi; la proxima peticion volveria a marcarla")
	}
	// Las replicas vivas NO se tocan.
	if len(g.extra["memory"]) != 2 {
		t.Errorf("se perdieron replicas vivas: quedan %d de 2", len(g.extra["memory"]))
	}

	g.olvidarInstancia("memory", "m3")
	if len(g.extra["memory"]) != 1 || g.extra["memory"][0].machineID != "m2" {
		t.Errorf("no se retiro la replica correcta: %+v", g.extra["memory"])
	}

	// Olvidar algo que no esta no debe romper nada ni borrar de mas.
	g.olvidarInstancia("memory", "no-existe")
	if len(g.extra["memory"]) != 1 {
		t.Error("olvidar un id desconocido toco lo que no debia")
	}
}
