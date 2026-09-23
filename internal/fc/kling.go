package fc

// Rutas propias de kling-vz, el ayudante de macOS (docs/backend-vz.md §3).
//
// Firecracker no las conoce y el núcleo solo las llama en macOS: allí no hay
// namespace, veth ni /proc que hagan ese trabajo desde el host, así que se le
// pide al propio VMM. Van en este paquete y no en otro porque viajan por el
// mismo socket y con el mismo formato de error que el resto del API.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// maxRespuestaKling acota lo que se lee de una respuesta del ayudante. Todas
// son objetos pequeños; un ayudante roto que escupiera megas no debe poder
// hacer crecer la memoria del daemon.
const maxRespuestaKling = 1 << 20

// KlingInfo es la respuesta de GET /kling/info.
type KlingInfo struct {
	Backend string `json:"backend"`
	Version string `json:"version"`
}

// KlingNetwork es la política de salida de una máquina (PUT /kling/network).
type KlingNetwork struct {
	Egress       string   `json:"egress"`
	AllowDomains []string `json:"allow_domains,omitempty"`
}

// doOut es do() leyendo además el cuerpo de la respuesta en out (si no es nil).
func (c *Client) doOut(ctx context.Context, method, path string, body, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	lr := io.LimitReader(resp.Body, maxRespuestaKling)
	if resp.StatusCode >= 300 {
		var e struct {
			FaultMessage string `json:"fault_message"`
		}
		_ = json.NewDecoder(lr).Decode(&e)
		return fmt.Errorf("kling-vz %s %s: %s: %s", method, path, resp.Status, e.FaultMessage)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(lr).Decode(out); err != nil {
		return fmt.Errorf("kling-vz %s %s: unreadable response: %w", method, path, err)
	}
	return nil
}

// KlingInfo pregunta al ayudante qué es y qué versión tiene.
func (c *Client) KlingInfo(ctx context.Context) (*KlingInfo, error) {
	var i KlingInfo
	if err := c.doOut(ctx, http.MethodGet, "/kling/info", nil, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// SetKlingNetwork fija la política de salida. Tiene que llegar antes de
// InstanceStart o de snapshot/load: sin ella el ayudante aplica "none".
func (c *Client) SetKlingNetwork(ctx context.Context, n KlingNetwork) error {
	return c.doOut(ctx, http.MethodPut, "/kling/network", n, nil)
}

// KlingForwards pide un puerto de loopback por cada puerto del invitado y
// devuelve la traducción "puerto del invitado" -> "127.0.0.1:NNNN".
func (c *Client) KlingForwards(ctx context.Context, ports []int) (map[string]string, error) {
	var r struct {
		Forwards map[string]string `json:"forwards"`
	}
	if err := c.doOut(ctx, http.MethodPut, "/kling/forwards", map[string][]int{"ports": ports}, &r); err != nil {
		return nil, err
	}
	// Se comprueba que vengan todos: un reenvío que falta es un puerto al que
	// el daemon creería llegar y no llega, y el fallo aparecería lejos de aquí.
	for _, p := range ports {
		if r.Forwards[strconv.Itoa(p)] == "" {
			return nil, fmt.Errorf("kling-vz did not forward guest port %d", p)
		}
	}
	return r.Forwards, nil
}

// KlingFootprintMiB es la memoria que ocupa la máquina en el host: el
// equivalente en macOS del RSS de firecracker.
func (c *Client) KlingFootprintMiB(ctx context.Context) (int64, error) {
	var s struct {
		FootprintMiB int64 `json:"footprint_mib"`
	}
	if err := c.doOut(ctx, http.MethodGet, "/kling/stats", nil, &s); err != nil {
		return 0, err
	}
	return s.FootprintMiB, nil
}
