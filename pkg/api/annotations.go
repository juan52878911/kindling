package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
)

// ANOTACIONES Y STORE: los dos sitios donde una extensión guarda estado en el
// daemon sin que el daemon lo entienda.
//
//   - Una anotación cuelga de un snapshot y vive y muere con él (meta.json). Es
//     para datos DEL snapshot: el catálogo de herramientas que ofrece, el
//     resultado del último sondeo de salud.
//   - El store es global, por espacio de nombres: $KLING_ROOT/store/<ns>/<key>.json.
//     Es para datos que no pertenecen a ninguna máquina ni snapshot: los
//     servidores MCP externos enlazados, por ejemplo.
//
// Los dos viven en el host del daemon, no en el del CLI, porque quien los lee
// (el gateway) corre allí y el CLI suele estar en otra máquina.

// KeyPattern valida nombres de anotación, espacios de nombres y claves del
// store. Minúsculas, sin barras ni "..", para que nunca puedan salir del
// directorio en el que se guardan.
var KeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

const (
	// MaxAnnotations es el máximo de anotaciones por snapshot.
	MaxAnnotations = 32
	// MaxValueBytes es el tamaño máximo de una anotación o de un valor del store.
	MaxValueBytes = 1 << 20
)

// StoreKeys es la respuesta de GET /store/{ns}.
type StoreKeys struct {
	Keys []string `json:"keys"`
}

// Annotation decodifica la anotación key en out. Devuelve false si el snapshot
// no la tiene.
func (s *Snapshot) Annotation(key string, out any) (bool, error) {
	raw, ok := s.Annotations[key]
	if !ok || len(raw) == 0 {
		return false, nil
	}
	return true, json.Unmarshal(raw, out)
}

// IsNotFound dice si un error del daemon es un 404.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// IsUnsupported dice si el daemon no conoce la ruta pedida: es lo que devuelve
// un daemon anterior a la capacidad. Sirve para caer a la ruta antigua durante
// la transición.
func IsUnsupported(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && (se.Code == http.StatusNotFound || se.Code == http.StatusMethodNotAllowed)
}

// Snapshot trae un snapshot por nombre.
func (c *Client) Snapshot(ctx context.Context, name string) (*Snapshot, error) {
	var s Snapshot
	return &s, c.do(ctx, http.MethodGet, "/snapshots/"+url.PathEscape(name), nil, &s)
}

// SetAnnotation guarda v (serializado a JSON) como la anotación key del snapshot.
func (c *Client) SetAnnotation(ctx context.Context, name, key string, v any) (*Snapshot, error) {
	var s Snapshot
	return &s, c.do(ctx, http.MethodPut,
		"/snapshots/"+url.PathEscape(name)+"/annotations/"+url.PathEscape(key), v, &s)
}

// RemoveAnnotation borra la anotación key del snapshot. No es un error que no
// existiera.
func (c *Client) RemoveAnnotation(ctx context.Context, name, key string) error {
	return c.do(ctx, http.MethodDelete,
		"/snapshots/"+url.PathEscape(name)+"/annotations/"+url.PathEscape(key), nil, nil)
}

// StoreKeys lista las claves de un espacio de nombres del store.
func (c *Client) StoreKeys(ctx context.Context, ns string) ([]string, error) {
	var k StoreKeys
	return k.Keys, c.do(ctx, http.MethodGet, "/store/"+url.PathEscape(ns), nil, &k)
}

// GetStore decodifica en out el valor de ns/key. Si no existe devuelve un error
// para el que IsNotFound es cierto.
func (c *Client) GetStore(ctx context.Context, ns, key string, out any) error {
	return c.do(ctx, http.MethodGet, "/store/"+url.PathEscape(ns)+"/"+url.PathEscape(key), nil, out)
}

// PutStore guarda v (serializado a JSON) en ns/key, reemplazando lo que hubiera.
func (c *Client) PutStore(ctx context.Context, ns, key string, v any) error {
	return c.do(ctx, http.MethodPut, "/store/"+url.PathEscape(ns)+"/"+url.PathEscape(key), v, nil)
}

// DeleteStore borra ns/key. No es un error que no existiera.
func (c *Client) DeleteStore(ctx context.Context, ns, key string) error {
	return c.do(ctx, http.MethodDelete, "/store/"+url.PathEscape(ns)+"/"+url.PathEscape(key), nil, nil)
}

// ImageFile lee un fichero dentro de una imagen ya construida (hasta 1 MiB).
// Si no está, el error cumple IsNotFound.
func (c *Client) ImageFile(ctx context.Context, image, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://kling/images/"+url.PathEscape(image)+"/files?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := LeerCuerpo(resp.Body, 2<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var e Error
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = resp.Status
		}
		return nil, &StatusError{Code: resp.StatusCode, Message: e.Message}
	}
	return b, nil
}

// StatImageFile dice si un fichero está en una imagen, cuánto ocupa y su sha256.
func (c *Client) StatImageFile(ctx context.Context, image, path string) (*ImageFileStat, error) {
	var st ImageFileStat
	return &st, c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/files?stat=1&path="+url.QueryEscape(path), nil, &st)
}

// PutImageFile pone un fichero dentro de una imagen ya construida.
func (c *Client) PutImageFile(ctx context.Context, image string, req PutImageFileRequest) (*ImageFileResult, error) {
	var res ImageFileResult
	return &res, c.doWith(c.long, ctx, http.MethodPut, "/images/"+url.PathEscape(image)+"/files", req, &res)
}
