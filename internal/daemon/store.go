package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/durable"
)

// store es el almacén clave-valor del daemon para las extensiones:
// $KLING_ROOT/store/<ns>/<key>.json. El daemon no interpreta lo que guarda.
//
// No vive en machine.Manager porque no tiene nada que ver con máquinas: es
// estado de extensiones que tiene que estar en el host del daemon (donde corre
// el gateway) y no en el del CLI.
type store struct {
	dir string
	mu  sync.Mutex
}

var errStoreNotFound = errors.New("not found")

func (st *store) path(ns, key string) (string, error) {
	if !api.KeyPattern.MatchString(ns) {
		return "", fmt.Errorf("invalid store namespace %q", ns)
	}
	if key != "" && !api.KeyPattern.MatchString(key) {
		return "", fmt.Errorf("invalid store key %q", key)
	}
	if key == "" {
		return filepath.Join(st.dir, ns), nil
	}
	return filepath.Join(st.dir, ns, key+".json"), nil
}

func (st *store) get(ns, key string) ([]byte, error) {
	p, err := st.path(ns, key)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errStoreNotFound
	}
	return b, err
}

func (st *store) put(ns, key string, value []byte) error {
	if len(value) > api.MaxValueBytes {
		return fmt.Errorf("value is %d bytes; the limit is %d", len(value), api.MaxValueBytes)
	}
	if !json.Valid(value) {
		return fmt.Errorf("value is not valid JSON")
	}
	p, err := st.path(ns, key)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	// Durable: aquí viven cosas como los servidores MCP externos enlazados, y
	// perderlos en un corte no da error, simplemente dejan de existir.
	return durable.Escribir(p, value, 0o600)
}

func (st *store) delete(ns, key string) error {
	p, err := st.path(ns, key)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (st *store) keys(ns string) ([]string, error) {
	p, err := st.path(ns, "")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if k, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() && api.KeyPattern.MatchString(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---- rutas

func (s *Server) handleStoreKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.keys(r.PathValue("ns"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, api.StoreKeys{Keys: keys})
}

func (s *Server) handleStoreGet(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.get(r.PathValue("ns"), r.PathValue("key"))
	if errors.Is(err, errStoreNotFound) {
		fail(w, http.StatusNotFound, fmt.Errorf("%s/%s does not exist", r.PathValue("ns"), r.PathValue("key")))
		return
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *Server) handleStorePut(w http.ResponseWriter, r *http.Request) {
	b, err := api.LeerCuerpo(r.Body, api.MaxValueBytes)
	if err != nil {
		fail(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	ns, key := r.PathValue("ns"), r.PathValue("key")
	if err := s.store.put(ns, key, b); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStored, Name: ns + "/" + key})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStoreDelete(w http.ResponseWriter, r *http.Request) {
	ns, key := r.PathValue("ns"), r.PathValue("key")
	if err := s.store.delete(ns, key); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStored, Name: ns + "/" + key, Message: "deleted"})
	w.WriteHeader(http.StatusNoContent)
}

// ---- MIGRACIÓN DE links.json (v0.4)
//
// Hasta v0.4 los servidores MCP externos enlazados vivían en
// $KLING_ROOT/links.json, gestionados por el núcleo. Ahora son de kindling-mcp,
// que los guarda en el store como el documento mcp/links (objeto nombre -> link).
// La primera vez que arranca un daemon nuevo sobre datos de v0.4 se mueven ahí,
// sin interpretarlos más allá del nombre, y el original se queda como
// links.json.migrated. Se puede quitar cuando no queden hosts de v0.4.

func migrateLinks(root string, st *store) {
	old := filepath.Join(root, "links.json")
	b, err := os.ReadFile(old)
	if err != nil {
		return
	}
	if _, err := st.get("mcp", "links"); err == nil {
		return // ya migrado
	}
	var list []json.RawMessage
	if err := json.Unmarshal(b, &list); err != nil {
		log.Printf("links.json: cannot read it (%v); leaving it where it is", err)
		return
	}
	m := map[string]json.RawMessage{}
	for _, raw := range list {
		var named struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &named) == nil && named.Name != "" {
			m[named.Name] = raw
		}
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if err := st.put("mcp", "links", out); err != nil {
		log.Printf("links.json: cannot migrate it to the store: %v", err)
		return
	}
	if err := os.Rename(old, old+".migrated"); err != nil {
		log.Printf("links.json migrated to store/mcp/links, but it could not be renamed: %v", err)
		return
	}
	log.Printf("links.json migrated to store/mcp/links (%d link(s)); original kept as links.json.migrated", len(m))
}
