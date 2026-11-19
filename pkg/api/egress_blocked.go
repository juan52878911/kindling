package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// CapabilityEgressBlocked: GET /machines/{ref}/egress (llegó en v0.19 sin
// capacidad; un daemon de entonces la sirve sin anunciarla).
const CapabilityEgressBlocked = "egress-blocked"

// BlockedDest es un destino que la política de salida de una máquina le cortó:
// un dominio (Kind "dns") o IP:puerto (Kind "ip"), con los intentos vistos.
// Nunca lleva datos de la conexión, solo a dónde iba.
type BlockedDest struct {
	Dest  string `json:"dest"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// EgressBlocked es lo que responde GET /machines/{ref}/egress.
type EgressBlocked struct {
	Egress       string        `json:"egress"`
	Total        int           `json:"total"`
	Destinations []BlockedDest `json:"destinations,omitempty"`
}

// Warning es el aviso para el usuario, o "" si no se cortó nada. Sugiere
// `-egress allowlist -allow <dominios>` con los dominios vistos; si no hay
// ninguno (solo IP directas) lo dice, porque el allowlist es por nombre.
func (e EgressBlocked) Warning() string {
	if e.Total == 0 || len(e.Destinations) == 0 {
		return ""
	}
	var dests, doms []string
	for _, d := range e.Destinations {
		dests = append(dests, d.Dest)
		if d.Kind == "dns" {
			doms = append(doms, d.Dest)
		}
	}
	const muestra = 8
	lista := dests
	if len(lista) > muestra {
		lista = append(append([]string(nil), lista[:muestra]...), fmt.Sprintf("… (+%d)", len(dests)-muestra))
	}
	plural := "conexiones bloqueadas"
	if e.Total == 1 {
		plural = "conexión bloqueada"
	}
	pol := e.Egress
	if pol == "" {
		pol = "none"
	}
	msg := fmt.Sprintf("%d %s por egress %s: %s", e.Total, plural, pol, strings.Join(lista, " "))
	switch {
	case len(doms) > 0 && pol == "allowlist":
		msg += " — añade -allow " + strings.Join(doms, ",")
	case len(doms) > 0:
		msg += " — usa -egress allowlist -allow " + strings.Join(doms, ",")
	default:
		msg += " — son IP directas: -egress allowlist solo filtra por dominio; usa -egress internet o permite el dominio que las resuelve"
	}
	return msg
}

// EgressBlocked pide al daemon los destinos que la salida de la máquina cortó.
func (c *Client) EgressBlocked(ctx context.Context, ref string) (*EgressBlocked, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://kling/machines/"+url.PathEscape(ref)+"/egress", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(body)
		var e Error
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return nil, fmt.Errorf("%s", e.Message)
		}
		return nil, fmt.Errorf("%s", resp.Status)
	}
	var out EgressBlocked
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CapabilityEgressSet: PATCH /machines/{ref}/egress cambia el modo y los
// dominios de una máquina sin reiniciarla.
const CapabilityEgressSet = "egress-set"

// EgressRequest es el cuerpo de PATCH /machines/{ref}/egress. Egress vacío
// conserva el modo; AllowDomains vacío conserva la lista (con allowlist ya
// activo) y es un error si el modo nuevo es allowlist y no hay lista previa.
type EgressRequest struct {
	Egress       string   `json:"egress,omitempty"`
	AllowDomains []string `json:"allow_domains,omitempty"`
}

// Valores de EgressPolicy.Applied.
const (
	EgressAppliedLive      = "live"       // la red de la máquina ya está reprogramada
	EgressAppliedNextStart = "next_start" // sin red montada: vale en el próximo arranque
	EgressAppliedUnchanged = "unchanged"  // ya era esa política
)

// EgressPolicy es la política de salida resultante de un PATCH.
type EgressPolicy struct {
	Egress       string   `json:"egress"`
	AllowDomains []string `json:"allow_domains,omitempty"`
	Applied      string   `json:"applied"`
	// Narrowed: el cambio permite menos que antes. ConnectionsClosed dice si,
	// además, se cortaron las conexiones que el invitado ya tenía abiertas;
	// con Narrowed y sin ConnectionsClosed (el host no tiene `conntrack`) solo
	// las conexiones nuevas quedan bajo la política nueva.
	Narrowed          bool `json:"narrowed,omitempty"`
	ConnectionsClosed bool `json:"connections_closed,omitempty"`
}

// SetEgress cambia la política de salida de una máquina (capacidad
// "egress-set").
func (c *Client) SetEgress(ctx context.Context, ref string, r EgressRequest) (*EgressPolicy, error) {
	var out EgressPolicy
	return &out, c.do(ctx, http.MethodPatch, "/machines/"+url.PathEscape(ref)+"/egress", r, &out)
}
