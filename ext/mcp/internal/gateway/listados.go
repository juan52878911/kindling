package gateway

import (
	"context"
	"errors"
	"sync"

	"github.com/juan52878911/kindling/pkg/api"
)

// UN LISTADO DE SNAPSHOTS POR PETICIÓN.
//
// El primer initialize de /mcp/_all pedía GET /snapshots una vez para saber
// qué servicios hay y otra por CADA servicio para leer su catálogo, más las de
// aislamiento y estado: con 50 servicios, 51 veces el mismo JSON (que lleva
// dentro los catálogos de todos) decodificado entero. Dentro de una misma
// petición el resultado no cambia, así que se pide una vez y se reparte.
//
// El recuerdo vive en el contexto de la petición y muere con ella: no hay
// caché entre peticiones que pueda esconder un `kling mcp import` recién hecho.

type claveListados struct{}

var errListadoSinAcabar = errors.New("snapshot listing did not finish")

type memoListados struct {
	mu  sync.Mutex
	por map[*api.Client]*listado // un daemon por host en el router
}

type listado struct {
	hecho chan struct{}
	snaps []*api.Snapshot
	err   error
}

// conMemoListados devuelve un contexto en el que listarSnapshots pide cada
// listado una sola vez. Si ya lo lleva, lo devuelve tal cual.
func conMemoListados(ctx context.Context) context.Context {
	if _, ok := ctx.Value(claveListados{}).(*memoListados); ok {
		return ctx
	}
	return context.WithValue(ctx, claveListados{}, &memoListados{por: map[*api.Client]*listado{}})
}

// listarSnapshots es c.Snapshots(ctx), compartido por toda la petición si el
// contexto viene de conMemoListados. Quien lo pide a la vez que otro espera al
// primero en vez de repetir la llamada. Los snapshots devueltos se comparten:
// son de solo lectura.
func listarSnapshots(ctx context.Context, c *api.Client) ([]*api.Snapshot, error) {
	m, _ := ctx.Value(claveListados{}).(*memoListados)
	if m == nil {
		return c.Snapshots(ctx)
	}
	m.mu.Lock()
	l, ya := m.por[c]
	if !ya {
		l = &listado{hecho: make(chan struct{})}
		m.por[c] = l
	}
	m.mu.Unlock()
	if !ya {
		// El close va en defer: si Snapshots entrara en pánico, los que
		// esperan no deben quedarse colgados para siempre.
		// Y el error de partida dice que no acabó, para que no lean una lista
		// vacía como «no hay servicios».
		defer close(l.hecho)
		l.err = errListadoSinAcabar
		l.snaps, l.err = c.Snapshots(ctx)
		return l.snaps, l.err
	}
	select {
	case <-l.hecho:
		return l.snaps, l.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
