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

	share "github.com/juan52878911/kindling/internal/shareproto"
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
	// Transport, en ro y rw: "virtiofs" la pide por virtio-fs (kling-hv:
	// 3-5× los metadatos y ~2× la E/S, pero sin avisos de los cambios del
	// host: ver docs/compartir.md), "fuse" por FUSE aunque el daemon tenga
	// virtio-fs por defecto (KLING_HV_VIRTIOFS=1), y vacío, lo que diga el
	// daemon (FUSE si no se le dijo nada).
	Transport string `json:"transport,omitempty"`
}

// ShareAttachment es una carpeta compartida de una máquina.
type ShareAttachment struct {
	Mode   string `json:"mode"`
	Mount  string `json:"mount"`
	Source string `json:"source,omitempty"`
	// ImageBytes es el tamaño del disco (ext4) de una copia.
	ImageBytes int64 `json:"image_bytes,omitempty"`
	// Transport es por dónde llega una carpeta viva al kernel del invitado:
	// "virtiofs" por un dispositivo virtio-fs de kling-hv (docs/backend-hv.md),
	// vacío por FUSE a través del agente (lo de siempre). Guardado, es lo que
	// el VMM ofreció al arrancar; en una respuesta con Status "attached", por
	// dónde va la sesión de verdad (un agente anterior o un kernel sin
	// virtio-fs caen a FUSE).
	Transport string `json:"transport,omitempty"`
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
	// SRC:DST:ro|rw:virtiofs (o :fuse): el transporte va detrás del modo y
	// solo con un modo vivo.
	transport := ""
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		switch t := rest[i+1:]; t {
		case share.TransportVirtioFS, share.TransportFUSE:
			transport, rest = t, rest[:i]
		}
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		switch m := rest[i+1:]; m {
		case share.ModeCopy, share.ModeRO, share.ModeRW:
			mode, rest = m, rest[:i]
		}
	}
	if transport != "" && mode != share.ModeRO && mode != share.ModeRW {
		return ShareSpec{}, fmt.Errorf("invalid share %q: :%s goes after ro or rw (SRC:DST:ro|rw:%s)", v, transport, transport)
	}
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return ShareSpec{}, fmt.Errorf("invalid share %q: use SRC:DST[:copy|ro|rw[:virtiofs|fuse]]", v)
	}
	src, dst := rest[:i], rest[i+1:]
	if err := share.ValidMount(dst); err != nil {
		return ShareSpec{}, fmt.Errorf("share %q: %w", v, err)
	}
	return ShareSpec{Mode: mode, Mount: dst, Source: src, Transport: transport}, nil
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
