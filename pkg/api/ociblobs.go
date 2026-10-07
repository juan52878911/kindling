package api

// Blobs OCI que sube `kling image import -archive` (GET/PUT /oci/blobs/{digest}):
// ver internal/daemon/ociblobs.go.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
)

// CapabilityOCIBlobs es la capacidad de GET /info que dice que el daemon
// recibe blobs OCI (kling image import -archive).
const CapabilityOCIBlobs = "oci-blobs"

// OCIBlob es la respuesta de GET y PUT /oci/blobs/{digest}.
type OCIBlob struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	// Unchanged: ya estaba y no se leyó nada.
	Unchanged bool `json:"unchanged,omitempty"`
}

func ociBlobURL(digest string) string {
	return "http://kling/oci/blobs/" + url.PathEscape(digest)
}

// StatOCIBlob dice si el daemon ya tiene el blob digest en su caché, y su
// tamaño. Sin él, (nil, nil).
func (c *Client) StatOCIBlob(ctx context.Context, digest string) (*OCIBlob, error) {
	var b OCIBlob
	err := c.do(ctx, http.MethodGet, "/oci/blobs/"+url.PathEscape(digest), nil, &b)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// PutOCIBlob sube un blob en flujo: size bytes de r. El daemon lo comprueba
// con su digest antes de guardarlo.
func (c *Client) PutOCIBlob(ctx context.Context, digest string, r io.Reader, size int64) (*OCIBlob, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, ociBlobURL(digest), io.LimitReader(r, size))
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody // si no, iría troceado, sin Content-Length
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, statusErrorFrom(resp)
	}
	b, err := LeerCuerpo(resp.Body, 1<<16)
	if err != nil {
		return nil, err
	}
	var out OCIBlob
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
