package api

import "bytes"

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
	for _, line := range bytes.Split(t, []byte("\n")) {
		if v, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			return bytes.TrimSpace(v)
		}
	}
	return t
}
