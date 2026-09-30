package machine

// Qué agente lleva cada máquina (docs/actualizar.md, §3.3).
//
// El agente de invitado se hornea en la imagen y el host se actualiza por su
// cuenta, así que durante mucho tiempo convivirán agentes de varias versiones.
// Hasta v0.17 el host lo deducía ruta a ruta (un 404 en /resync, en /ready...);
// desde v0.18 el agente dice en su /healthz qué es y qué sabe hacer, y el host
// lo apunta en la máquina (api.Machine.Agent). Con eso "qué máquinas llevan un
// agente viejo" es una consulta, y lo que el agente no anuncia no se le pide.
//
// Un agente anterior contesta "ok" a secas: queda como GuestAgent vacío y se
// le sigue tratando exactamente como antes, sondeando.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// errAgenteNoContesta: GET /healthz no llegó a contestar.
var errAgenteNoContesta = errors.New("guest agent did not answer /healthz")

// preguntarAgente hace un GET /healthz pidiendo JSON a base.
func preguntarAgente(ctx context.Context, base string) (*api.GuestAgent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+api.GuestHealthPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := listoClient.Do(req)
	if err != nil {
		return nil, errors.Join(errAgenteNoContesta, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return interpretarAgente(resp.StatusCode, resp.Header.Get("Content-Type"), b), nil
}

// interpretarAgente traduce la respuesta de /healthz. Todo lo que no es el
// JSON de un agente nuevo —el "ok" de uno anterior, un 404 de un servicio
// propio en el 8080— es un agente que no anuncia nada: GuestAgent vacío, y
// quien pregunte seguirá sondeando por ruta.
func interpretarAgente(code int, contentType string, b []byte) *api.GuestAgent {
	if code != http.StatusOK || !strings.HasPrefix(contentType, "application/json") {
		return &api.GuestAgent{}
	}
	var h api.GuestHealth
	if err := json.Unmarshal(b, &h); err != nil || h.Status != "ok" {
		return &api.GuestAgent{}
	}
	out := &api.GuestAgent{Agent: h.Agent, Version: h.Version, Caps: h.Caps}
	if out.Caps == nil {
		// Contestó en JSON: sabe anunciar, aunque no anuncie nada.
		out.Caps = []string{}
	}
	return out
}

// agenteDe devuelve lo que se sabe del agente de la máquina id (nil si nada).
func (m *Manager) agenteDe(id string) *api.GuestAgent {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mc := m.byID[id]; mc != nil {
		return mc.Agent
	}
	return nil
}

// conocerAgente pregunta en segundo plano al agente de la máquina id qué es,
// hasta que conteste o pase vigiaAgenteMax, y lo apunta en la máquina. Si ya
// se sabe (una máquina descongelada lleva el mismo agente que al congelarse),
// no pregunta. No se persiste aquí: viaja con la máquina en el siguiente
// guardado del estado.
func (m *Manager) conocerAgente(id string) {
	if m.agenteDe(id) != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), vigiaAgenteMax)
		defer cancel()
		go func() {
			select {
			case <-m.quit:
				cancel()
			case <-ctx.Done():
			}
		}()
		for ctx.Err() == nil {
			m.mu.RLock()
			mc := m.byID[id]
			var addr string
			if mc != nil && mc.State == api.StateRunning && mc.Reachable() {
				addr = mc.Addr(api.GuestPort)
			}
			m.mu.RUnlock()
			if addr == "" {
				return
			}
			pctx, pcancel := context.WithTimeout(ctx, plazoPeticionListo)
			ag, err := preguntarAgente(pctx, "http://"+addr)
			pcancel()
			if err == nil {
				m.mu.Lock()
				if mc := m.byID[id]; mc != nil && mc.Agent == nil {
					mc.Agent = ag
				}
				m.mu.Unlock()
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(pasoListo):
			}
		}
	}()
}

// olvidarAgente borra lo que se sabía del agente de id: tras un arranque en
// frío la imagen pudo cambiar.
func (m *Manager) olvidarAgente(id string) {
	m.mu.Lock()
	if mc := m.byID[id]; mc != nil {
		mc.Agent = nil
	}
	m.mu.Unlock()
}
