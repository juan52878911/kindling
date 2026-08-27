package api

import (
	"bytes"
	"encoding/json"
)

// Streamable HTTP deja al servidor elegir cómo contesta: puede devolver el
// JSON-RPC tal cual, o envolverlo en un único evento SSE. Los servidores que
// hablan HTTP de forma nativa suelen hacer lo segundo, así que quien lea una
// respuesta MCP tiene que aceptar las dos formas.
//
// AcceptMCP es lo que hay que mandar en Accept para que no rechacen la petición:
// la especificación exige ofrecer ambos tipos, y los servidores estrictos
// responden 406 si falta uno.
const AcceptMCP = "application/json, text/event-stream"

// MCPPayload saca el JSON-RPC de una respuesta, venga suelto o dentro de un
// evento SSE.
func MCPPayload(raw []byte) []byte {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] == '{' || t[0] == '[' {
		return t
	}
	// Se busca la RESPUESTA, no el primer evento. Un servidor puede emitir
	// notifications/progress o de logging antes del resultado, y quedarse con el
	// primer `data:` devolvia entonces la notificacion. El llamador la parsea SIN
	// ERROR —es JSON-RPC valido—, con Result vacio y Error nil: el modelo recibe
	// una respuesta vacia, nada falla en ningun sitio, y el resultado de verdad
	// se tira a la basura.
	var primero []byte
	for _, line := range bytes.Split(t, []byte("\n")) {
		v, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		v = bytes.TrimSpace(v)
		if primero == nil {
			primero = v
		}
		if esRespuesta(v) {
			return v
		}
	}
	// Ningun evento parece una respuesta: se devuelve el primero, que es lo que
	// se hacia siempre. Asi un servidor que conteste algo con otra forma sigue
	// llegando al llamador para que decida el, en vez de perderse aqui.
	if primero != nil {
		return primero
	}
	return t
}

// esRespuesta distingue la respuesta de una notificacion: una respuesta JSON-RPC
// lleva `id` y `result` o `error`; una notificacion no lleva `id`.
func esRespuesta(ev []byte) bool {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(ev, &m) != nil {
		return false
	}
	if len(m.ID) == 0 || string(m.ID) == "null" {
		return false
	}
	return len(m.Result) > 0 || len(m.Error) > 0
}
