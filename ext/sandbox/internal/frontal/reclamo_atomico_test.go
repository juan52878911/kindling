package frontal

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Con un daemon que anuncia sandbox-claim, reclamar una precalentada es UNA
// llamada (POST /sandboxes/claim): ni listado, ni etiquetas, ni lectura, ni
// renovación, y sin la pausa de esperaReclamo (hipótesis del informe: los 50 ms
// fijos y las 5-9 idas y vueltas son lo que cuesta el reclamo).
func TestReclamoAtomicoEsUnaLlamadaSinPausa(t *testing.T) {
	f := nuevoFalso(t)
	f.conReclamo = true
	f.snapshots = []string{"sbx-node"}
	conPool(f, "node", 1)
	pre := f.sembrar(&api.Machine{From: "sbx-node", Labels: map[string]string{LabelTemplate: "node"}})
	srv, s := montar(t, map[string]*falso{"a": f})
	s.esperaReclamo = 3 * time.Second // si se consulta, la creación tarda 3 s

	listados := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.sandboxesLlamadas }
	antes := listados()
	ini := time.Now()
	sb := crea(t, srv, tokenAlice, CrearPeticion{Template: "node", TTLSeconds: 300, Labels: map[string]string{"k": "v"}})
	if d := time.Since(ini); d > time.Second {
		t.Fatalf("the claim waited: %s", d)
	}
	if sb.ID != "a/"+pre.ID {
		t.Fatalf("expected the prewarmed %s, got %s", pre.ID, sb.ID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reclamoLlamadas != 1 || f.etiquetasLlamadas != 0 || f.renovaciones != 0 || f.lecturas != 0 || f.creadas != 0 {
		t.Fatalf("daemon calls: claim=%d labels=%d renew=%d get=%d created=%d",
			f.reclamoLlamadas, f.etiquetasLlamadas, f.renovaciones, f.lecturas, f.creadas)
	}
	// Sin listar para reclamar: lo que listó la creación es solo lo de la cuota.
	if f.sandboxesLlamadas-antes > 1 {
		t.Fatalf("the claim listed the host: %d listings", f.sandboxesLlamadas-antes)
	}
	req := f.reclamoReq[0]
	if req.Match[LabelTemplate] != "node" || len(req.Absent) != 2 || req.TTLSeconds != 300 {
		t.Fatalf("claim request: %+v", req)
	}
	mc := f.maquinas[pre.ID]
	if mc.Labels[LabelTenant] != "alice" || mc.Labels["k"] != "v" || mc.Labels[LabelReclamo] == "" {
		t.Fatalf("claimed machine labels: %v", mc.Labels)
	}
}

// Un daemon sin la capacidad sigue con las dos fases.
func TestReclamoSinCapacidadUsaLasDosFases(t *testing.T) {
	f := nuevoFalso(t)
	f.snapshots = []string{"sbx-node"}
	conPool(f, "node", 1)
	pre := f.sembrar(&api.Machine{From: "sbx-node", Labels: map[string]string{LabelTemplate: "node"}})
	srv, s := montar(t, map[string]*falso{"a": f})
	s.esperaReclamo = 5 * time.Millisecond
	sb := crea(t, srv, tokenAlice, CrearPeticion{Template: "node"})
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb.ID != "a/"+pre.ID || f.reclamoLlamadas != 0 || f.renovaciones != 1 {
		t.Fatalf("legacy path: id=%s claim=%d renew=%d", sb.ID, f.reclamoLlamadas, f.renovaciones)
	}
}

// 100 creaciones a la vez sobre un pool de 10: 10 reclaman, 90 van al camino
// frío, y ninguna precalentada sale dos veces.
func TestReclamoAtomicoCienCreacionesSobreDiez(t *testing.T) {
	f := nuevoFalso(t)
	f.conReclamo = true
	f.snapshots = []string{"sbx-node"}
	conPool(f, "node", 10)
	pre := map[string]bool{}
	for i := 0; i < 10; i++ {
		pre["a/"+f.sembrar(&api.Machine{From: "sbx-node", Labels: map[string]string{LabelTemplate: "node"}}).ID] = true
	}
	srv, _ := montar(t, map[string]*falso{"a": f})

	var mu sync.Mutex
	vistos := map[string]int{}
	var fallos atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.DefaultClient.Do(peticion(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Template: "node"}))
			if err != nil || resp.StatusCode != 201 {
				fallos.Add(1)
				if resp != nil {
					resp.Body.Close()
				}
				return
			}
			sb := decodifica[Sandbox](t, resp)
			mu.Lock()
			vistos[sb.ID]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if fallos.Load() != 0 {
		t.Fatalf("%d creations failed", fallos.Load())
	}
	reclamadas := 0
	for id, n := range vistos {
		if n != 1 {
			t.Fatalf("%s handed out %d times", id, n)
		}
		if pre[id] {
			reclamadas++
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if reclamadas != 10 || f.creadas != 90 {
		t.Fatalf("claimed %d (want 10), created %d (want 90)", reclamadas, f.creadas)
	}
}

// pedidosFalsos apunta los avisos de reposición que recibe el frontal.
type pedidosFalsos struct {
	mu sync.Mutex
	l  []string
}

func (p *pedidosFalsos) Pedir(host, plantilla string) {
	p.mu.Lock()
	p.l = append(p.l, host+"/"+plantilla)
	p.mu.Unlock()
}

func (p *pedidosFalsos) vistos() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.l...)
}

// El frontal avisa al fondo tras reclamar con éxito y también cuando no hay
// copia que reclamar (y la creación cae al camino frío).
func TestReclamoAtomicoAvisaAlRellenador(t *testing.T) {
	f := nuevoFalso(t)
	f.conReclamo = true
	f.snapshots = []string{"sbx-node"}
	conPool(f, "node", 1)
	f.sembrar(&api.Machine{From: "sbx-node", Labels: map[string]string{LabelTemplate: "node"}})
	srv, s := montar(t, map[string]*falso{"a": f})
	rell := &pedidosFalsos{}
	s.rellena = rell

	crea(t, srv, tokenAlice, CrearPeticion{Template: "node"}) // reclama
	if v := rell.vistos(); len(v) != 1 || v[0] != "a/node" {
		t.Fatalf("after a claim: %v", v)
	}
	crea(t, srv, tokenAlice, CrearPeticion{Template: "node"}) // pool vacío: frío
	if v := rell.vistos(); len(v) != 2 || f.creadas != 1 {
		t.Fatalf("after an empty pool: asked %v, created %d", v, f.creadas)
	}
}
