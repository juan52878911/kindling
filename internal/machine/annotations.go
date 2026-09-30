package machine

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/esquema"
)

// Snapshot devuelve un snapshot por nombre, con los mismos campos calculados
// (disco, instancias vivas) que el listado.
//
// Antes esto llamaba a Snapshots() y buscaba el nombre en la lista entera: para
// enseñar UN snapshot, releía y recorría TODOS (M-08). Carga solo el suyo
// (cacheado, ver loadSnapshotCached) y cuenta sus propias instancias vivas sin
// mirar las de los demás.
func (m *Manager) Snapshot(name string) (*api.Snapshot, error) {
	s, disco, err := m.loadSnapshotCached(name)
	if err != nil {
		return nil, err
	}
	s.DiskBytes = disco
	m.anotarCredencialesPlantilla(s)
	m.marcarObsoleto(s)
	m.mu.RLock()
	for _, mc := range m.byID {
		if mc.From == name && mc.State == api.StateRunning {
			s.Instances++
		}
	}
	m.mu.RUnlock()
	return s, nil
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
//
// Escribe siempre la versión actual: un meta v0 se copia antes a
// meta.json.v0.bak, y las claves que este binario no conoce (las de un kling
// más nuevo de la misma versión) se conservan tal cual (ver meta.go). Uno de
// una versión mayor ni se lee: loadSnapshotMeta falla antes.
func (m *Manager) editMeta(name string, edit func(*api.Snapshot) (string, error)) (*api.Snapshot, error) {
	m.metaMu.Lock()
	defer m.metaMu.Unlock()

	snap, previo, v, err := m.loadSnapshotMeta(name)
	if err != nil {
		return nil, err
	}
	msg, err := edit(snap)
	if err != nil {
		return nil, err
	}

	b, err := codificarMeta(snap, previo)
	if err != nil {
		return nil, err
	}
	if v < metaSchema {
		if err := esquema.Respaldar(filepath.Join(m.snapDir(name), "meta.json"), v); err != nil {
			return nil, fmt.Errorf("snapshot %q: backing up its meta.json before migrating it: %w", name, err)
		}
	}
	if err := writeMeta(m.snapDir(name), b); err != nil {
		return nil, err
	}
	// El meta.json que loadSnapshotCached recordaba ya no es el que hay en
	// disco (M-08).
	m.invalidateSnapCache(name)
	m.priv.EnsureReadable(m.snapDir(name))

	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvAnnotated, Name: name, Message: msg})
	return snap, nil
}
