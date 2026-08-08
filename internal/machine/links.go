package machine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// Los enlaces son servidores MCP externos que el agregador enruta sin
// ejecutarlos. Viven en un fichero aparte de los snapshots porque no tienen
// imagen, ni memoria, ni ciclo de vida: solo una URL.

func (m *Manager) linksPath() string { return filepath.Join(m.root, "links.json") }

func (m *Manager) loadLinks() map[string]*api.Link {
	out := map[string]*api.Link{}
	b, err := os.ReadFile(m.linksPath())
	if err != nil {
		return out
	}
	var list []*api.Link
	if json.Unmarshal(b, &list) != nil {
		return out
	}
	for _, l := range list {
		out[l.Name] = l
	}
	return out
}

func (m *Manager) saveLinks(links map[string]*api.Link) error {
	list := make([]*api.Link, 0, len(links))
	for _, l := range links {
		list = append(list, l)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.linksPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.linksPath())
}

// Links devuelve los servidores externos registrados.
func (m *Manager) Links() []*api.Link {
	m.mu.RLock()
	defer m.mu.RUnlock()

	links := m.loadLinks()
	out := make([]*api.Link, 0, len(links))
	for _, l := range links {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SetLink registra o actualiza un servidor externo.
func (m *Manager) SetLink(l *api.Link) (*api.Link, error) {
	if !validName.MatchString(l.Name) {
		return nil, fmt.Errorf("nombre inválido: %q", l.Name)
	}
	if l.URL == "" {
		return nil, fmt.Errorf("falta la URL del servidor MCP")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	links := m.loadLinks()
	if prev, ok := links[l.Name]; ok && l.CreatedAt.IsZero() {
		l.CreatedAt = prev.CreatedAt
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now()
	}
	links[l.Name] = l
	if err := m.saveLinks(links); err != nil {
		return nil, err
	}

	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvCommitted, Name: l.Name,
		Message: fmt.Sprintf("servidor externo enlazado: %s (%d herramienta(s))", l.URL, len(l.Tools))})
	return l, nil
}

// RemoveLink desregistra un servidor externo.
func (m *Manager) RemoveLink(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	links := m.loadLinks()
	if _, ok := links[name]; !ok {
		return fmt.Errorf("no existe el enlace %q", name)
	}
	delete(links, name)
	return m.saveLinks(links)
}
