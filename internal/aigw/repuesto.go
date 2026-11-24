package aigw

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Repuestos y precalentado de dorados (C9-08, C9-10; ronda 9, Z4).

// plazoCalentarRepuesto acota la petición de calentamiento de un repuesto:
// fuera del camino de la petición, pero sin dejar una reposición colgada.
const plazoCalentarRepuesto = 2 * time.Minute

// cuerpoCalentamiento es la petición con que se toca un repuesto según el
// contrato del backend: la misma que deploy/von.Warm manda antes de congelar
// el dorado (una respuesta corta), para que la réplica toque sus páginas de
// pesos y cree sus hilos fuera del camino de la petición. Los contratos sin
// petición conocida (chispa-*, raw: su calentamiento lo da el operador al
// fabricar) se quedan en la sonda: método vacío.
func cuerpoCalentamiento(contrato string) (method, path string, body []byte) {
	switch contrato {
	case ContractOpenAI:
		return http.MethodPost, "/v1/chat/completions",
			[]byte(`{"messages":[{"role":"user","content":"Say hello in one word."}],"max_tokens":8,"temperature":0}`)
	case ContractOpenAIEmbeddings:
		return http.MethodPost, "/v1/embeddings", []byte(`{"input":"hello"}`)
	}
	return "", "", nil
}

// clienteCalentar manda las peticiones de calentamiento: sin conexiones que
// se queden abiertas hacia la réplica (las del pool del gateway son otras).
var clienteCalentar = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// calentarRepuesto es el WarmSpare del gateway: manda a addr la petición de
// calentamiento del contrato del backend de snap, con la credencial del
// bloque si la lleva, y exige un 2xx.
func (g *Gateway) calentarRepuesto(ctx context.Context, snap, addr string) error {
	b := g.backendDe(snap)
	if b == nil {
		return nil
	}
	method, path, body := cuerpoCalentamiento(b.Contract)
	if method == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, plazoCalentarRepuesto)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := ponerAuth(b, req.Header); err != nil {
		return err
	}
	resp, err := clienteCalentar.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s answered %d", method, path, resp.StatusCode)
	}
	return nil
}

// anclados son los dorados que este gateway pidió anclar (pin), para
// soltarlos cuando un Reload les quite pin o el modelo desaparezca.
type anclados struct {
	mu  sync.Mutex
	set map[string]bool
}

// calentarDorados pide al daemon (en segundo plano, sin bloquear a quien
// llama) que traiga a la caché de página el mem.file de cada dorado con
// réplicas del registro, anclando los que llevan pin y soltando los que lo
// perdieron. Un daemon sin la capacidad (api.WarmUnavailable) se ignora en
// silencio: es lo de hoy.
func (g *Gateway) calentarDorados(ctx context.Context) {
	if g.opts.Client == nil {
		return
	}
	type pedido struct {
		snap string
		pin  *bool
	}
	var pedidos []pedido
	vistos := map[string]bool{}
	g.pin.mu.Lock()
	if g.pin.set == nil {
		g.pin.set = map[string]bool{}
	}
	for _, b := range g.config().Backends() {
		if b == nil || b.Snapshot == "" || vistos[b.Snapshot] {
			continue
		}
		vistos[b.Snapshot] = true
		p := pedido{snap: b.Snapshot}
		switch {
		case b.Pin:
			v := true
			p.pin = &v
			g.pin.set[b.Snapshot] = true
		case g.pin.set[b.Snapshot]:
			v := false
			p.pin = &v
			delete(g.pin.set, b.Snapshot)
		}
		pedidos = append(pedidos, p)
	}
	for snap := range g.pin.set {
		if !vistos[snap] {
			v := false
			pedidos = append(pedidos, pedido{snap: snap, pin: &v})
			delete(g.pin.set, snap)
		}
	}
	g.pin.mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	for _, p := range pedidos {
		go func() {
			_, err := g.opts.Client.WarmSnapshot(ctx, p.snap, api.WarmSnapshotRequest{Pin: p.pin})
			switch {
			case err == nil, api.WarmUnavailable(err), api.IsNotFound(err):
			default:
				log.Printf("prewarming %s: %v", p.snap, err)
			}
		}()
	}
}

// llenarRepuestos apunta en el planificador los repuestos de cada dorado del
// registro (los que tienen spare y, para que el segador borre los que sobran,
// los que lo tenían). Vuelve enseguida: los repuestos nacen en segundo plano.
func (g *Gateway) llenarRepuestos(ctx context.Context) {
	if g.opts.Client == nil {
		return
	}
	for _, b := range g.config().Backends() {
		if b != nil && b.Snapshot != "" {
			if b.Spare > 0 || g.sched.Status(b.Snapshot).Spares > 0 {
				g.sched.FillSpares(ctx, b.Snapshot)
			}
		}
	}
}
