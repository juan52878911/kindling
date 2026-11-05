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
	g    *api.Graph
	ms   []*api.Machine
	info *api.Info
}

// verProyecto pregunta en paralelo lo de vistaProyecto. Un 404 del grafo no
// es error (g queda nil).
func verProyecto(ctx context.Context, c *api.Client, proyecto string, conInfo bool) (*vistaProyecto, error) {
	v := &vistaProyecto{}
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
	if errI != nil {
		return nil, errI
	}
	return v, nil
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
