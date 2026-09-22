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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/internal/durable"
	"github.com/juan52878911/kindling/pkg/api"
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

// ---- COMPATIBILIDAD: /links sobre el store (se retira en v0.6)
//
// Los servidores MCP externos vivían en $KLING_ROOT/links.json, gestionados
// por machine.Manager. Ahora son un documento más del store, mcp/links: un
// objeto nombre -> Link. Las rutas antiguas se mantienen encima para que un CLI
// o un gateway anteriores sigan funcionando contra este daemon.

const (
	linksNS  = "mcp"
	linksKey = "links"
)

// validLinkName es la misma regla que tenían los enlaces en v0.4 (admite
// mayúsculas), para no rechazar ahora un nombre que antes se aceptaba.
var validLinkName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func (s *Server) loadLinks() (map[string]*api.Link, error) {
	out := map[string]*api.Link{}
	b, err := s.store.get(linksNS, linksKey)
	if errors.Is(err, errStoreNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return out, json.Unmarshal(b, &out)
}

func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	links, err := s.loadLinks()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	list := make([]*api.Link, 0, len(links))
	for _, l := range links {
		list = append(list, l)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleSetLink(w http.ResponseWriter, r *http.Request) {
	var l api.Link
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if !validLinkName.MatchString(l.Name) {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid name: %q", l.Name))
		return
	}
	if l.URL == "" {
		fail(w, http.StatusBadRequest, fmt.Errorf("missing MCP server URL"))
		return
	}
	s.linksMu.Lock()
	defer s.linksMu.Unlock()
	links, err := s.loadLinks()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if prev, ok := links[l.Name]; ok && l.CreatedAt.IsZero() {
		l.CreatedAt = prev.CreatedAt
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now()
	}
	links[l.Name] = &l
	if err := s.saveLinks(links); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStored, Name: l.Name,
		Message: fmt.Sprintf("external server linked: %s (%d tool(s))", l.URL, len(l.Tools))})
	writeJSON(w, http.StatusOK, &l)
}

func (s *Server) handleRemoveLink(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.linksMu.Lock()
	defer s.linksMu.Unlock()
	links, err := s.loadLinks()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if _, ok := links[name]; !ok {
		fail(w, http.StatusBadRequest, fmt.Errorf("link %q not found", name))
		return
	}
	delete(links, name)
	if err := s.saveLinks(links); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) saveLinks(links map[string]*api.Link) error {
	b, err := json.MarshalIndent(links, "", "  ")
	if err != nil {
		return err
	}
	return s.store.put(linksNS, linksKey, b)
}

// migrateLinks mueve $KLING_ROOT/links.json (v0.4) al store la primera vez que
// arranca un daemon v0.5. Deja el original como links.json.migrated: no se
// borra nada que no se pueda recuperar a mano.
func migrateLinks(root string, st *store) {
	old := filepath.Join(root, "links.json")
	b, err := os.ReadFile(old)
	if err != nil {
		return
	}
	if _, err := st.get(linksNS, linksKey); err == nil {
		return // ya migrado; el original se quedó por alguna razón
	}
	var list []*api.Link
	if err := json.Unmarshal(b, &list); err != nil {
		log.Printf("links.json: cannot read it (%v); leaving it where it is", err)
		return
	}
	m := map[string]*api.Link{}
	for _, l := range list {
		if l != nil && l.Name != "" {
			m[l.Name] = l
		}
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if err := st.put(linksNS, linksKey, out); err != nil {
		log.Printf("links.json: cannot migrate it to the store: %v", err)
		return
	}
	if err := os.Rename(old, old+".migrated"); err != nil {
		log.Printf("links.json migrated to store/%s/%s, but it could not be renamed: %v", linksNS, linksKey, err)
		return
	}
	log.Printf("links.json migrated to store/%s/%s (%d link(s)); original kept as links.json.migrated",
		linksNS, linksKey, len(m))
}
