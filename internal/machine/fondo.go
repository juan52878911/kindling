package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/durable"
)

// FONDO DE COPIAS PRECALENTADAS DE UNA PLANTILLA.
//
// `kling run -from T` paga el ciclo entero en el camino crítico: copiar el
// overlay, montar la red, cargar la memoria, reapuntar discos, resincronizar
// el invitado y lanzar los ganchos de identidad (~100 ms con node22, ~30 con
// una mínima). Con un fondo de N copias ya restauradas, Run solo reclama una:
// la renombra, le pone las etiquetas y el TTL de la petición y la devuelve.
//
// Cada copia del fondo nace por el camino de siempre (runFrom), y espera a
// estar lista (ganchos incluidos) ANTES de ofrecerse. Por eso una copia
// reclamada ya pasó los mismos ganchos de renovación que una nueva —machine-id,
// claves SSH, semillas— y no comparte identidad con ninguna otra: lo único que
// cambia al reclamarla es su nombre y su contabilidad.
//
// Apagado por defecto: sin `kling template pool` no hay fondo ni coste.
// Las copias del fondo son máquinas normales (se ven en ps con la etiqueta
// kling.pool, cuentan para el tope de máquinas, la memoria y las cuotas del
// daemon), y borrar la plantilla las retira.

// ctxFondo marca el contexto de las altas del propio fondo, que no deben
// reclamar del fondo lo que están creando para él.
type ctxFondo struct{}

// fondoPlantilla es el estado del fondo de una plantilla. Lo protege fondoMu.
type fondoPlantilla struct {
	size   int
	listas []string        // copias reclamables, de la más antigua a la más nueva
	todas  map[string]bool // las del fondo, listas o no; las reclamadas salen de aquí
	// llenando es true mientras hay una goroutine reponiendo; hecho se cierra
	// cuando acaba, para que vaciar pueda esperarla.
	llenando bool
	hecho    chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	// obsoleto es la causa (causaObsoleto) si el dorado es de otro VMM y
	// fallo el último error al reponer: los dos salen en GET /info para que
	// `kling status` diga qué rehacer, en vez de quedarse en el log.
	obsoleto, fallo string
}

func (m *Manager) fondosPath() string { return filepath.Join(m.root, "fondos.json") }

// fondoDe devuelve (creándolo) el estado del fondo de name. Con fondoMu.
func (m *Manager) fondoDe(name string) *fondoPlantilla {
	if m.fondos == nil {
		m.fondos = map[string]*fondoPlantilla{}
	}
	f := m.fondos[name]
	if f == nil {
		ctx, cancel := context.WithCancel(context.Background())
		f = &fondoPlantilla{todas: map[string]bool{}, ctx: ctx, cancel: cancel}
		m.fondos[name] = f
	}
	return f
}

// guardarFondos escribe los tamaños pedidos. Con fondoMu.
func (m *Manager) guardarFondos() error {
	tam := map[string]int{}
	for n, f := range m.fondos {
		if f.size > 0 {
			tam[n] = f.size
		}
	}
	b, err := json.MarshalIndent(tam, "", "  ")
	if err != nil {
		return err
	}
	return durable.Escribir(m.fondosPath(), b, 0o644)
}

// SetFondo fija cuántas copias despiertas mantiene la plantilla name. 0 apaga
// el fondo y lo vacía. La reposición va en segundo plano: vuelve enseguida.
func (m *Manager) SetFondo(name string, size int) (*api.PoolInfo, error) {
	if size < 0 || size > api.PoolMax {
		return nil, fmt.Errorf("pool size must be between 0 and %d (got %d)", api.PoolMax, size)
	}
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("invalid snapshot name: %q", name)
	}
	if _, err := m.loadSnapshot(name); err != nil {
		return nil, err
	}
	if size == 0 {
		m.vaciarFondo(name, true)
		return &api.PoolInfo{Template: name}, nil
	}
	m.fondoMu.Lock()
	f := m.fondoDe(name)
	f.size = size
	// Si baja, se retiran las sobrantes (las más nuevas, que son las que
	// menos han esperado).
	var sobran []string
	for len(f.listas) > size {
		id := f.listas[len(f.listas)-1]
		f.listas = f.listas[:len(f.listas)-1]
		delete(f.todas, id)
		sobran = append(sobran, id)
	}
	err := m.guardarFondos()
	m.fondoMu.Unlock()
	for _, id := range sobran {
		go m.retirarDelFondo(id)
	}
	if err != nil {
		return nil, fmt.Errorf("saving the pool size: %w", err)
	}
	m.rellenarFondo(name)
	return m.InfoFondo(name), nil
}

// InfoFondo es el estado del fondo de name (todo a cero si no tiene).
func (m *Manager) InfoFondo(name string) *api.PoolInfo {
	m.fondoMu.Lock()
	defer m.fondoMu.Unlock()
	out := &api.PoolInfo{Template: name}
	if f := m.fondos[name]; f != nil {
		out.Size, out.Ready = f.size, len(f.listas)
		out.Stale, out.Error = f.obsoleto, f.fallo
		if f.obsoleto != "" {
			out.Fix = remedioDorado(name)
		}
	}
	return out
}

// ProblemasFondos son los fondos con tamaño que no se reponen (dorado de
// otro VMM, o el último alta falló), por nombre.
func (m *Manager) ProblemasFondos() []api.PoolInfo {
	m.fondoMu.Lock()
	var nombres []string
	for n, f := range m.fondos {
		if f.size > 0 && (f.obsoleto != "" || f.fallo != "") {
			nombres = append(nombres, n)
		}
	}
	m.fondoMu.Unlock()
	sort.Strings(nombres)
	out := make([]api.PoolInfo, 0, len(nombres))
	for _, n := range nombres {
		out = append(out, *m.InfoFondo(n))
	}
	return out
}

// retirarDelFondo borra una copia que ya no es del fondo ni de nadie.
func (m *Manager) retirarDelFondo(id string) {
	if err := m.Remove(id); err != nil && !errors.Is(err, ErrNoMachine) {
		log.Printf("pool: couldn't remove %s: %v", shortID(id), err)
	}
}

// vaciarFondo para la reposición de name, espera a que acabe y retira todas
// las copias del fondo. Con olvidar, además borra su tamaño del disco (apagarlo
// o borrar la plantilla); sin él, el tamaño se conserva.
func (m *Manager) vaciarFondo(name string, olvidar bool) {
	m.fondoMu.Lock()
	f := m.fondos[name]
	if f == nil {
		m.fondoMu.Unlock()
		return
	}
	f.cancel()
	hecho := f.hecho
	m.fondoMu.Unlock()
	// Sin el candado: el relleno lo toma para registrar la copia que acaba.
	if hecho != nil {
		<-hecho
	}
	m.fondoMu.Lock()
	ids := make([]string, 0, len(f.todas))
	for id := range f.todas {
		ids = append(ids, id)
	}
	delete(m.fondos, name)
	var err error
	if olvidar {
		err = m.guardarFondos()
	}
	m.fondoMu.Unlock()
	if err != nil {
		log.Printf("pool %s: saving sizes: %v", name, err)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.retirarDelFondo(id)
		}()
	}
	wg.Wait()
}

// rellenarFondo lanza, si no hay ya una, la goroutine que repone el fondo de
// name hasta su tamaño. Nunca bloquea a quien llama.
func (m *Manager) rellenarFondo(name string) {
	m.fondoMu.Lock()
	f := m.fondos[name]
	if f == nil || f.llenando || f.ctx.Err() != nil || len(f.todas) >= f.size {
		m.fondoMu.Unlock()
		return
	}
	f.llenando = true
	hecho := make(chan struct{})
	f.hecho = hecho
	m.fondoMu.Unlock()
	go func() {
		defer close(hecho)
		m.reponerFondo(name, f)
	}()
}

// reponerFondo crea copias una a una hasta llegar al tamaño. La decisión de
// parar y el desmarcado de llenando van bajo el mismo candado que mira cuánto
// falta: un reclamo que llegue justo ahora ve llenando=false y lanza otra
// reposición, o la ve este bucle en su siguiente vuelta; no se pierde ninguno.
func (m *Manager) reponerFondo(name string, f *fondoPlantilla) {
	for {
		m.fondoMu.Lock()
		if f.ctx.Err() != nil || len(f.todas) >= f.size {
			f.llenando = false
			m.fondoMu.Unlock()
			return
		}
		m.fondoMu.Unlock()

		alta := m.altaFondo
		if alta == nil {
			alta = m.crearParaFondo
		}
		id, err := alta(f.ctx, name)
		if err != nil {
			obsoleto, fallo := "", err.Error()
			if errors.Is(err, ErrDoradoObsoleto) {
				// Lo normal tras cambiar de VMM (vz → hv): no es un fallo
				// pasajero, y reintentar no lo arregla.
				obsoleto, fallo = m.causaFondo(name), ""
			}
			if f.ctx.Err() == nil {
				if obsoleto != "" {
					log.Printf("pool %s: golden is stale (%s): the pool stays empty until it is re-saved:\n%s", name, obsoleto, remedioDorado(name))
				} else {
					log.Printf("pool %s: could not pre-warm a copy: %v", name, err)
				}
			}
			m.fondoMu.Lock()
			f.llenando = false
			f.obsoleto, f.fallo = obsoleto, fallo
			m.fondoMu.Unlock()
			return
		}
		m.fondoMu.Lock()
		f.obsoleto, f.fallo = "", ""
		if f.ctx.Err() != nil || len(f.todas) >= f.size {
			// Lo apagaron o lo encogieron mientras nacía.
			m.fondoMu.Unlock()
			m.retirarDelFondo(id)
			continue
		}
		f.todas[id] = true
		f.listas = append(f.listas, id)
		m.fondoMu.Unlock()
	}
}

// causaFondo es por qué el dorado name no se puede restaurar aquí (otro VMM),
// o "stale" si no se sabe decir mejor.
func (m *Manager) causaFondo(name string) string {
	if s, err := m.loadSnapshot(name); err == nil {
		if c := causaObsoleto(s.VMM, m.origenActual().vmm); c != "" {
			return c
		}
	}
	return "stale"
}

// crearParaFondo da de alta una copia de name por el camino de siempre y
// espera a que esté lista, ganchos de identidad incluidos. Devuelve su id.
func (m *Manager) crearParaFondo(ctx context.Context, name string) (string, error) {
	ctx = context.WithValue(ctx, ctxFondo{}, true)
	nombre := "pool-" + newID()[:8]
	mc, err := m.Run(ctx, api.RunRequest{
		Name:                nombre,
		From:                name,
		Labels:              map[string]string{api.LabelPool: name},
		WaitReady:           true,
		ReadyTimeoutSeconds: 60,
	})
	if err != nil {
		// Un alta cancelada (se apagó o se vació el fondo) puede dejar su
		// entrada fallida en el estado: sin retirarla, impediría borrar la
		// plantilla ("has live instances").
		m.retirarDelFondo(nombre)
		return "", err
	}
	if mc.Ready == api.ReadyWaiting || mc.Ready == api.ReadyFailed {
		m.retirarDelFondo(mc.ID)
		return "", fmt.Errorf("the copy did not become ready (%s)", mc.Ready)
	}
	m.mu.Lock()
	if live := m.byID[mc.ID]; live != nil {
		live.Labels = api.MergeLabels(live.Labels, map[string]string{api.LabelPoolReady: "1"})
		m.persist()
	}
	m.mu.Unlock()
	return mc.ID, nil
}

// peticionLlana dice si la petición se conforma con lo que una copia del
// fondo ya es. Red, volúmenes y límite de CPU se fijan al restaurar; lo único que se puede ajustar después es el nombre, las
// etiquetas, el TTL y qué hacer al vencer.
//
// Con un dueño explícito (cuota de inquilino, docs/authz.md) tampoco se
// reclama: la copia nació con las etiquetas del dorado y la cuota del dueño
// se decide al crearla.
func peticionLlana(req api.RunRequest, snap *api.Snapshot) bool {
	// VCPUs, MemMiB, MemMaxMiB, DiskMiB e Image no cuentan: con From runFrom los
	// ignora y hereda los del dorado (el CLI manda siempre VCPUs y MemMiB).
	if req.CPUPct != 0 || len(req.AllowDomains) != 0 || len(req.VolumeSet()) != 0 || len(req.Shares) != 0 ||
		len(req.Env) != 0 {
		return false
	}
	if _, ok := req.Labels[api.LabelOwner]; ok {
		return false
	}
	if req.Egress != "" && req.Egress != snap.Egress && !(req.Egress == "none" && snap.Egress == "") {
		return false
	}
	return !req.AllowExec || snap.AllowExec
}

// reclamarFondo entrega una copia ya despierta del fondo de req.From, o
// (nil, false) si no hay: nunca espera, el llamador sigue por el camino lento.
// El nombre de req ya lo reservó run (puede ir vacío).
func (m *Manager) reclamarFondo(ctx context.Context, req api.RunRequest) (*api.Machine, bool) {
	if ctx.Value(ctxFondo{}) != nil {
		return nil, false
	}
	m.fondoMu.Lock()
	f := m.fondos[req.From]
	vacio := f == nil || len(f.listas) == 0
	m.fondoMu.Unlock()
	if vacio {
		return nil, false
	}
	snap, _, err := m.loadSnapshotCached(req.From)
	if err != nil || !peticionLlana(req, snap) {
		return nil, false
	}
	for {
		// Reclamar es sacar de listas bajo el candado: dos peticiones
		// simultáneas no pueden llevarse la misma.
		m.fondoMu.Lock()
		f := m.fondos[req.From]
		if f == nil || len(f.listas) == 0 {
			m.fondoMu.Unlock()
			return nil, false
		}
		id := f.listas[0]
		f.listas = f.listas[1:]
		delete(f.todas, id)
		m.fondoMu.Unlock()

		out := m.adjudicarDelFondo(id, req)
		// Sea cual sea el resultado, falta una: se repone.
		m.rellenarFondo(req.From)
		if out != nil {
			return out, true
		}
	}
}

// adjudicarDelFondo convierte la copia id en la máquina que pidió req. nil si
// la copia ya no sirve (murió o la borraron), y en ese caso se retira.
func (m *Manager) adjudicarDelFondo(id string, req api.RunRequest) *api.Machine {
	defer m.lock(id)()
	m.mu.Lock()
	live := m.byID[id]
	if live == nil || live.State != api.StateRunning {
		m.mu.Unlock()
		if live != nil {
			go m.retirarDelFondo(id)
		}
		return nil
	}
	if req.Name == "" {
		req.Name = req.From + "-" + id[:6]
	}
	etiquetas := api.MergeLabels(live.Labels, req.Labels)
	delete(etiquetas, api.LabelPool)
	delete(etiquetas, api.LabelPoolReady)
	now := time.Now()
	live.Name = req.Name
	live.Labels = etiquetas
	live.TTLSeconds = req.TTLSeconds
	live.TTLAt = &now
	live.OnTTL = req.OnTTL
	live.CreatedAt = now
	m.persist()
	out := *live
	m.mu.Unlock()
	m.bus.Publish(api.Event{Time: now, Type: api.EvStarted, ID: id, Name: out.Name,
		Message: fmt.Sprintf("claimed from the pool of %s", req.From)})
	return &out
}

// reanudarFondos, al arrancar el daemon, readopta las copias que sobrevivieron
// y repone lo que falte. Las copias del fondo que no estén vivas se retiran:
// no hay quien las reclame.
func (m *Manager) reanudarFondos() {
	b, err := os.ReadFile(m.fondosPath())
	if err != nil {
		return
	}
	var tam map[string]int
	if err := json.Unmarshal(b, &tam); err != nil {
		log.Printf("pool: %s unreadable, ignoring it: %v", m.fondosPath(), err)
		return
	}
	m.mu.RLock()
	vivas := map[string][]string{}
	var muertas []string
	for id, mc := range m.byID {
		plantilla := mc.Labels[api.LabelPool]
		if plantilla == "" {
			continue
		}
		if mc.State == api.StateRunning && mc.Labels[api.LabelPoolReady] == "1" && tam[plantilla] > 0 {
			vivas[plantilla] = append(vivas[plantilla], id)
		} else {
			muertas = append(muertas, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range muertas {
		go m.retirarDelFondo(id)
	}
	for name, n := range tam {
		if _, err := m.loadSnapshot(name); err != nil || n <= 0 {
			continue
		}
		ids := vivas[name]
		sort.Strings(ids)
		m.fondoMu.Lock()
		f := m.fondoDe(name)
		f.size = min(n, api.PoolMax)
		for _, id := range ids {
			if len(f.todas) >= f.size {
				go m.retirarDelFondo(id)
				continue
			}
			f.todas[id] = true
			f.listas = append(f.listas, id)
		}
		m.fondoMu.Unlock()
		m.rellenarFondo(name)
	}
}

// pararFondos corta la reposición (apagado del daemon). Las copias siguen
// donde estaban: el próximo arranque las readopta.
func (m *Manager) pararFondos() {
	m.fondoMu.Lock()
	defer m.fondoMu.Unlock()
	for _, f := range m.fondos {
		f.cancel()
	}
}

// restablecerFondo vuelve a poner el tamaño de un fondo que se vació sin
// olvidarlo (un commit -replace) y lo repone.
func (m *Manager) restablecerFondo(name string, size int) {
	m.fondoMu.Lock()
	m.fondoDe(name).size = size
	m.fondoMu.Unlock()
	m.rellenarFondo(name)
}
