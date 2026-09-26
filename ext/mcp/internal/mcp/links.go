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
// único documento, mcp/links: un objeto nombre -> Link. El daemon v0.5 migró ahí
// el links.json antiguo.

const (
	linksNS  = "mcp"
	linksKey = "links"
)

var validLinkName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func loadLinks(ctx context.Context, c *api.Client) (map[string]*Link, error) {
	m := map[string]*Link{}
	err := c.GetStore(ctx, linksNS, linksKey, &m)
	if err != nil && api.IsNotFound(err) {
		// Un 404 es "todavía no hay enlaces"... salvo que el daemon no tenga
		// store, que también es un 404.
		if i, ierr := c.Info(ctx); ierr == nil && i.Has("store") {
			return map[string]*Link{}, nil
		}
		return nil, tooOld(err)
	}
	return m, err
}

// Links devuelve los servidores externos enlazados, ordenados por nombre.
func Links(ctx context.Context, c *api.Client) ([]*Link, error) {
	m, err := loadLinks(ctx, c)
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
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return fmt.Errorf("link %q not found", name)
	}
	delete(m, name)
	return c.PutStore(ctx, linksNS, linksKey, m)
}
