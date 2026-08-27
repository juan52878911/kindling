package main

// La sonda que SI puede fallar.
//
// `tools/list` lo contesta el servidor MCP sin tocar por dentro nada de lo que
// de verdad usa. Un servicio de navegador lo responde perfectamente con el
// navegador inservible, y eso es literalmente lo que paso aqui: `playwright`
// figuraba "healthy" mientras `browser_navigate` moria por timeout del
// websocket CDP, primero por falta de CPU y despues por un resolver roto.
//
// Una comprobacion que no puede fallar no es una comprobacion. Cuando el
// catalogo lo permite, aqui se LLAMA a una herramienta de verdad.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/juan52878911/kindling/internal/api"
)

// pruebasDeVida son herramientas conocidas, invocables sin efectos, que
// ejercitan la parte fragil de su servicio.
//
// `about:blank` NO toca la red a proposito. Lo que se comprueba es que el
// puente hable CDP con el navegador; meter una URL real ataria la salud de tu
// sistema a que un sitio de terceros siga en pie, y convertiria una caida ajena
// en una alarma tuya.
var pruebasDeVida = []struct {
	herramienta string
	args        string
	que         string
}{
	{"browser_navigate", `{"url":"about:blank"}`, "el navegador responde por CDP"},
}

// pruebaAplicable dice que prueba le toca a este catalogo, si es que hay alguna.
// La mayoria de servicios no tienen ninguna, y para esos la sonda se queda como
// estaba: honesto es decir que solo se comprobo el handshake.
func pruebaAplicable(tools []api.ToolSpec) (herramienta, args, que string, hay bool) {
	for _, p := range pruebasDeVida {
		for _, t := range tools {
			if t.Name == p.herramienta {
				return p.herramienta, p.args, p.que, true
			}
		}
	}
	return "", "", "", false
}

// ejercitar hace el handshake y llama a una herramienta. Devuelve error si la
// llamada falla O si el servidor contesta con isError, que es como los
// servidores MCP reportan un fallo de la herramienta sin fallar el JSON-RPC.
func ejercitar(post poster, herramienta, args string) error {
	sid, _, err := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"kling","version":"1"}}}`)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	_, _, _ = post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	cuerpo := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
		herramienta, args)
	_, raw, err := post(sid, cuerpo)
	if err != nil {
		return fmt.Errorf("%s: %w", herramienta, err)
	}

	var res struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(api.MCPPayload(raw), &res); err != nil {
		return fmt.Errorf("%s: respuesta ilegible: %w", herramienta, err)
	}
	if res.Error != nil {
		return fmt.Errorf("%s: %s", herramienta, res.Error.Message)
	}
	// isError es la parte que se olvida. El JSON-RPC va bien, el HTTP va bien, y
	// la herramienta ha fallado: sin mirar esto, la sonda daria por sano un
	// servicio que acaba de decir que no puede trabajar.
	if res.Result.IsError {
		return fmt.Errorf("%s: %s", herramienta, resumirContenido(res.Result.Content))
	}
	return nil
}

func resumirContenido(c []struct {
	Text string `json:"text"`
}) string {
	if len(c) == 0 {
		return "la herramienta devolvio un error sin texto"
	}
	t := strings.TrimSpace(c[0].Text)
	t = strings.ReplaceAll(t, "\n", " | ")
	if len(t) > 220 {
		t = t[:220] + "…"
	}
	return t
}
