package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// Este fichero es SEMILLA de la ronda 9 (analisis/ronda9-carga/plan-r9.md):
// solo declaraciones compartidas entre zonas. Lo implementa Z1 (máquina); lo
// llaman Z3 (CLI, tras deploy/roll) y Z4 (gateway de IA, al arrancar y al
// recargar).

// LabelGoldenMirror es la etiqueta de la PLANTILLA de un dorado con la que el
// fabricante dice si el commit debe espejar el mem.file en el almacén CoW
// (C9-15). Sin etiqueta = lo de hoy (se espeja si hay almacén). Con
// GoldenMirrorOff el daemon no lo espeja: es lo que pone el fabricante para un
// backend idle "remove", cuyas réplicas nunca se congelan en diferencial. El
// primer thaw en diferencial de un dorado así paga el espejo entonces.
const (
	LabelGoldenMirror = "kling.golden.mirror"
	GoldenMirrorOff   = "off"
)

// WarmSnapshotRequest es el cuerpo de POST /snapshots/{name}/warm (C9-10).
type WarmSnapshotRequest struct {
	// Pin, si no es nil, ancla (true) o suelta (false) el mem.file del dorado
	// en RAM: el daemon mantiene un mapeo compartido de solo lectura con mlock,
	// que ancla las mismas páginas que mapean las réplicas. Es RAM no
	// recuperable: opt-in, contada en la admisión de memoria, y solo en Linux
	// (en macOS el daemon responde 501). nil = no tocar el anclado.
	Pin *bool `json:"pin,omitempty"`
	// Wait pide que la respuesta espere a que el dorado esté entero en la
	// caché de página; sin él la lectura sigue en segundo plano y la respuesta
	// vuelve enseguida con lo que hubiera ya.
	Wait bool `json:"wait,omitempty"`
}

// WarmSnapshotResult es la respuesta de POST /snapshots/{name}/warm.
type WarmSnapshotResult struct {
	// Bytes es lo asignado del mem.file (lo que hay que traer).
	Bytes int64 `json:"bytes"`
	// CachedBytes es lo que ya estaba en la caché de página al contestar.
	CachedBytes int64 `json:"cached_bytes"`
	// Started dice que quedó una lectura en segundo plano.
	Started bool `json:"started,omitempty"`
	// Pinned es el estado del anclado tras la llamada.
	Pinned bool `json:"pinned,omitempty"`
}

// CommitTimings es el desglose de un commit, en milisegundos (C9-12): lo mismo
// que el daemon ya escribe en su log ("commit X -> Y: overlay…, dump…"). Lo
// rellena Z1 en la respuesta de Commit; Z3 lo imprime en `kling ai model add`
// y `deploy`. Un campo a cero = paso que no hubo o daemon que no lo mide.
type CommitTimings struct {
	SqueezeMS int64 `json:"squeeze_ms,omitempty"`
	OverlayMS int64 `json:"overlay_ms,omitempty"`
	DumpMS    int64 `json:"dump_ms,omitempty"`
	// HolesMS y DigestMS son un solo pase desde la ronda 9 (C9-02): el pase
	// único se apunta entero en DigestMS y HolesMS queda a cero.
	HolesMS  int64 `json:"holes_ms,omitempty"`
	SyncMS   int64 `json:"sync_ms,omitempty"`
	DigestMS int64 `json:"digest_ms,omitempty"`
	TotalMS  int64 `json:"total_ms,omitempty"`
}

// WarmSnapshot pide al daemon que traiga el mem.file del dorado name a la
// caché de página y, con r.Pin, que lo ancle o lo suelte. Es idempotente y
// barato si ya está caliente. Contrato para quien llama: un error para el que
// WarmUnavailable es cierto NO es un fallo: se sigue sin precalentar, como hoy.
func (c *Client) WarmSnapshot(ctx context.Context, name string, r WarmSnapshotRequest) (*WarmSnapshotResult, error) {
	var res WarmSnapshotResult
	return &res, c.doWith(c.long, ctx, http.MethodPost, "/snapshots/"+url.PathEscape(name)+"/warm", r, &res)
}

// WarmUnavailable dice si err de WarmSnapshot significa "este daemon no sabe
// precalentar o anclar": la ruta no existe (daemon anterior) o responde 501
// (backend sin la capacidad, o la semilla). Quien llama lo ignora.
func WarmUnavailable(err error) bool {
	var se *StatusError
	return IsUnsupported(err) || (errors.As(err, &se) && se.Code == http.StatusNotImplemented)
}
