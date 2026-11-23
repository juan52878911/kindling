//go:build !darwin

package net

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// FONDO DE RANURAS DE RED.
//
// Montar la base de una red (netns, veth, tap, sysctl, barrera IPv6, DNAT) no
// depende de la petición, y es lo que más cuesta de Setup. El daemon tiene N
// redes con la base ya montada (ranuras), repuestas en segundo plano; crear una
// máquina toma una y solo aplica lo propio (aplicarPropio: un iptables-restore
// y poco más).
//
// Aislamiento: una ranura es de UN solo uso. Se monta, se entrega una vez, y se
// destruye con su máquina; nunca vuelve al fondo, así que no hay estado que
// arrastrar de una máquina a otra. Sus reglas son las más restrictivas (ni una
// regla que abra la salida: ya lleva las de none) y no tiene invitado ni VMM: nadie emite por ella mientras
// espera. El netns, el veth y el cgroup se nombran por el id de la máquina
// (Corto), por eso la ranura ya lleva SU id y su índice de subred.

// PoolPorDefecto es cuántas ranuras mantiene el daemon (KLING_NET_POOL).
const PoolPorDefecto = 16

// TamPool lee KLING_NET_POOL: 0 apaga el fondo; vacío o inválido, el defecto.
func TamPool() int {
	v := os.Getenv("KLING_NET_POOL")
	if v == "" {
		return PoolPorDefecto
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 256 {
		log.Printf("net: KLING_NET_POOL=%q is not valid (0-256); using %d", v, PoolPorDefecto)
		return PoolPorDefecto
	}
	return n
}

// montajesCompletos limita a 4 los montajes completos a la vez (y las
// reposiciones del fondo): sin tope, una ráfaga de 50 creaciones monta 50 redes
// sobre 8 vCPU y todas pelean por rtnl_lock.
var montajesCompletos = make(chan struct{}, 4)

// creacionesEsperando cuenta los montajes completos de creaciones que esperan
// hueco en montajesCompletos. Los reponedores del fondo no lo toman mientras
// haya alguno: una ráfaga de altas no hace cola detrás de reposiciones que
// pueden esperar.
var creacionesEsperando atomic.Int32

// tomarMontaje toma un hueco de montajesCompletos. Una creación (creacion)
// lo pide sin más, anotando que espera; un reponedor cede el hueco mientras
// haya creaciones esperando. Se devuelve con soltarMontaje.
func tomarMontaje(creacion bool) {
	if creacion {
		creacionesEsperando.Add(1)
		montajesCompletos <- struct{}{}
		creacionesEsperando.Add(-1)
		return
	}
	for {
		for creacionesEsperando.Load() > 0 {
			time.Sleep(time.Millisecond)
		}
		montajesCompletos <- struct{}{}
		if creacionesEsperando.Load() == 0 {
			return
		}
		<-montajesCompletos // llegó una creación mientras esperaba: se lo deja
	}
}

func soltarMontaje() { <-montajesCompletos }

// MontarRanura monta la base de n (con DNAT) y la deja como ranura: Setup solo
// aplicará lo propio. Si falla, lo que quedó montado se derriba.
func (n *Net) MontarRanura(owner int) error {
	tomarMontaje(false)
	defer soltarMontaje()
	if err := n.montarBase(nuevaTrazaSetup(n.NS), owner, true); err != nil {
		n.Teardown()
		return err
	}
	// La /30 tiene que ser solo nuestra antes de entregarla (subredes.go).
	if err := n.VerificarUnica(); err != nil {
		n.Teardown()
		return err
	}
	// El registro de none (nft) se instala aquí, en el reponedor, y no en la
	// creación: su tabla solo depende de HostIP y del tap.
	n.instalarRegistroIP()
	registroEnRanura.Store(n.NS, struct{}{})
	n.baseLista = true
	n.baseEgress = EgressNone
	return nil
}

// DestruirRanura derriba una ranura que no llegó a máquina.
func (n *Net) DestruirRanura() {
	registroEnRanura.Delete(n.NS)
	n.baseLista = false
	n.baseEgress = ""
	n.Teardown()
	n.SoltarReserva()
}

// Ranura es una red con la base montada, con el id de su futura máquina.
type Ranura struct {
	ID  string
	Net *Net
}

// PlazoRanuraTomada: una ranura tomada (su id entregado a una máquina) que no
// llega a montarse en este plazo se destruye.
const PlazoRanuraTomada = 60 * time.Second

// Fondo es el conjunto de ranuras listas de un daemon.
type Fondo struct {
	tam      int
	crear    func() (*Ranura, error)
	destruir func(*Ranura)
	ahora    func() time.Time
	// Bloquear toma, sin esperar, el cerrojo de ciclo de vida del id de una
	// ranura (nil: sin cerrojo). Recoger no destruye una ranura cuyo id está
	// ocupado: un asignarRed tardío podría montar sobre el netns que se borra.
	Bloquear func(id string) (func(), bool)

	parar    chan struct{} // se cierra en Cerrar; para la recogida
	mu       sync.Mutex
	listas   []*Ranura
	tomadas  map[string]tomada // por id: entregado el id, aún sin asignarRed
	montando map[string]bool   // ns de las que se están creando
	enCurso  int
	// ultimoFallo: tras un fallo de montaje no se reintenta durante
	// esperaTrasFallo (sin root o sin `ip`, cada Tomar fallaría igual).
	ultimoFallo time.Time
	cerrado     bool
	wg          sync.WaitGroup
	wgRecogida  sync.WaitGroup
}

type tomada struct {
	r  *Ranura
	en time.Time
}

// NuevoFondo crea un fondo de tam ranuras. No empieza a reponer: Reponer.
func NuevoFondo(tam int, crear func() (*Ranura, error), destruir func(*Ranura)) *Fondo {
	return &Fondo{tam: tam, crear: crear, destruir: destruir, ahora: time.Now,
		tomadas: map[string]tomada{}, montando: map[string]bool{}, parar: make(chan struct{})}
}

// esperaTrasFallo es lo que el fondo deja pasar tras un montaje fallido.
const esperaTrasFallo = 5 * time.Second

// reponedoresMax: se reponen de 2 en 2 para no competir con las creaciones.
const reponedoresMax = 2

// Reponer lanza, en segundo plano, lo que falta para llenar el fondo.
func (f *Fondo) Reponer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reponerLocked()
}

func (f *Fondo) reponerLocked() {
	if f.ahora().Sub(f.ultimoFallo) < esperaTrasFallo {
		return
	}
	for !f.cerrado && f.enCurso < reponedoresMax && len(f.listas)+f.enCurso < f.tam {
		f.enCurso++
		f.wg.Add(1)
		go f.reponedor()
	}
}

func (f *Fondo) reponedor() {
	defer f.wg.Done()
	for {
		r, err := f.crear()
		f.mu.Lock()
		if r != nil {
			delete(f.montando, r.Net.NS) // ya es de listas, o se destruye abajo
		}
		if err != nil {
			f.enCurso--
			f.ultimoFallo = f.ahora()
			f.mu.Unlock()
			log.Printf("net: could not mount a network slot: %v", err)
			return // sin reintento en bucle: la siguiente Tomar vuelve a probar
		}
		if f.cerrado || len(f.listas) >= f.tam {
			f.enCurso--
			f.mu.Unlock()
			f.destruir(r)
			return
		}
		f.listas = append(f.listas, r)
		if len(f.listas)+f.enCurso-1 >= f.tam {
			// los demás reponedores ya cubren lo que falta
			f.enCurso--
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
	}
}

// Tomar entrega una ranura lista, o nil si no hay: quien llama sigue por el
// camino de siempre. Cada ranura sale una sola vez.
func (f *Fondo) Tomar() *Ranura {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cerrado {
		return nil
	}
	var r *Ranura
	if k := len(f.listas); k > 0 {
		r = f.listas[k-1]
		f.listas[k-1] = nil
		f.listas = f.listas[:k-1]
		f.tomadas[r.ID] = tomada{r: r, en: f.ahora()}
	}
	f.reponerLocked()
	return r
}

// Entregar saca del registro de tomadas la ranura de la máquina id y devuelve
// su red, lista para Setup: desde aquí es de la máquina (si falla, su desmontaje
// la derriba). nil si id no es de una ranura, o si ya se recogió por tardar.
func (f *Fondo) Entregar(id string) *Net {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tomadas[id]
	if !ok {
		return nil
	}
	delete(f.tomadas, id)
	return t.r.Net
}

// Descartar destruye una ranura tomada que no se va a usar. Nunca vuelve al
// fondo: ya pudo ver una máquina.
func (f *Fondo) Descartar(id string) {
	f.mu.Lock()
	t, ok := f.tomadas[id]
	delete(f.tomadas, id)
	f.mu.Unlock()
	if ok {
		f.destruir(t.r)
	}
}

// IniciarRecogida lanza la goroutine que llama a Recoger cada intervalo hasta
// que Cerrar la para (ticker con parada, no time.Tick).
func (f *Fondo) IniciarRecogida(intervalo time.Duration) {
	f.mu.Lock()
	if f.cerrado {
		f.mu.Unlock()
		return
	}
	f.wgRecogida.Add(1)
	f.mu.Unlock()
	go func() {
		defer f.wgRecogida.Done()
		t := time.NewTicker(intervalo)
		defer t.Stop()
		for {
			select {
			case <-f.parar:
				return
			case <-t.C:
				f.Recoger()
			}
		}
	}()
}

// Recoger destruye las ranuras tomadas hace más de PlazoRanuraTomada que no
// llegaron a asignarRed (la creación se abandonó). Devuelve cuántas. Cada una
// se destruye con el cerrojo de ciclo de vida de su id (Bloquear); si está
// ocupado, queda para la vuelta siguiente.
func (f *Fondo) Recoger() int {
	f.mu.Lock()
	var viejas []string
	for id, t := range f.tomadas {
		if f.ahora().Sub(t.en) > PlazoRanuraTomada {
			viejas = append(viejas, id)
		}
	}
	f.mu.Unlock()
	n := 0
	for _, id := range viejas {
		soltar := func() {}
		if f.Bloquear != nil {
			s, ok := f.Bloquear(id)
			if !ok {
				continue
			}
			soltar = s
		}
		f.mu.Lock()
		t, ok := f.tomadas[id]
		if ok && f.ahora().Sub(t.en) > PlazoRanuraTomada {
			delete(f.tomadas, id)
		} else {
			ok = false
		}
		f.mu.Unlock()
		if ok {
			log.Printf("net: slot %s was taken but never mounted; destroying it", t.r.Net.NS)
			f.destruir(t.r)
			n++
		}
		soltar()
	}
	return n
}

// Propia dice si el netns ns o el prefijo de id es de una ranura que el fondo
// tiene en espera, tomada o a medio crear: el barrido de huérfanos no la toca.
func (f *Fondo) Propia(ns string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.montando[ns] {
		return true
	}
	for _, r := range f.listas {
		if r.Net.NS == ns {
			return true
		}
	}
	for _, t := range f.tomadas {
		if t.r.Net.NS == ns {
			return true
		}
	}
	return false
}

// PrefijoEnUso dice si el prefijo (Corto) del id es de una ranura del fondo:
// un id nuevo no puede repetirlo.
func (f *Fondo) PrefijoEnUso(corto string) bool { return f.Propia("kl-" + corto) }

// Marcar apunta que ns se está creando (antes de montarlo), para que Propia lo
// cubra; Desmarcar lo quita al acabar.
func (f *Fondo) Marcar(ns string) {
	f.mu.Lock()
	f.montando[ns] = true
	f.mu.Unlock()
}

func (f *Fondo) Desmarcar(ns string) {
	f.mu.Lock()
	delete(f.montando, ns)
	f.mu.Unlock()
}

// Cerrar para el fondo y destruye las ranuras en espera. Las tomadas siguen
// su camino.
func (f *Fondo) Cerrar() {
	f.mu.Lock()
	if !f.cerrado {
		close(f.parar)
	}
	f.cerrado = true
	listas := f.listas
	f.listas = nil
	f.mu.Unlock()
	f.wgRecogida.Wait()
	f.wg.Wait()
	f.mu.Lock()
	listas = append(listas, f.listas...)
	f.listas = nil
	f.mu.Unlock()
	for _, r := range listas {
		f.destruir(r)
	}
}

// Len es cuántas ranuras hay listas.
func (f *Fondo) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.listas)
}

func (r *Ranura) String() string { return fmt.Sprintf("slot %s (index %d)", r.ID, r.Net.Index) }
