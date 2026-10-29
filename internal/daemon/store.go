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

	"github.com/juan52878911/kindling/internal/machine"
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

// nsGrafos es el espacio del almacén donde el manager guarda los grafos
// (internal/machine/grafo.go). Se lee como cualquier otro, pero solo lo
// escribe el daemon: las aristas de un grafo son autorizaciones, y una
// extensión que las reescribiera en disco las cambiaría en el próximo
// arranque sin pasar por la validación.
const nsGrafos = "graph"

var errNsGrafos = errors.New("store namespace \"graph\" is reserved: graphs are changed through /graphs")

// errStoreMal es un error de la petición al store (nombre, clave o valor que
// no valen): 400. Lo demás es del disco del host: 500 (storeStatus).
type errStoreMal struct{ msg string }

func (e *errStoreMal) Error() string { return e.msg }

func storeMal(format string, a ...any) error { return &errStoreMal{fmt.Sprintf(format, a...)} }

// storeStatus es el código de un error del store.
func storeStatus(err error) int {
	var mal *errStoreMal
	if errors.As(err, &mal) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (st *store) path(ns, key string) (string, error) {
	if !api.KeyPattern.MatchString(ns) {
		return "", storeMal("invalid store namespace %q", ns)
	}
	if key != "" && !api.KeyPattern.MatchString(key) {
		return "", storeMal("invalid store key %q", key)
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
		return storeMal("value is %d bytes; the limit is %d", len(value), api.MaxValueBytes)
	}
	if !json.Valid(value) {
		return storeMal("value is not valid JSON")
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
		fail(w, storeStatus(err), err)
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
		fail(w, storeStatus(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *Server) handleStorePut(w http.ResponseWriter, r *http.Request) {
	b, err := api.LeerCuerpo(r.Body, api.MaxValueBytes)
	if err != nil {
		fail(w, jsonBodyStatus(err), err)
		return
	}
	ns, key := r.PathValue("ns"), r.PathValue("key")
	if ns == nsGrafos {
		fail(w, http.StatusForbidden, errNsGrafos)
		return
	}
	if err := s.store.put(ns, key, b); err != nil {
		fail(w, storeStatus(err), err)
		return
	}
	s.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStored, Name: ns + "/" + key})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStoreDelete(w http.ResponseWriter, r *http.Request) {
	ns, key := r.PathValue("ns"), r.PathValue("key")
	if ns == nsGrafos {
		fail(w, http.StatusForbidden, errNsGrafos)
		return
	}
	if err := s.store.delete(ns, key); err != nil {
		fail(w, storeStatus(err), err)
		return
	}
	s.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStored, Name: ns + "/" + key, Message: "deleted"})
	w.WriteHeader(http.StatusNoContent)
}

// ---- links.json de v0.4
//
// La comprobación es machine.LinksV04 (la misma que hace `kling upgrade` antes
// de parar nada); aquí solo se avisa de un resto que ya no cuenta.

// comprobarLinksV04 da error si en root hay un links.json de v0.4 sin migrar.
func comprobarLinksV04(root string) error {
	if err := machine.LinksV04(root); err != nil {
		return err
	}
	viejo := filepath.Join(root, "links.json")
	if _, err := os.Stat(viejo); err == nil {
		log.Printf("%s: ignored, the links are already in store/mcp/links", viejo)
	}
	return nil
}
