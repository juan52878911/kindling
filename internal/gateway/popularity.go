package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// POPULARIDAD POR SERVICIO.
//
// El prewarm tenía un sesgo caro: rellenaba el fondo de TODOS los snapshots por
// igual. En un anfitrión con la RAM justa eso es gastar memoria precalentando
// servicios que nadie llama, y que el servicio realmente popular se lleve el 507
// cuando por fin lo piden. Aquí se lleva la cuenta de qué se usa para que
// PrewarmAll priorice lo que de verdad tiene demanda.
//
// Se usa una media móvil exponencial (EMA) y no un contador a secas: da peso a
// lo reciente sin olvidar de golpe, así ni un pico puntual fija el orden para
// siempre ni una racha vieja sigue mandando cuando el uso ya cambió.
//
// Se persiste en disco porque el arranque del gateway es justo el momento más
// sensible a la memoria: sin historial, el primer PrewarmAll volvería a repartir
// a ciegas y a pelearse por la RAM hasta reaprender qué es popular. Sobrevivir al
// reinicio evita ese periodo tonto.

// popularityAlpha es el peso de la ventana actual frente al pasado en la EMA.
// 0,4 reacciona rápido a un cambio de uso sin dejar que un único pico lo domine.
const popularityAlpha = 0.4

type popularity struct {
	path string // fichero de persistencia; "" la deja solo en memoria
	mu   sync.Mutex
	ema  map[string]float64 // servicio -> EMA de llegadas por ventana
	pend map[string]int     // llegadas desde el último plegado
}

func newPopularity(path string) *popularity {
	p := &popularity{path: path, ema: map[string]float64{}, pend: map[string]int{}}
	p.load()
	return p
}

// popularityPath es dónde se guarda el historial. Junto a la configuración, no
// dentro de Library ni de un directorio de estado aparte: es el mismo criterio
// que sigue config.Path para una herramienta de terminal.
func popularityPath() string {
	if p := os.Getenv("KLING_POPULARITY"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "" // sin sitio fiable: la popularidad vive solo en memoria
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "kling", "gateway-popularity.json")
}

// observe cuenta una llegada. Barato a propósito: se llama en el camino caliente
// de cada petición, así que solo toca un contador bajo el candado y difiere el
// trabajo de verdad —plegar y persistir— a la pasada periódica de PrewarmAll.
func (p *popularity) observe(service string) {
	if p == nil || service == "" {
		return
	}
	p.mu.Lock()
	p.pend[service]++
	p.mu.Unlock()
}

// fold pliega las llegadas acumuladas en la EMA y persiste el resultado. Se
// llama una vez por pasada de prewarm: esa cadencia ES la ventana de la media
// móvil. Un servicio que no recibió nada decae hacia cero y acaba cediendo su
// sitio en el orden a quien sí se usa.
func (p *popularity) fold() {
	if p == nil {
		return
	}
	p.mu.Lock()
	seen := map[string]bool{}
	for s := range p.ema {
		seen[s] = true
	}
	for s := range p.pend {
		seen[s] = true
	}
	for s := range seen {
		p.ema[s] = popularityAlpha*float64(p.pend[s]) + (1-popularityAlpha)*p.ema[s]
		// Podar lo que ya es ruido: que no ocupe el mapa ni el fichero para siempre.
		if p.ema[s] < 0.01 && p.pend[s] == 0 {
			delete(p.ema, s)
		}
	}
	p.pend = map[string]int{}
	snapshot := make(map[string]float64, len(p.ema))
	for s, v := range p.ema {
		snapshot[s] = v
	}
	p.mu.Unlock()
	p.save(snapshot)
}

// score es la popularidad actual de un servicio: la EMA ya plegada más las
// llegadas todavía sin plegar. Cuanto mayor, antes se precalienta.
func (p *popularity) score(service string) float64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ema[service] + float64(p.pend[service])
}

func (p *popularity) load() {
	if p == nil || p.path == "" {
		return
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		return // ausente o ilegible: se empieza sin historial, no es un error
	}
	m := map[string]float64{}
	if json.Unmarshal(b, &m) == nil {
		p.ema = m
	}
}

// save escribe el historial de forma atómica: un reinicio no debe poder leer
// medio fichero. Los fallos se tragan a propósito —la popularidad es una mejora,
// no una garantía, y no vale colgar el prewarm por no poder escribir un cache—.
func (p *popularity) save(snapshot map[string]float64) {
	if p == nil || p.path == "" {
		return
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p.path)
	}
}
