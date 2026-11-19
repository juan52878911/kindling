package frontal

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// Creación en lote: POST /v1/sandboxes con "count".
//
// Cada sandbox del lote pasa por Servidor.crear, o sea por el mismo reparto
// entre hosts (hosts.Candidatos), el mismo fondo de precalentadas y los mismos
// reintentos que uno suelto. La cuota se reservó entera antes (reservarN).

// concurrenciaLote es cuántas creaciones de un lote corren a la vez. Acotada:
// un lote de 16 no debe abrir 16 restauraciones simultáneas en un host.
const concurrenciaLote = 4

// LoteRespuesta es la respuesta de una creación con count.
type LoteRespuesta struct {
	// Sandboxes son los creados, en el orden de la petición (sin huecos: si
	// el lote es parcial, solo los que salieron).
	Sandboxes []Sandbox `json:"sandboxes"`
	// Errors lo es de los que no, solo con atomic false y lote parcial.
	Errors []LoteError `json:"errors,omitempty"`
}

// LoteError es un sandbox del lote que no se creó.
type LoteError struct {
	Index  int    `json:"index"`
	Status int    `json:"status"`
	Error  string `json:"error"`
}

type resultadoLote struct {
	h   *hosts.Host
	mc  *api.Machine
	err error
}

// respuestaLote crea n sandboxes y contesta. La cuota está reservada.
func (s *Servidor) respuestaLote(w http.ResponseWriter, r *http.Request, t *Tenant, pet CrearPeticion,
	snaps map[string]*api.Snapshot, n int, hVol *hosts.Host, clonados []string) {
	res := s.crearLote(r.Context(), t, pet, snaps, n)

	var creados []resultadoLote
	var primero error
	fallos := 0
	for _, x := range res {
		switch {
		case x.err == nil && x.mc != nil:
			creados = append(creados, x)
		case x.err != nil:
			fallos++
			if primero == nil {
				primero = x.err
			}
		}
	}
	if pet.Atomic && primero != nil {
		// Todo o nada: lo creado se borra. Con contexto propio, que el de la
		// petición puede estar ya cancelado.
		for _, x := range creados {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := x.h.Cliente.RemoveSandbox(ctx, x.mc.ID); err != nil {
				s.log.Printf("frontal: atomic batch: removing %s: %v", idDe(x.h, x.mc), err)
			}
			cancel()
		}
		creados = nil
	}
	if len(creados) == 0 {
		if hVol != nil {
			s.borrarVolumenes(hVol, clonados)
		}
		if primero == nil {
			primero = context.Canceled
		}
		s.falloCrear(w, pet, primero)
		return
	}
	out := LoteRespuesta{Sandboxes: make([]Sandbox, 0, len(creados))}
	for _, x := range creados {
		out.Sandboxes = append(out.Sandboxes, vista(x.h, x.mc))
	}
	code := http.StatusCreated
	if fallos > 0 {
		code = http.StatusMultiStatus
		for i, x := range res {
			if x.err == nil {
				continue
			}
			c, msg := codigoYMensaje(codigoCrear(x.err), x.err)
			out.Errors = append(out.Errors, LoteError{Index: i, Status: c, Error: msg})
		}
		sort.Slice(out.Errors, func(a, b int) bool { return out.Errors[a].Index < out.Errors[b].Index })
	}
	writeJSON(w, code, out)
}

// crearLote lanza las n creaciones con concurrenciaLote como máximo y
// devuelve un resultado por posición. Con atomic, tras el primer fallo no se
// empieza ninguna más (las en vuelo acaban: cancelarlas a medias dejaría
// máquinas huérfanas que nadie borra).
func (s *Servidor) crearLote(ctx context.Context, t *Tenant, pet CrearPeticion, snaps map[string]*api.Snapshot, n int) []resultadoLote {
	res := make([]resultadoLote, n)
	var parar atomic.Bool
	var wg sync.WaitGroup
	cupo := make(chan struct{}, min(n, concurrenciaLote))
	for i := range n {
		cupo <- struct{}{}
		if pet.Atomic && parar.Load() {
			<-cupo
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-cupo }()
			inicio := time.Now()
			p := pet
			p.desplazo = i
			h, mc, err := s.crear(ctx, t, p, snaps)
			if err != nil {
				s.metricas.registrar(resultadoDe(err), time.Since(inicio))
				parar.Store(true)
			} else {
				s.metricas.registrar(resultadoOK, time.Since(inicio))
			}
			res[i] = resultadoLote{h: h, mc: mc, err: err}
		}()
	}
	wg.Wait()
	return res
}
