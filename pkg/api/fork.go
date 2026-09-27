package api

// Ramificar un sandbox vivo: `kling sandbox fork <ref> -n N`.
//
// Un fork es un snapshot temporal del sandbox (se pausa un momento, se vuelca,
// se reanuda) y N restauraciones desde él. Cada copia despierta con la memoria,
// los procesos y el disco que tenía el original en ese instante, y a partir de
// ahí diverge sola: lo que escriba una no lo ve ninguna otra, ni el original.

import (
	"context"
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
