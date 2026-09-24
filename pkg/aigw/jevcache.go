package aigw

import (
	"container/list"
	"sync"

	"github.com/juan52878911/kindling/pkg/jev"
)

// jevCache guarda los modelos JEV cargados, con un presupuesto de memoria.
//
// Un .jev ocupa 0,3–2 MB en disco pero se expande a denso en memoria (5 MB con
// 2^18 cubos y 10 clases): con cientos de tareas no caben todos, y la mayoría
// no se usa en todo el día. Se cargan la primera vez que alguien los pide y se
// descartan por LRU al pasar del presupuesto. Cargar cuesta milisegundos; por
// eso no hace falta más política que esta.
//
// La carga (leer y validar el fichero) se hace FUERA del candado: una tarea
// cuyo modelo tarda en leerse no puede parar las predicciones de las demás.
// Dos peticiones que piden a la vez el mismo modelo esperan a una sola carga.
type jevCache struct {
	budget int64 // bytes; <= 0 = sin límite

	mu      sync.Mutex
	used    int64
	items   map[string]*list.Element // ruta -> elemento de lru
	lru     *list.List               // frente = más reciente
	loading map[string]*carga

	loads, evictions, failures uint64
}

type jevItem struct {
	path  string
	model *jev.Model
	size  int64
}

type carga struct {
	done  chan struct{}
	model *jev.Model
	err   error
}

func newJEVCache(budget int64) *jevCache {
	return &jevCache{budget: budget, items: map[string]*list.Element{}, lru: list.New(), loading: map[string]*carga{}}
}

// modelBytes es lo que un modelo ocupa en memoria, a efectos del presupuesto:
// la tabla de pesos manda (int16 densos) y el resto es ruido.
func modelBytes(m *jev.Model) int64 {
	return int64(len(m.W))*2 + int64(len(m.Labels))*64 + 4096
}

// loadFn se sustituye en los tests.
var loadFn = jev.LoadFile

// get devuelve el modelo de path, cargándolo si hace falta.
func (c *jevCache) get(path string) (*jev.Model, error) {
	c.mu.Lock()
	if el, ok := c.items[path]; ok {
		c.lru.MoveToFront(el)
		m := el.Value.(*jevItem).model
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
// escribir el .jev nuevo, para que la siguiente petición ya use los umbrales
// nuevos sin volver a leer el disco.
func (c *jevCache) put(path string, m *jev.Model) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(path)
	c.insertLocked(path, m)
}

// drop olvida el modelo de path (la siguiente petición lo relee).
func (c *jevCache) drop(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(path)
}

func (c *jevCache) dropLocked(path string) {
	if el, ok := c.items[path]; ok {
		c.used -= el.Value.(*jevItem).size
		c.lru.Remove(el)
		delete(c.items, path)
	}
}

func (c *jevCache) insertLocked(path string, m *jev.Model) {
	it := &jevItem{path: path, model: m, size: modelBytes(m)}
	c.items[path] = c.lru.PushFront(it)
	c.used += it.size
	// Se desaloja desde el menos reciente, pero nunca el recién metido: un
	// modelo más grande que todo el presupuesto se sirve igual (quien lo
	// configuró lo quiere) y se queda solo.
	for c.budget > 0 && c.used > c.budget && c.lru.Len() > 1 {
		el := c.lru.Back()
		old := el.Value.(*jevItem)
		c.lru.Remove(el)
		delete(c.items, old.path)
		c.used -= old.size
		c.evictions++
	}
}

type jevStats struct {
	Loaded                     int
	Bytes                      int64
	Loads, Evictions, Failures uint64
}

func (c *jevCache) stats() jevStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return jevStats{Loaded: c.lru.Len(), Bytes: c.used, Loads: c.loads, Evictions: c.evictions, Failures: c.failures}
}
