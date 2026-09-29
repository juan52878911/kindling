package api

// Ramificar un sandbox vivo: `kling sandbox fork <ref> -n N`.
//
// Un fork es un snapshot temporal del sandbox (se pausa un momento, se vuelca,
// se reanuda) y N restauraciones desde él. Cada copia despierta con la memoria,
// los procesos y el disco que tenía el original en ese instante, y a partir de
// ahí diverge sola: lo que escriba una no lo ve ninguna otra, ni el original.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// ForkMax acota cuántas copias se piden de una vez. No es una medida de
// capacidad —el tope de máquinas y la memoria del host deciden de verdad—:
// es que una sola petición no pueda encargar miles de restauraciones.
const ForkMax = 64

// LabelForkOf es la etiqueta que lleva cada copia con el id del sandbox del que
// salió. Solo informa: el snapshot temporal se reconoce por su marca en disco,
// no por etiquetas (ver internal/machine/fork.go).
const LabelForkOf = "kling.fork-of"

// ForkRequest pide Count copias de un sandbox vivo
// (POST /sandboxes/{ref}/fork).
type ForkRequest struct {
	// Count: por defecto 1, como mucho ForkMax.
	Count int `json:"count,omitempty"`
	// TTLSeconds y OnTTL: por defecto, los del original. El reloj de cada copia
	// empieza al crearla.
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
	OnTTL      string `json:"on_ttl,omitempty"`
	// Labels se suman a las de cada copia desde su nacimiento. Claves con
	// KeyPattern; no se admiten kind ni kling.fork-of, que pone el daemon.
	Labels map[string]string `json:"labels,omitempty"`
}

// Límites de las etiquetas de un fork.
const (
	forkMaxLabels   = 32
	forkMaxLabelVal = 256
)

// ValidateForkLabels comprueba las etiquetas que se piden para las copias de
// un fork. nil y vacío valen.
func ValidateForkLabels(labels map[string]string) error {
	if len(labels) > forkMaxLabels {
		return fmt.Errorf("too many labels (%d); the limit is %d", len(labels), forkMaxLabels)
	}
	for k, v := range labels {
		if !KeyPattern.MatchString(k) {
			return fmt.Errorf("label %q is not valid (lowercase letters, digits, '.', '_', '-')", k)
		}
		if k == LabelKind || k == LabelForkOf {
			return fmt.Errorf("label %q is reserved", k)
		}
		if len(v) > forkMaxLabelVal {
			return fmt.Errorf("label %q: value longer than %d bytes", k, forkMaxLabelVal)
		}
	}
	return nil
}

// ForkResult son las copias, ya con su agente escuchando, y el snapshot
// temporal del que salieron. Ese snapshot vive mientras quede alguna copia
// (viva o dormida) y el daemon lo borra solo cuando ya no queda ninguna.
type ForkResult struct {
	Snapshot  string     `json:"snapshot"`
	Sandboxes []*Machine `json:"sandboxes"`
}

// ForkSandbox ramifica un sandbox vivo en req.Count copias (capacidad "fork").
func (c *Client) ForkSandbox(ctx context.Context, ref string, req ForkRequest) (*ForkResult, error) {
	var out ForkResult
	return &out, c.doWith(c.long, ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/fork", req, &out)
}
