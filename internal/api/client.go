package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/juan52878911/kindling/internal/transport"
)

// Client habla con el daemon. El transporte (socket local o SSH) es
// intercambiable y el resto del CLI no necesita saber cuál está en uso.
type Client struct {
	http *http.Client
	d    *transport.Dialer
}

func NewClient(endpoint string) *Client {
	d := transport.New(endpoint)
	return &Client{
		d: d,
		http: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return d.Dial(ctx)
			},
			// Cada petición abre su propia conexión: con SSH detrás, reutilizar
			// conexiones complica más de lo que ahorra.
			DisableKeepAlives: true,
		}},
	}
}

func (c *Client) Endpoint() string { return c.d.Describe() }

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://kling"+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var e Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		return fmt.Errorf("%s", e.Message)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var i Info
	return &i, c.do(ctx, http.MethodGet, "/info", nil, &i)
}

func (c *Client) List(ctx context.Context) ([]*Machine, error) {
	var l []*Machine
	return l, c.do(ctx, http.MethodGet, "/machines", nil, &l)
}

func (c *Client) Run(ctx context.Context, r RunRequest) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines", r, &m)
}

func (c *Client) Freeze(ctx context.Context, ref string) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/freeze", nil, &m)
}

func (c *Client) Thaw(ctx context.Context, ref string) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/thaw", nil, &m)
}

func (c *Client) Stop(ctx context.Context, ref string) (*Machine, error) {
	var m Machine
	return &m, c.do(ctx, http.MethodPost, "/machines/"+ref+"/stop", nil, &m)
}

func (c *Client) Remove(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/machines/"+ref, nil, nil)
}

// Logs trae la consola serie. Se devuelve texto plano, no JSON: es para leer.
func (c *Client) Logs(ctx context.Context, ref string, tail int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://kling/machines/%s/logs?tail=%d", ref, tail), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		var e Error
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return "", fmt.Errorf("%s", e.Message)
		}
		return "", fmt.Errorf("%s", resp.Status)
	}
	return string(b), nil
}

// SetLabels reetiqueta una máquina. Se usa al importar, cuando la decisión sobre
// el modo de ejecución solo puede tomarse DESPUÉS de ver su catálogo.
func (c *Client) SetLabels(ctx context.Context, ref string, labels map[string]string) error {
	return c.do(ctx, http.MethodPut, "/machines/"+ref+"/labels", labels, nil)
}

func (c *Client) Commit(ctx context.Context, ref, name string) (*Snapshot, error) {
	var s Snapshot
	return &s, c.do(ctx, http.MethodPost, "/machines/"+ref+"/commit", CommitRequest{Name: name}, &s)
}

func (c *Client) Snapshots(ctx context.Context) ([]*Snapshot, error) {
	var l []*Snapshot
	return l, c.do(ctx, http.MethodGet, "/snapshots", nil, &l)
}

func (c *Client) Links(ctx context.Context) ([]*Link, error) {
	var l []*Link
	return l, c.do(ctx, http.MethodGet, "/links", nil, &l)
}

func (c *Client) SetLink(ctx context.Context, l *Link) (*Link, error) {
	var out Link
	return &out, c.do(ctx, http.MethodPut, "/links", l, &out)
}

func (c *Client) RemoveLink(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/links/"+name, nil, nil)
}

func (c *Client) SetCatalog(ctx context.Context, name string, tools []ToolSpec) (*Snapshot, error) {
	var s Snapshot
	return &s, c.do(ctx, http.MethodPut, "/snapshots/"+name+"/catalog", CatalogRequest{Tools: tools}, &s)
}

func (c *Client) RemoveSnapshot(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/snapshots/"+name, nil, nil)
}

// Events consume el stream NDJSON del daemon hasta que se cancele el contexto.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://kling/events", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) == nil {
			fn(ev)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil && err != io.EOF {
		return err
	}
	return nil
}

// Guest habla con el servidor que corre dentro de una microVM, pasando por el
// daemon. Es la única vía que funciona igual en local y por SSH.
func (c *Client) Guest(ctx context.Context, ref string, r GuestRequest) (*GuestResponse, error) {
	var out GuestResponse
	if err := c.do(ctx, "POST", "/machines/"+ref+"/guest", r, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
