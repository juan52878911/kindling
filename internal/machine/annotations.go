package machine

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Snapshot devuelve un snapshot por nombre, con los mismos campos calculados
// (disco, instancias vivas) que el listado.
func (m *Manager) Snapshot(name string) (*api.Snapshot, error) {
	if _, err := m.loadSnapshot(name); err != nil {
		return nil, err
	}
	for _, s := range m.Snapshots() {
		if s.Name == name {
			return s, nil
		}
	}
	return nil, fmt.Errorf("snapshot %q does not exist", name)
}

// SetAnnotation guarda value como la anotación key del snapshot name.
//
// El daemon no interpreta el contenido: solo comprueba que es JSON, que la
// clave es válida y que no se pasa de los topes. Escribe meta.json de forma
// durable, igual que el commit, porque perder una anotación en un corte no da
// error: la extensión simplemente deja de ver lo que había guardado.
func (m *Manager) SetAnnotation(name, key string, value json.RawMessage) (*api.Snapshot, error) {
	if !api.KeyPattern.MatchString(key) {
		return nil, fmt.Errorf("invalid annotation key %q: lowercase letters, digits, '.', '_' or '-', up to 64", key)
	}
	if len(value) > api.MaxValueBytes {
		return nil, fmt.Errorf("annotation %q is %d bytes; the limit is %d", key, len(value), api.MaxValueBytes)
	}
	if !json.Valid(value) {
		return nil, fmt.Errorf("annotation %q is not valid JSON", key)
	}
	return m.editMeta(name, func(s *api.Snapshot) (string, error) {
		if _, exists := s.Annotations[key]; !exists && len(s.Annotations) >= api.MaxAnnotations {
			return "", fmt.Errorf("snapshot %q already has %d annotations, the maximum", name, api.MaxAnnotations)
		}
		if s.Annotations == nil {
			s.Annotations = map[string]json.RawMessage{}
		}
		s.Annotations[key] = append(json.RawMessage(nil), value...)
		return "annotated: " + key, nil
	})
}

// RemoveAnnotation borra la anotación key. Que no existiera no es un error: el
// resultado que pide quien llama —que no esté— ya se cumple.
func (m *Manager) RemoveAnnotation(name, key string) (*api.Snapshot, error) {
	if !api.KeyPattern.MatchString(key) {
		return nil, fmt.Errorf("invalid annotation key %q", key)
	}
	return m.editMeta(name, func(s *api.Snapshot) (string, error) {
		delete(s.Annotations, key)
		if len(s.Annotations) == 0 {
			s.Annotations = nil
		}
		return "annotation removed: " + key, nil
	})
}

// editMeta es el leer-modificar-escribir de meta.json de un snapshot existente,
// serializado por metaMu. edit devuelve el mensaje del evento.
func (m *Manager) editMeta(name string, edit func(*api.Snapshot) (string, error)) (*api.Snapshot, error) {
	m.metaMu.Lock()
	defer m.metaMu.Unlock()

	snap, err := m.loadSnapshot(name)
	if err != nil {
		return nil, err
	}
	msg, err := edit(snap)
	if err != nil {
		return nil, err
	}
	mirrorLegacyMCP(snap)

	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeMeta(m.snapDir(name), b); err != nil {
		return nil, err
	}
	m.priv.EnsureReadable(m.snapDir(name))

	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvAnnotated, Name: name, Message: msg})
	return snap, nil
}
