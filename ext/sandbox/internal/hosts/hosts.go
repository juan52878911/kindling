// Package hosts es el inventario de daemons de kindling que hay detrás del
// frontal, y la decisión de en cuál cae cada sandbox.
//
// kindling es de un host: su daemon no sabe que existen otros y sus snapshots no
// viajan (están atados al TSC de su máquina). Todo lo que sea "varios hosts"
// vive aquí, fuera del núcleo, que es donde puede vivir sin mentir: el frontal
// mantiene N clientes, elige uno por hueco libre y reintenta en otro cuando el
// elegido dice que no cabe.
package hosts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Host es un daemon de kindling.
type Host struct {
	Nombre   string
	Endpoint string
	Cliente  *api.Client
}

// Local dice si el daemon está en esta misma máquina (socket Unix): solo
// entonces los puertos que publica en 127.0.0.1 son alcanzables desde aquí.
// Con ssh:// el 127.0.0.1 es el del otro extremo.
func (h *Host) Local() bool { return !strings.HasPrefix(h.Endpoint, "ssh://") }

// Estado es lo que se sabe de un host ahora mismo.
type Estado struct {
	Nombre       string `json:"name"`
	Endpoint     string `json:"endpoint"`
	Vivo         bool   `json:"alive"`
	Version      string `json:"version,omitempty"`
	Maquinas     int    `json:"machines,omitempty"`
	DisponibleMB int64  `json:"available_mib,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ErrSinHosts es que no hay ninguno configurado.
var ErrSinHosts = errors.New("no kindling hosts configured")

// ErrSinSitio es que ninguno pudo aceptar el trabajo.
var ErrSinSitio = errors.New("no host had room")

// Registro es el conjunto de hosts, con su último estado conocido.
type Registro struct {
	mu     sync.RWMutex
	hosts  []*Host
	estado map[string]Estado
	// cache evita preguntar la memoria de cada host en cada creación: con
	// ráfagas de sandboxes eso serían miles de llamadas por minuto y la cifra
	// apenas se mueve entre una y otra.
	visto map[string]time.Time
	// sondas son las sondas en curso por host (se cierra el canal al
	// terminar): las lecturas sin forzar que llegan mientras tanto se suman a
	// la que hay en vez de lanzar otra.
	sondas map[string]chan struct{}
}

// plazoSonda es cuánto espera una sonda a un host antes de darlo por caído.
const plazoSonda = 5 * time.Second

// TTLEstado es cuánto vale una lectura de capacidad antes de repetirla.
const TTLEstado = 3 * time.Second

// Opcion ajusta cómo Nuevo construye los clientes de los hosts.
type Opcion func(*opciones)

type opciones struct {
	transporte func(http.RoundTripper) http.RoundTripper
}

// ConTransporte envuelve el transporte HTTP de cada cliente (p. ej. con
// telemetria.Transporte para propagar el traceparent hasta los daemons).
func ConTransporte(f func(http.RoundTripper) http.RoundTripper) Opcion {
	return func(o *opciones) { o.transporte = f }
}

// Nuevo construye el registro a partir de endpoints con nombre.
func Nuevo(endpoints map[string]string, opts ...Opcion) *Registro {
	var o opciones
	for _, f := range opts {
		f(&o)
	}
	r := &Registro{estado: map[string]Estado{}, visto: map[string]time.Time{}, sondas: map[string]chan struct{}{}}
	nombres := make([]string, 0, len(endpoints))
	for n := range endpoints {
		nombres = append(nombres, n)
	}
	sort.Strings(nombres)
	for _, n := range nombres {
		cl := api.NewClient(endpoints[n])
		if o.transporte != nil {
			cl.WrapTransport(o.transporte)
		}
		r.hosts = append(r.hosts, &Host{Nombre: n, Endpoint: endpoints[n], Cliente: cl})
	}
	return r
}

// Todos devuelve los hosts en orden estable.
func (r *Registro) Todos() []*Host {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]*Host(nil), r.hosts...)
}

// Host devuelve uno por nombre.
func (r *Registro) Host(nombre string) (*Host, bool) {
	for _, h := range r.Todos() {
		if h.Nombre == nombre {
			return h, true
		}
	}
	return nil, false
}

// Estados consulta todos los hosts y devuelve lo que contestan.
func (r *Registro) Estados(ctx context.Context) []Estado {
	hosts := r.Todos()
	out := make([]Estado, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h *Host) {
			defer wg.Done()
			out[i] = r.mirar(ctx, h, true)
		}(i, h)
	}
	wg.Wait()
	return out
}

// mirar pregunta a un host por su estado, con caché corta.
//
// Con forzar, sondea ahora y espera (lo usan Estados y quien necesita saber
// si un host contesta en este momento). Sin forzar, una lectura caducada
// devuelve la anterior y refresca en segundo plano, con una sola sonda por
// host a la vez: un host que acepta y no contesta ya no mete el plazo de la
// sonda en cada creación. Solo la primera lectura de un host, de la que no
// hay nada anterior, espera a la sonda (o a que venza ctx).
func (r *Registro) mirar(ctx context.Context, h *Host, forzar bool) Estado {
	if forzar {
		return r.sondear(ctx, h)
	}
	r.mu.Lock()
	visto, hay := r.visto[h.Nombre]
	previo := r.estado[h.Nombre]
	if hay && time.Since(visto) < TTLEstado {
		r.mu.Unlock()
		return previo
	}
	fin, enCurso := r.sondas[h.Nombre]
	if !enCurso {
		fin = make(chan struct{})
		r.sondas[h.Nombre] = fin
		// La sonda no depende de la petición que la lanza: otras esperan su
		// resultado, y cortarla con ella dejaría sin lectura a las demás.
		sctx := context.WithoutCancel(ctx)
		go func() {
			defer func() {
				r.mu.Lock()
				delete(r.sondas, h.Nombre)
				r.mu.Unlock()
				close(fin)
			}()
			r.sondear(sctx, h)
		}()
	}
	r.mu.Unlock()
	if hay {
		return previo
	}
	select {
	case <-fin:
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.estado[h.Nombre]
	case <-ctx.Done():
		return Estado{Nombre: h.Nombre, Endpoint: h.Endpoint, Error: ctx.Err().Error()}
	}
}

// sondear pregunta a un host por su estado y lo guarda en la caché.
func (r *Registro) sondear(ctx context.Context, h *Host) Estado {
	ctx, cancel := context.WithTimeout(ctx, plazoSonda)
	defer cancel()
	e := Estado{Nombre: h.Nombre, Endpoint: h.Endpoint}
	info, err := h.Cliente.Info(ctx)
	if err != nil {
		e.Error = err.Error()
	} else {
		e.Vivo, e.Version, e.Maquinas = true, info.Version, info.Machines
		if ps, err := h.Cliente.ProcStats(ctx); err == nil {
			e.DisponibleMB = ps.AvailableMiB
		}
	}
	r.mu.Lock()
	r.estado[h.Nombre] = e
	r.visto[h.Nombre] = time.Now()
	r.mu.Unlock()
	return e
}

// Candidatos ordena los hosts por hueco libre, de más a menos, dejando fuera los
// que no contestan. sonValidos filtra los que no sirven para este trabajo (por
// ejemplo, los que no tienen la plantilla construida).
func (r *Registro) Candidatos(ctx context.Context, sirve func(*Host) bool) []*Host {
	type par struct {
		h     *Host
		libre int64
	}
	// En paralelo: la primera lectura de cada host espera a su sonda, y en
	// serie cada host colgado sumaría su plazo.
	var validos []*Host
	for _, h := range r.Todos() {
		if sirve == nil || sirve(h) {
			validos = append(validos, h)
		}
	}
	estados := make([]Estado, len(validos))
	var wg sync.WaitGroup
	for i, h := range validos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			estados[i] = r.mirar(ctx, h, false)
		}()
	}
	wg.Wait()
	var pares []par
	for i, h := range validos {
		if estados[i].Vivo {
			pares = append(pares, par{h, estados[i].DisponibleMB})
		}
	}
	sort.SliceStable(pares, func(i, j int) bool { return pares[i].libre > pares[j].libre })
	out := make([]*Host, len(pares))
	for i, p := range pares {
		out[i] = p.h
	}
	return out
}

// Intentar prueba trabajo en los hosts que sirvan, en orden de hueco, y se queda
// con el primero que acepte.
//
// Reintenta cuando el host dice que no cabe (507), que llegó a su tope de
// máquinas, o que el fallo es el de TSC que api.EsFalloTSC reconoce (un
// reinicio del host invalidó su dorado): las tres son negativas que OTRO host
// puede atender. Un error cualquiera no se reintenta, porque repetirlo en otro
// sitio solo multiplica el mismo fallo.
func Intentar[T any](ctx context.Context, r *Registro, sirve func(*Host) bool, trabajo func(context.Context, *Host) (T, error)) (T, *Host, error) {
	return IntentarRotado(ctx, r, sirve, 0, trabajo)
}

// IntentarRotado es Intentar empezando por el candidato número desplazo
// (módulo cuántos hay) en vez de por el de más hueco. Los trabajos de un lote
// lo usan con su índice para repartirse entre los hosts en vez de caer todos
// en el que más hueco tenía al empezar; si el elegido no cabe, se sigue por
// los demás en el mismo orden circular.
func IntentarRotado[T any](ctx context.Context, r *Registro, sirve func(*Host) bool, desplazo int, trabajo func(context.Context, *Host) (T, error)) (T, *Host, error) {
	var cero T
	cands := r.Candidatos(ctx, sirve)
	if n := len(cands); n > 1 && desplazo > 0 {
		k := desplazo % n
		cands = append(append([]*Host(nil), cands[k:]...), cands[:k]...)
	}
	if len(cands) == 0 {
		if len(r.Todos()) == 0 {
			return cero, nil, ErrSinHosts
		}
		return cero, nil, fmt.Errorf("%w: no host is reachable and ready", ErrSinSitio)
	}
	var ultimo error
	for _, h := range cands {
		out, err := trabajo(ctx, h)
		if err == nil {
			return out, h, nil
		}
		ultimo = fmt.Errorf("%s: %w", h.Nombre, err)
		// El disco casi lleno (503 desde kindling v0.8) también lo resuelve otro
		// host: es falta de sitio, como la memoria, solo que de otro recurso.
		if !api.IsInsufficientMemory(err) && !api.IsMachineLimit(err) && !api.EsFalloTSC(err) && !api.IsDiskFull(err) {
			return cero, h, ultimo
		}
	}
	return cero, nil, fmt.Errorf("%w: %v", ErrSinSitio, ultimo)
}
