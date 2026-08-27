package main

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

func catalogo(nombres ...string) []api.ToolSpec {
	out := make([]api.ToolSpec, len(nombres))
	for i, n := range nombres {
		out[i] = api.ToolSpec{Name: n}
	}
	return out
}

func TestPruebaAplicableSoloDondeHayAlgoQueEjercitar(t *testing.T) {
	if _, _, _, hay := pruebaAplicable(catalogo("browser_navigate", "browser_click")); !hay {
		t.Error("un catalogo con browser_navigate deberia tener prueba")
	}
	// La mayoria de servicios no tienen ninguna, y eso NO es un fallo: la sonda
	// se queda en el handshake y hay que ser honesto sobre lo que comprobo.
	if _, _, _, hay := pruebaAplicable(catalogo("read_file", "write_file")); hay {
		t.Error("un servidor de ficheros no deberia tener prueba de navegador")
	}
	if _, _, _, hay := pruebaAplicable(nil); hay {
		t.Error("un catalogo vacio no deberia tener prueba")
	}
}

// posterFalso devuelve siempre el mismo cuerpo a la llamada de herramienta.
func posterFalso(respuestaTool string) poster {
	return func(sid, body string) (string, []byte, error) {
		switch {
		case strings.Contains(body, `"initialize"`):
			return "sid-1", []byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"x"}}}`), nil
		case strings.Contains(body, `"tools/call"`):
			return sid, []byte(respuestaTool), nil
		default:
			return sid, []byte(`{}`), nil
		}
	}
}

func TestEjercitarDetectaElFalloQueVieneDentroDeUnaRespuestaCorrecta(t *testing.T) {
	// El caso real: HTTP 200, JSON-RPC valido, y la herramienta diciendo que no
	// puede trabajar. Sin mirar isError, la sonda daria esto por sano — que es
	// exactamente como playwright estuvo marcado "healthy" sin poder navegar.
	conIsError := `{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[` +
		`{"type":"text","text":"### Error\nTimeoutError: Timeout 30000ms exceeded."}]}}`
	err := ejercitar(posterFalso(conIsError), "sid-1", "browser_navigate", `{"url":"about:blank"}`)
	if err == nil {
		t.Fatal("isError:true se dio por bueno")
	}
	if !strings.Contains(err.Error(), "TimeoutError") {
		t.Errorf("el error no dice que paso: %v", err)
	}
	// Y en una linea: un salto de linea crudo romperia la tabla de mcp health.
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("el error lleva saltos de linea crudos: %q", err)
	}
}

func TestEjercitarAceptaUnaLlamadaQueFunciona(t *testing.T) {
	ok := `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"### Page\n- Page URL: about:blank"}]}}`
	if err := ejercitar(posterFalso(ok), "sid-1", "browser_navigate", `{"url":"about:blank"}`); err != nil {
		t.Errorf("una llamada correcta se reporto como fallo: %v", err)
	}
}

func TestEjercitarPropagaUnErrorDeJSONRPC(t *testing.T) {
	rpcErr := `{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"unknown tool"}}`
	err := ejercitar(posterFalso(rpcErr), "sid-1", "browser_navigate", `{}`)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("error de JSON-RPC no propagado: %v", err)
	}
}

// Sin sesion no se puede ejercer nada, y hay que decirlo en vez de abrir una
// nueva: la segunda sesion es justo lo que rechazan los servicios con la
// memoria por defecto.
func TestEjercitarSinSesionLoDice(t *testing.T) {
	err := ejercitar(posterFalso(`{}`), "", "browser_navigate", `{}`)
	if err == nil {
		t.Fatal("sin sesion deberia fallar")
	}
	if !strings.Contains(err.Error(), "session") {
		t.Errorf("el error no dice que falta la sesion: %v", err)
	}
}
