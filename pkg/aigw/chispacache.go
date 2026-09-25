package aigw

import (
	"container/list"
	"sync"

	"github.com/juan52878911/kindling/pkg/chispa"
)

// chispaCache guarda los modelos Chispa cargados, con un presupuesto de memoria.
//
// Un .chispa ocupa 0,3–2 MB en disco pero se expande a denso en memoria (5 MB con
// 2^18 cubos y 10 clases): con cientos de tareas no caben todos, y la mayoría
// no se usa en todo el día. Se cargan la primera vez que alguien los pide y se
// descartan por LRU al pasar del presupuesto. Cargar cuesta milisegundos; por
// eso no hace falta más política que esta.
//
// La carga (leer y validar el fichero) se hace FUERA del candado: una tarea
// cuyo modelo tarda en leerse no puede parar las predicciones de las demás.
// Dos peticiones que piden a la vez el mismo modelo esperan a una sola carga.
type chispaCache struct {
	budget int64 // bytes; <= 0 = sin límite

	mu      sync.Mutex
	used    int64
	items   map[string]*list.Element // ruta -> elemento de lru
	lru     *list.List               // frente = más reciente
	loading map[string]*carga

	loads, evictions, failures uint64
}

type chispaItem struct {
	path  string
	model *chispa.Model
	size  int64
}

type carga struct {
	done  chan struct{}
	model *chispa.Model
	err   error
}

func newChispaCache(budget int64) *chispaCache {
	return &chispaCache{budget: budget, items: map[string]*list.Element{}, lru: list.New(), loading: map[string]*carga{}}
}

// modelBytes es lo que un modelo ocupa en memoria, a efectos del presupuesto:
// la tabla de pesos manda (int16 densos) y el resto es ruido.
func modelBytes(m *chispa.Model) int64 {
	return int64(len(m.W))*2 + int64(len(m.Labels))*64 + 4096
}

// loadFn se sustituye en los tests.
var loadFn = chispa.LoadFile

// get devuelve el modelo de path, cargándolo si hace falta.
func (c *chispaCache) get(path string) (*chispa.Model, error) {
	c.mu.Lock()
	if el, ok := c.items[path]; ok {
		c.lru.MoveToFront(el)
		m := el.Value.(*chispaItem).model
		c.mu.Unlock()
		return m, nil
	}
	if cg, ok := c.loading[path]; ok {
		c.mu.Unlock()
		<-cg.done
		return cg.model, cg.err
	}
	cg := &carga{done: make(chan struct{})}
	c.loading[path] = cg
	c.mu.Unlock()

	cg.model, cg.err = loadFn(path)

	c.mu.Lock()
	delete(c.loading, path)
	if cg.err != nil {
		c.failures++
	} else {
		c.loads++
		c.insertLocked(path, cg.model)
	}
	c.mu.Unlock()
	close(cg.done)
	return cg.model, cg.err
}

// put sustituye (o mete) el modelo de path: lo usa la recalibración tras
// escribir el .chispa nuevo, para que la siguiente petición ya use los umbrales
// nuevos sin volver a leer el disco.
func (c *chispaCache) put(path string, m *chispa.Model) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(path)
	c.insertLocked(path, m)
}

// drop olvida el modelo de path (la siguiente petición lo relee).
func (c *chispaCache) drop(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(path)
}

func (c *chispaCache) dropLocked(path string) {
	if el, ok := c.items[path]; ok {
		c.used -= el.Value.(*chispaItem).size
		c.lru.Remove(el)
		delete(c.items, path)
	}
}

func (c *chispaCache) insertLocked(path string, m *chispa.Model) {
	it := &chispaItem{path: path, model: m, size: modelBytes(m)}
	c.items[path] = c.lru.PushFront(it)
	c.used += it.size
	// Se desaloja desde el menos reciente, pero nunca el recién metido: un
	// modelo más grande que todo el presupuesto se sirve igual (quien lo
	// configuró lo quiere) y se queda solo.
	for c.budget > 0 && c.used > c.budget && c.lru.Len() > 1 {
		el := c.lru.Back()
		old := el.Value.(*chispaItem)
		c.lru.Remove(el)
		delete(c.items, old.path)
		c.used -= old.size
		c.evictions++
	}
}

type chispaStats struct {
	Loaded                     int
	Bytes                      int64
	Loads, Evictions, Failures uint64
}

func (c *chispaCache) stats() chispaStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return chispaStats{Loaded: c.lru.Len(), Bytes: c.used, Loads: c.loads, Evictions: c.evictions, Failures: c.failures}
}
