package api

import "slices"

// GuestHealthPath (GET) es la sonda de vida del agente de invitado. A secas
// contesta "ok" como siempre; con Accept: application/json contesta
// GuestHealth, que dice qué agente es y qué sabe hacer. Un agente anterior
// ignora el Accept y sigue contestando "ok": es la forma de reconocerlo.
const GuestHealthPath = "/healthz"

// Capacidades que anuncia el agente en GuestHealth.Caps. Solo se añaden,
// nunca se reusan: el host decide con ellas en vez de sondear la ruta y mirar
// si da 404 o 405.
const (
	GuestCapResync  = "resync"   // POST /resync
	GuestCapReady   = "ready"    // GET /ready
	GuestCapHooks   = "hooks"    // POST /hooks
	GuestCapMemInfo = "meminfo"  // GET /meminfo
	GuestCapVolume  = "volume"   // /volume/{sync,release,acquire}
	GuestCapShare   = "share"    // POST /share/attach
	GuestCapExec    = "exec"     // /exec, /exec/stream, /exec/pty, /files (con kling.exec=1)
	GuestCapMCP     = "mcp"      // el puente MCP: / y /mcp
	GuestCapBootOpt = "boot-opt" // ignora los kling.* y las opciones de volumen que no conoce
	GuestCapService = "service"  // GET /service: el servicio supervisado (guest_service.go)
)

// GuestHealth es la respuesta JSON de GET /healthz.
type GuestHealth struct {
	Status string `json:"status"` // "ok"
	// Agent es el binario: "kling-guest" o "kling-bridge".
	Agent   string   `json:"agent,omitempty"`
	Version string   `json:"version,omitempty"`
	Caps    []string `json:"caps,omitempty"`
}

// GuestAgent es lo que el host sabe del agente de una máquina, visto en su
// /healthz. Version vacía y Caps nil es un agente anterior a v0.18, que no
// anuncia nada: se le sigue tratando como hasta ahora, sondeando por ruta.
type GuestAgent struct {
	Agent   string   `json:"agent,omitempty"`
	Version string   `json:"version,omitempty"`
	Caps    []string `json:"caps,omitempty"`
}

// Announces dice si el agente anuncia sus capacidades. Si no, Has no sabe
// nada y quien pregunta tiene que sondear como antes.
func (g *GuestAgent) Announces() bool { return g != nil && g.Caps != nil }

// Lacks dice si el agente anuncia sus capacidades y cap no está entre ellas:
// entonces no hace falta ni preguntarle por la ruta.
func (g *GuestAgent) Lacks(cap string) bool {
	return g.Announces() && !slices.Contains(g.Caps, cap)
}
