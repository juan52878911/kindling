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
