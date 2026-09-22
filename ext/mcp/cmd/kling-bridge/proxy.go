package main

// Modo PROXY para servidores MCP que hablan Streamable HTTP/SSE NATIVO.
//
// EL PROBLEMA. El puente nació para servidores de stdio: lanza un hijo por
// sesión y traduce el JSON-RPC que entra por tuberías a HTTP. Pero cada vez más
// servidores MCP hablan "streamable HTTP"/SSE de fábrica: NO leen stdin,
// escuchan en un puerto HTTP. Metidos por el camino de stdio no arrancan
// siquiera —o hay que degradarlos a stdio, que muchos ya no ofrecen—.
//
// LA SOLUCIÓN. Si la imagen marca transport:http en /etc/kling/service.json, el
// puente entra en modo PROXY: arranca el servidor UNA sola vez (no uno por
// sesión), espera a que su puerto responda y REVERSA hacia él las peticiones
// del gateway con httputil.ReverseProxy de la stdlib. El gateway no se entera:
// sigue hablando al mismo GuestPort (:8080) del puente.
//
//	gateway ──HTTP──> kling-bridge ──HTTP(127.0.0.1:port)──> servidor MCP
//
// PROPIEDAD QUE SE PIERDE — LÉELO. En modo stdio el puente da AISLAMIENTO
// PROCESO POR SESIÓN: cada Mcp-Session-Id tiene su propio proceso hijo, y el
// estado de una conversación no puede filtrarse a otra ni siquiera si el
// servidor MCP tuviera un fallo que lo intentara. En modo proxy el hijo es
// ÚNICO y COMPARTIDO por TODAS las sesiones: si el servidor mezcla estado entre
// ellas, el puente ya no lo impide. Es el precio inevitable de que un servidor
// HTTP —que se asume a sí mismo como un único proceso multi-sesión— funcione
// tal cual, sin reescribirlo. El ruteo/parseo por Mcp-Session-Id se mantiene del
// lado del puente SOLO para que el reaper de ociosas siga teniendo con qué
// trabajar (ver reapIdleProxy), no para aislar procesos: aquí no los hay que
// aislar.

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"time"
)

const serviceSpecPath = "/etc/kling/service.json"

// serviceSpec es el marcador que hornea la imagen para declarar CÓMO habla el
// servidor MCP hijo. Lo escribe scripts/80-mcp-image.sh en el modo http.
type serviceSpec struct {
	// Transport: "http" activa el modo proxy. Vacío o "stdio" -> modo stdio (el
	// mayoritario), y este marcador ni se escribe.
	Transport string `json:"transport"`
	// Port es donde el hijo HTTP escucha DENTRO del invitado. Distinto del
	// puerto del puente (:8080, el que ve el gateway): el puente reversa de uno
	// al otro.
	Port int `json:"port"`
}

// loadServiceSpec lee /etc/kling/service.json. Devuelve nil salvo que declare
// explícitamente transport:http con un puerto válido: el caso mayoritario es
// stdio y no debe pagar por una lectura fallida más de la cuenta.
func loadServiceSpec() *serviceSpec {
	b, err := os.ReadFile(serviceSpecPath)
	if err != nil {
		return nil
	}
	return parseServiceSpec(b)
}

// parseServiceSpec separa el parseo de la lectura para poder probarlo sin tocar
// el disco. Un marcador que no diga transport:http con puerto no cuenta: el
// modo por defecto es stdio.
func parseServiceSpec(b []byte) *serviceSpec {
	var s serviceSpec
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	if s.Transport != "http" || s.Port <= 0 {
		return nil
	}
	return &s
}

// startProxy arranca el servidor MCP HTTP y prepara el reverse proxy hacia él.
// Se llama UNA vez, al bootear, cuando service.json declara transport:http.
func (b *bridge) startProxy() error {
	if err := b.spawnProxyChild(); err != nil {
		return err
	}
	target := &url.URL{Scheme: "http", Host: b.childAddr()}
	b.proxy = b.newReverseProxy(target)
	log.Printf("http mode: reverse-proxy :8080 -> %s ready", b.childAddr())
	return nil
}

// childAddr es host:puerto del servidor MCP HTTP dentro del invitado.
func (b *bridge) childAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", b.service.Port)
}

// newReverseProxy construye el proxy hacia el hijo. Aparte de NewSingleHost...
// por dos ajustes que el modo MCP necesita:
//   - FlushInterval = -1: los servidores MCP responden por SSE, y un proxy que
//     bufferiza dejaría al cliente esperando eventos que ya han llegado.
//   - ModifyResponse: en un initialize el cliente aún no manda Mcp-Session-Id;
//     lo asigna el SERVIDOR en la respuesta. Se rastrea desde ahí para que el
//     reaper de ociosas conozca la sesión desde su primer mensaje.
func (b *bridge) newReverseProxy(target *url.URL) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	rp.ModifyResponse = func(resp *http.Response) error {
		if sid := resp.Header.Get(SessionHeader); sid != "" {
			b.touchProxySession(sid)
		}
		return nil
	}
	return rp
}

// spawnProxyChild lanza el servidor MCP HTTP y espera a que su puerto acepte
// conexiones. Si no llega a estar listo, lo mata y devuelve error: mejor fallar
// aquí, donde se ve en la consola serie, que como un 502 más tarde.
func (b *bridge) spawnProxyChild() error {
	cmd := exec.Command(b.argv[0], b.argv[1:]...)
	cmd.Env = b.env
	// La salida del servidor va a la consola serie de la microVM, igual que en
	// stdio y que el navegador compartido.
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	enSuPropioGrupo(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the HTTP MCP server: %w", err)
	}
	// No hacemos Wait: el servidor es de por vida. Si muere, el cosechador
	// (procReaper, wait4 de PID 1) lo recoge como huérfano; en /reset lo matamos
	// nosotros y arranca otro.
	b.pmu.Lock()
	b.proxyChild = cmd
	b.pmu.Unlock()
	log.Printf("http mode: MCP server started (pid %d), waiting on %s", cmd.Process.Pid, b.childAddr())

	if err := waitForPort(b.childAddr(), 40*time.Second); err != nil {
		matarGrupo(cmd)
		return err
	}
	return nil
}

// restartProxyChild mata el servidor compartido y arranca otro limpio. Lo usa
// /reset (ver handleReset): el snapshot dorado no debe congelarse con el
// servidor en estado post-handshake, o al restaurar rechazaría los initialize
// que vengan después. Reiniciarlo lo deja como recién booteado.
func (b *bridge) restartProxyChild() error {
	b.pmu.Lock()
	old := b.proxyChild
	b.proxyChild = nil
	b.pmu.Unlock()
	if old != nil && old.Process != nil {
		matarGrupo(old)
	}
	return b.spawnProxyChild()
}

// stopProxyChild mata el servidor compartido, si corre. Lo llama el apagado del
// puente: somos PID 1 y no debemos dejar el proceso suelto al morir la microVM.
func (b *bridge) stopProxyChild() {
	b.pmu.Lock()
	c := b.proxyChild
	b.proxyChild = nil
	b.pmu.Unlock()
	if c != nil && c.Process != nil {
		matarGrupo(c)
	}
}

// waitForPort espera a que algo acepte conexiones TCP en addr, o falla al
// vencer el plazo. Es la señal de "el servidor ya está listo": más fiable que
// dormir un tiempo fijo, que o se queda corto en un arranque lento o alarga el
// boot sin necesidad.
func waitForPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("the HTTP MCP server did not open %s within %s", addr, timeout)
}

// ── ruteo y reaper del modo proxy ───────────────────────────────────────────

// handleProxy reversa una petición del gateway hacia el servidor MCP HTTP
// compartido, rastreando de paso el Mcp-Session-Id para el reaper de ociosas.
func (b *bridge) handleProxy(w http.ResponseWriter, r *http.Request) {
	// Rastreo de sesión: aquí no hay proceso por sesión que gestionar, pero sí
	// queremos que una conversación abandonada acabe cerrándose en el servidor.
	// Se anota la actividad de cada id que pasa; el reaper mira estas marcas.
	if sid := r.Header.Get(SessionHeader); sid != "" {
		b.touchProxySession(sid)
		// DELETE cierra la sesión en el protocolo MCP: en cuanto se reversa al
		// servidor, el puente también la olvida.
		if r.Method == http.MethodDelete {
			defer b.forgetProxySession(sid)
		}
	}
	b.proxy.ServeHTTP(w, r)
}

func (b *bridge) touchProxySession(sid string) {
	b.mu.Lock()
	if b.proxySeen == nil {
		b.proxySeen = map[string]time.Time{}
	}
	b.proxySeen[sid] = time.Now()
	b.mu.Unlock()
}

func (b *bridge) forgetProxySession(sid string) {
	b.mu.Lock()
	delete(b.proxySeen, sid)
	b.mu.Unlock()
}

// reapIdleProxy es el reaper de ociosas del modo proxy. No mata procesos —el
// hijo es único y compartido, matarlo tumbaría a todas las sesiones vivas—,
// pero SÍ cierra en el servidor las sesiones que llevan tiempo sin actividad,
// enviándoles un DELETE como haría un cliente que se despide. Sin esto, el
// estado por sesión del servidor compartido crecería sin límite con cada
// conversación que un cliente abandona sin cerrar.
func (b *bridge) reapIdleProxy() {
	now := time.Now()
	var dead []string
	b.mu.Lock()
	for sid, last := range b.proxySeen {
		if now.Sub(last) > b.idle {
			dead = append(dead, sid)
			delete(b.proxySeen, sid)
		}
	}
	b.mu.Unlock()
	for _, sid := range dead {
		b.deleteProxySession(sid)
	}
}

// deleteProxySession pide al servidor compartido que cierre una sesión. Best
// effort: si falla, el puente ya la olvidó y no hay más que hacer desde aquí; el
// objetivo es soltar el estado, no garantizar que el servidor obedezca.
func (b *bridge) deleteProxySession(sid string) {
	req, err := http.NewRequest(http.MethodDelete, "http://"+b.childAddr()+"/mcp", nil)
	if err != nil {
		return
	}
	req.Header.Set(SessionHeader, sid)
	client := &http.Client{Timeout: 5 * time.Second}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
	log.Printf("http mode: session %.8s idle, DELETE to the shared server", sid)
}
