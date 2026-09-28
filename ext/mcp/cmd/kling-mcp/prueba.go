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
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
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
func pruebaAplicable(tools []mcp.ToolSpec) (herramienta, args, que string, hay bool) {
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
// El `sid` viene de fuera y NO se abre uno nuevo: un servicio con la memoria por
// defecto (256 MiB) admite UNA sola sesion, y pedir la segunda la rechaza con un
// 400 en texto plano. La sonda profunda fallaba en todos ellos por eso.
func ejercitar(post poster, sid, herramienta, args string) error {
	if sid == "" {
		return fmt.Errorf("no session to exercise %s with", herramienta)
	}
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
	if err := json.Unmarshal(mcp.MCPPayload(raw), &res); err != nil {
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

// posterCrudo habla con el invitado por una ruta y un metodo cualesquiera, no
// solo por /mcp. Lo necesita la comprobacion de DNS, que consulta /dns.
type posterCrudo func(metodo, ruta, cuerpo string) ([]byte, error)

// guestRaw arma un posterCrudo contra una maquina concreta.
func guestRaw(ctx context.Context, c *api.Client, ref string) posterCrudo {
	return func(metodo, ruta, cuerpo string) ([]byte, error) {
		resp, err := c.Guest(ctx, ref, api.GuestRequest{
			Port: api.GuestPort, Path: ruta, Method: metodo, Body: cuerpo,
			// La mayoría de estas rutas son /mcp; a /dns la cabecera no le estorba.
			Headers: map[string]string{"Accept": mcp.AcceptMCP},
		})
		if err != nil {
			return nil, err
		}
		if resp.Status >= 300 {
			return nil, fmt.Errorf("HTTP %d from %s", resp.Status, ruta)
		}
		return []byte(resp.Body), nil
	}
}

// comprobarDNS mira si el invitado puede resolver nombres.
//
// La comprobacion tiene dos mitades muy distintas, y esa distincion es el
// arreglo entero:
//
//   - la CONFIGURACION no depende de la red. Un resolv.conf que apunte a
//     127.0.0.53 —lo que deja systemd-resolved— o a la IP privada del router es
//     inservible dentro de una microVM, y eso se sabe sin mandar un solo
//     paquete. Es el fallo que de verdad hubo, y el que aqui se caza siempre.
//   - la RESOLUCION si depende de que haya salida y de que el resolver conteste.
//     Se reporta, pero se distingue: culpar al servicio de que un tercero este
//     caido seria convertir una caida ajena en una alarma tuya.
func comprobarDNS(post posterCrudo) error { return comprobarDNSDe(post, "") }

// dominioDeSonda elige qué nombre pedirle al invitado para juzgar su DNS. Con
// allowlist el resolver de la máquina contesta REFUSED a todo lo que no esté
// permitido, a propósito; preguntar por example.com marcaba enfermo a CUALQUIER
// servicio en allowlist (visto en el e2e con figma). Se pregunta por el primer
// dominio permitido, y si no hay ninguno no se juzga: ese servicio no debe poder
// resolver nada. Con internet vale el nombre por defecto del invitado.
func dominioDeSonda(egress string, permitidos []string) (host string, juzgar bool) {
	if egress != "allowlist" {
		return "", true
	}
	for _, d := range permitidos {
		d = strings.TrimSpace(strings.TrimPrefix(d, "*."))
		if d != "" {
			return d, true
		}
	}
	return "", false
}

// comprobarDNSDe es comprobarDNS preguntando por host ("" = el que elija el
// invitado, example.com).
func comprobarDNSDe(post posterCrudo, host string) error {
	ruta := "/dns"
	if host != "" {
		ruta += "?host=" + url.QueryEscape(host)
	}
	raw, err := post("GET", ruta, "")
	if err != nil {
		// Un puente antiguo no tiene /dns. No es un fallo del servicio.
		return nil
	}
	var info struct {
		Nameservers []string `json:"nameservers"`
		Resuelve    bool     `json:"resuelve"`
		Probado     string   `json:"probado"`
		Error       string   `json:"error"`
	}
	if json.Unmarshal(raw, &info) != nil {
		return nil // idem: no se entiende, no se juzga
	}

	if len(info.Nameservers) == 0 {
		return fmt.Errorf("the guest has NO nameserver in /etc/resolv.conf: it cannot resolve anything")
	}
	// Si TODOS son inalcanzables, la configuracion esta rota. Uno solo utilizable
	// basta: el resolver es una lista y se prueban en orden.
	utiles := 0
	for _, ns := range info.Nameservers {
		if !InalcanzableDesdeUnInvitado(ns) {
			utiles++
		}
	}
	if utiles == 0 {
		return fmt.Errorf("every nameserver of the guest is unreachable from inside a microVM (%s).\n"+
			"That is what the host's /etc/resolv.conf leaves behind: 127.0.0.53 does not exist in "+
			"there, and a private address is blocked by the egress firewall on purpose",
			strings.Join(info.Nameservers, ", "))
	}
	if !info.Resuelve {
		return fmt.Errorf("the nameservers look usable (%s) but resolving %q failed: %s",
			strings.Join(info.Nameservers, ", "), info.Probado, info.Error)
	}
	return nil
}

// esModeloIA dice si un snapshot es un modelo de `kling ai` (una réplica de
// llama-server o una tarea de Chispa) y no un servidor MCP. Comparten el
// catálogo de snapshots y la etiqueta "service", pero no hablan MCP: pedirles
// tools/list da un 404 y health/heal los marcaban enfermos a todos (visto en el
// e2e del lab con chispa-room y los x86-*). Las etiquetas son las que ponen
// pkg/von (von.model, von.kind) y `kling ai chispa deploy` (chispa.task); se
// repiten aquí para no atar la extensión a esos paquetes.
func esModeloIA(s *api.Snapshot) bool {
	for _, l := range []string{"von.model", "von.kind", "chispa.task"} {
		if _, ok := s.Labels[l]; ok {
			return true
		}
	}
	return false
}
