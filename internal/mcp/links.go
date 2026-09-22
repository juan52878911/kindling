package mcp

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Los servidores MCP externos enlazados viven en el store del daemon como un
// único documento, mcp/links: un objeto nombre -> Link. Es el mismo documento
// que las rutas /links de compatibilidad leen y escriben, así que un gateway y
// un CLI de versiones distintas ven lo mismo.

const (
	linksNS  = "mcp"
	linksKey = "links"
)

var validLinkName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func loadLinks(ctx context.Context, c *api.Client) (map[string]*Link, error) {
	m := map[string]*Link{}
	err := c.GetStore(ctx, linksNS, linksKey, &m)
	if err != nil && api.IsNotFound(err) && storeKnown(ctx, c) {
		return map[string]*Link{}, nil
	}
	return m, err
}

// storeKnown distingue "el store no tiene la clave" de "el daemon no tiene
// store": los dos son un 404.
func storeKnown(ctx context.Context, c *api.Client) bool {
	i, err := c.Info(ctx)
	return err == nil && i.Has("store")
}

// Links devuelve los servidores externos enlazados, ordenados por nombre.
func Links(ctx context.Context, c *api.Client) ([]*Link, error) {
	m, err := loadLinks(ctx, c)
	if api.IsUnsupported(err) {
		return c.Links(ctx) // daemon v0.4
	}
	if err != nil {
		return nil, err
	}
	out := make([]*Link, 0, len(m))
	for _, l := range m {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SetLink registra o actualiza un servidor externo. Conserva la fecha de alta
// si ya existía.
func SetLink(ctx context.Context, c *api.Client, l *Link) (*Link, error) {
	if !validLinkName.MatchString(l.Name) {
		return nil, fmt.Errorf("invalid name: %q", l.Name)
	}
	if l.URL == "" {
		return nil, fmt.Errorf("missing MCP server URL")
	}
	m, err := loadLinks(ctx, c)
	if api.IsUnsupported(err) {
		return c.SetLink(ctx, l)
	}
	if err != nil {
		return nil, err
	}
	if prev, ok := m[l.Name]; ok && l.CreatedAt.IsZero() {
		l.CreatedAt = prev.CreatedAt
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now()
	}
	m[l.Name] = l
	return l, c.PutStore(ctx, linksNS, linksKey, m)
}

// RemoveLink desregistra un servidor externo.
func RemoveLink(ctx context.Context, c *api.Client, name string) error {
	m, err := loadLinks(ctx, c)
	if api.IsUnsupported(err) {
		return c.RemoveLink(ctx, name)
	}
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return fmt.Errorf("link %q not found", name)
	}
	delete(m, name)
	return c.PutStore(ctx, linksNS, linksKey, m)
}
