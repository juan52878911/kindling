package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
