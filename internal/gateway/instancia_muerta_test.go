package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Medido en fc-test: el recolector de disco del daemon retira instancias
// dormidas para hacer sitio, y el gateway se quedaba con la entrada apuntando a
// una IP muerta PARA SIEMPRE. Todas las peticiones siguientes daban
// "dial tcp 172.30.1.74:8080: i/o timeout" y nada lo arreglaba.
func TestOlvidarInstanciaDejaSitioParaQueSeCreeOtra(t *testing.T) {
	g := &Gateway{
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

// La salud se decide por el RESULTADO. Antes se anotaba el exito al conseguir la
// instancia, asi que un servicio que devolvia 502 en cada llamada REAFIRMABA que
// estaba sano en cada intento: medido en vivo con `memory`, health="healthy"
// escrito en el mismo instante en que yo recibia 502 de ese servicio.
func TestCodigoVistoRecuerdaConQueSeRespondio(t *testing.T) {
	casos := []struct {
		code int
		sano bool
	}{
		{http.StatusOK, true},
		{http.StatusBadRequest, true},           // error del cliente: el servicio SI contesto
		{http.StatusBadGateway, false},          // no contesto
		{http.StatusGatewayTimeout, false},      // tampoco
		{http.StatusInsufficientStorage, false}, // 507: no se pudo servir
	}
	for _, c := range casos {
		rec := httptest.NewRecorder()
		cw := &codigoVisto{ResponseWriter: rec, code: http.StatusOK}
		cw.WriteHeader(c.code)
		if cw.code != c.code {
			t.Errorf("codigoVisto guardo %d, esperaba %d", cw.code, c.code)
		}
		if (cw.code < 500) != c.sano {
			t.Errorf("codigo %d: sano=%v, esperaba %v", c.code, cw.code < 500, c.sano)
		}
		if rec.Code != c.code {
			t.Errorf("el codigo no llego al cliente: %d", rec.Code)
		}
	}
}

// Sin reenviar Flush, envolver el ResponseWriter dejaria las respuestas en el
// buffer hasta cerrar la conexion — y el gateway sirve SSE.
func TestCodigoVistoReenviaElFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := &codigoVisto{ResponseWriter: rec, code: http.StatusOK}
	if _, ok := interface{}(cw).(http.Flusher); !ok {
		t.Fatal("codigoVisto no es un http.Flusher: el streaming se quedaria bufereado")
	}
	_, _ = cw.Write([]byte("data: x\n\n"))
	cw.Flush()
	if !rec.Flushed {
		t.Error("el Flush no llego al ResponseWriter de abajo")
	}
}
