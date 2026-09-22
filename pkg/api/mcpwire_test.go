package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPPayloadSacaLaRespuestaYNoLaNotificacionQueVaDelante(t *testing.T) {
	casos := []struct {
		nombre string
		raw    string
		quiero string
	}{
		{
			"JSON suelto",
			`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
			`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
		},
		{
			"un solo evento SSE",
			"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n",
			`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
		},
		{
			// El caso que rompia: el servidor avisa del progreso ANTES del
			// resultado. Quedarse con el primer data: devolvia la notificacion,
			// el llamador la parseaba sin error con Result vacio, y el resultado
			// se perdia sin que nada fallara.
			"notificacion de progreso antes del resultado",
			"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n" +
				"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n",
			`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
		},
		{
			"dos notificaciones antes",
			"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\n" +
				"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
				"data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"tools\":[]}}\n\n",
			`{"jsonrpc":"2.0","id":7,"result":{"tools":[]}}`,
		},
		{
			"un error tambien es respuesta",
			"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
				"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-32602,\"message\":\"malo\"}}\n\n",
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"malo"}}`,
		},
		{
			// Si NINGUNO parece respuesta se devuelve el primero, como siempre:
			// mejor que el llamador vea algo raro a que se pierda aqui.
			"solo notificaciones: se devuelve la primera",
			"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n",
			`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`,
		},
	}

	for _, c := range casos {
		got := string(MCPPayload([]byte(c.raw)))
		if got != c.quiero {
			t.Errorf("%s:\n  devolvio %s\n  esperaba %s", c.nombre, got, c.quiero)
		}
	}
}

// La consecuencia real, expresada como la ve el llamador: un unmarshal que
// TIENE EXITO con el resultado vacio y sin error. Eso es lo que llegaba al
// modelo.
func TestUnaNotificacionColadaSeriaUnResultadoVacioSinError(t *testing.T) {
	raw := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"lo importante\"}]}}\n\n"

	var res struct {
		Result *struct {
			Content []struct{ Text string } `json:"content"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(MCPPayload([]byte(raw)), &res); err != nil {
		t.Fatalf("no se pudo leer: %v", err)
	}
	if res.Result == nil || len(res.Result.Content) == 0 {
		t.Fatal("resultado vacio y err=nil: el contenido real se perdio en silencio")
	}
	if !strings.Contains(res.Result.Content[0].Text, "lo importante") {
		t.Errorf("llego otro contenido: %q", res.Result.Content[0].Text)
	}
}
