package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/juan52878911/kindling/pkg/api"
)

// paraleloCompose: cuántas peticiones independientes (publicar, dejar de
// publicar) manda a la vez una orden de compose. Cada una es un viaje al
// daemon; en serie, un proyecto de diez servicios pagaba diez.
const paraleloCompose = 8

// enParalelo llama a f(0..n-1) con como mucho k a la vez y vuelve cuando
// acaban todas.
func enParalelo(n, k int, f func(i int)) {
	sem := make(chan struct{}, max(k, 1))
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			f(i)
		}()
	}
	wg.Wait()
}

// vistaProyecto es lo que una orden de compose pregunta al daemon de un
// proyecto al empezar, de una vez y en paralelo: su grafo (nil si no existe),
// las máquinas y, si se pidió, el Info.
type vistaProyecto struct {
	// nombre es el del grafo g: el del proyecto, o el que le dio un kling
	// anterior (adoptarViejo).
	nombre string
	g      *api.Graph
	ms     []*api.Machine
	info   *api.Info
	// errLista es el error de List cuando no hay grafo: entonces no para la
	// orden (no hay nada del proyecto que mirar), pero down -v no puede
	// fiarse de una lista que no tiene.
	errLista error
}

// enParaleloHastaFallo es enParalelo con un contexto que se cancela en el
// primer error: lo que está corriendo se corta, lo que falta no empieza, y
// vuelve ese primer error.
func enParaleloHastaFallo(ctx context.Context, n, k int, f func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var primero error
	enParalelo(n, k, func(i int) {
		if ctx.Err() != nil {
			return
		}
		if err := f(ctx, i); err != nil {
			mu.Lock()
			if primero == nil {
				primero = err
				cancel()
			}
			mu.Unlock()
		}
	})
	if primero == nil {
		primero = ctx.Err()
	}
	return primero
}

// verProyecto pregunta en paralelo lo de vistaProyecto. Un 404 del grafo no
// es error (g queda nil).
func verProyecto(ctx context.Context, c *api.Client, proyecto string, conInfo bool) (*vistaProyecto, error) {
	v := &vistaProyecto{nombre: proyecto}
	var errG, errL, errI error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		v.g, errG = c.Graph(ctx, proyecto)
		var se *api.StatusError
		if errors.As(errG, &se) && se.Code == 404 {
			v.g, errG = nil, nil
		}
	}()
	go func() {
		defer wg.Done()
		v.ms, errL = c.List(ctx)
	}()
	if conInfo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v.info, errI = c.Info(ctx)
		}()
	}
	wg.Wait()
	// En el orden de antes: primero el grafo, luego la lista, luego el Info.
	if errG != nil {
		return nil, errG
	}
	if v.g != nil && errL != nil {
		return nil, errL
	}
	v.errLista = errL
	if errI != nil {
		return nil, errI
	}
	return v, nil
}

// adoptarViejo busca, si no hay grafo con el nombre del proyecto, el que le
// dio un kling anterior (viejo: cortado sin hash, ver nombreDNSViejo), y lo
// toma por suyo solo si sus máquinas llevan la etiqueta de ese proyecto (y,
// si la llevan, la de este fichero): otro proyecto puede llamarse así. Dice
// si lo adoptó; entonces v.g y v.nombre son los de ese grafo.
func (v *vistaProyecto) adoptarViejo(ctx context.Context, c *api.Client, viejo, abs string) (bool, error) {
	if v.g != nil || viejo == "" || viejo == v.nombre || v.errLista != nil {
		return false, nil
	}
	g, err := c.Graph(ctx, viejo)
	var se *api.StatusError
	if errors.As(err, &se) && se.Code == 404 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	suyo := false
	for _, m := range v.ms {
		if m.Labels[api.LabelGraph] != g.ID {
			continue
		}
		if m.Labels[labelComposeProject] != viejo {
			return false, nil
		}
		if f := m.Labels[labelComposeFile]; f != "" && abs != "" && f != abs {
			return false, nil
		}
		suyo = true
	}
	if suyo {
		v.g, v.nombre = g, viejo
	}
	return suyo, nil
}

// dueno es comprobarDueno con lo ya preguntado.
func (v *vistaProyecto) dueno(proyecto, abs string) error {
	if v.g == nil {
		return nil
	}
	return duenoDe(v.g, v.ms, proyecto, abs)
}

// sinGrafo son las máquinas de v que no eran del grafo g: lo que queda
// después de borrarlo (para quitarHuerfanas sin volver a listar).
func (v *vistaProyecto) sinGrafo(g *api.Graph) []*api.Machine {
	if g == nil {
		return v.ms
	}
	del := map[string]bool{}
	for _, n := range g.Nodes {
		if n.MachineID != "" {
			del[n.MachineID] = true
		}
	}
	var out []*api.Machine
	for _, m := range v.ms {
		if del[m.ID] || (g.ID != "" && m.Labels[api.LabelGraph] == g.ID) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// publicarCompose pide al daemon que publique los ports: de cada nodo. Un
// puerto ocupado es un error (el up se deshace); UDP en el Mac, un aviso.
// Los nodos se publican en paralelo; lo que se dice de cada uno sale en el
// orden de siempre (el de sus nombres), y el error es el del primero que
// falle en ese orden.
func publicarCompose(ctx context.Context, c *api.Client, t *traduccionCompose) error {
	nodos := clavesOrd(t.Publica)
	type salida struct {
		texto strings.Builder
		err   error
	}
	res := make([]salida, len(nodos))
	enParalelo(len(nodos), paraleloCompose, func(i int) {
		nodo, r := nodos[i], &res[i]
		pub, err := c.Publish(ctx, t.Proyect+"-"+nodo, t.Publica[nodo])
		var se *api.StatusError
		if errors.As(err, &se) && se.Code == 501 {
			// Solo UDP: los TCP del nodo, sin él.
			var tcp []api.PortPublish
			for _, p := range t.Publica[nodo] {
				if p.ProtoOr() == api.ProtoTCP {
					tcp = append(tcp, p)
				}
			}
			fmt.Fprintf(&r.texto, "warning: %s: %v\n", nodo, se.Message)
			if len(tcp) == 0 {
				return
			}
			pub, err = c.Publish(ctx, t.Proyect+"-"+nodo, tcp)
		}
		if err != nil {
			r.err = fmt.Errorf("%s: publishing its ports: %w", nodo, err)
			return
		}
		for _, p := range pub.Ports {
			fmt.Fprintf(&r.texto, "  %-12s %s\n", nodo, p.String())
		}
	})
	for i := range res {
		os.Stderr.WriteString(res[i].texto.String())
		if res[i].err != nil {
			return res[i].err
		}
	}
	return nil
}
