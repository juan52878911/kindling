package api

// Carpetas compartidas: un directorio del host dentro de una microVM. Ver
// docs/compartir.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/juan52878911/kindling/pkg/share"
)

// ShareSpec es una carpeta compartida pedida en POST /machines (RunRequest.Shares)
// o POST /sandboxes.
type ShareSpec struct {
	// Mode: "copy" (por defecto), "ro" o "rw".
	Mode string `json:"mode,omitempty"`
	// Mount es dónde aparece dentro del invitado.
	Mount string `json:"mount"`
	// Upload es el id de una subida (POST /shares/uploads). Solo en modo copy.
	Upload string `json:"upload,omitempty"`
	// Source es el directorio EN EL HOST DEL DAEMON, en los modos ro y rw. Tiene
	// que estar bajo algún daemon.share_roots. En copy solo se guarda para
	// enseñarlo (lo que el cliente dijo que subía).
	Source string `json:"source,omitempty"`
}

// ShareAttachment es una carpeta compartida de una máquina.
type ShareAttachment struct {
	Mode   string `json:"mode"`
	Mount  string `json:"mount"`
	Source string `json:"source,omitempty"`
	// ImageBytes es el tamaño del disco (ext4) de una copia.
	ImageBytes int64 `json:"image_bytes,omitempty"`
	// Status es el estado de una carpeta viva: "attached", "detached" (sin
	// sesión ahora mismo: la máquina está congelada o reconectando) o
	// "error: ...". No se guarda: lo calcula el daemon al responder.
	Status string `json:"status,omitempty"`
}

// Live dice si la carpeta se sirve en vivo (ro o rw) y no es una copia.
func (s ShareAttachment) Live() bool { return s.Mode == share.ModeRO || s.Mode == share.ModeRW }

// ShareUpload es la respuesta de POST /shares/uploads.
type ShareUpload struct {
	ID    string `json:"id"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
	Dirs  int    `json:"dirs"`
	Links int    `json:"symlinks"`
	// ImageBytes es el tamaño del ext4 construido.
	ImageBytes int64 `json:"image_bytes"`
}

// ParseShare interpreta SRC:DST[:copy|ro|rw], la sintaxis de -share.
//
// Se lee de derecha a izquierda porque SRC es una ruta del usuario y puede
// llevar dos puntos; DST no (viaja por la línea de comandos del kernel).
func ParseShare(v string) (ShareSpec, error) {
	mode := share.ModeCopy
	rest := v
	if i := strings.LastIndex(v, ":"); i >= 0 {
		switch m := v[i+1:]; m {
		case share.ModeCopy, share.ModeRO, share.ModeRW:
			mode, rest = m, v[:i]
		}
	}
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return ShareSpec{}, fmt.Errorf("invalid share %q: use SRC:DST[:copy|ro|rw]", v)
	}
	src, dst := rest[:i], rest[i+1:]
	if err := share.ValidMount(dst); err != nil {
		return ShareSpec{}, fmt.Errorf("share %q: %w", v, err)
	}
	return ShareSpec{Mode: mode, Mount: dst, Source: src}, nil
}

// UploadShare sube un tar (el contenido de una carpeta en modo copy) y devuelve
// el id con el que pedirla en ShareSpec.Upload.
func (c *Client) UploadShare(ctx context.Context, tarball io.Reader) (*ShareUpload, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://kling/shares/uploads", tarball)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	// Por el cliente largo: el daemon construye el ext4 antes de contestar, y
	// con un repositorio grande eso pasa del minuto que se da a las cabeceras.
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, fmt.Errorf("this daemon predates shared folders (kindling v0.10); upgrade it")
	}
	if resp.StatusCode >= 300 {
		return nil, statusErrorFrom(resp)
	}
	var out ShareUpload
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
