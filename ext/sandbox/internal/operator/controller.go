package operator

// El bucle del operador: list + watch contra Kubernetes, reconciliando cada
// Sandbox contra el frontal. Sin client-go no hay workqueue ni informer, así
// que esto es la versión de mano: una caché en memoria de los objetos vistos,
// un candado que serializa toda reconciliación (evita que un watch y un
// resync se pisen creando dos veces el mismo sandbox) y un resync periódico
// que hace de red de seguridad para lo que un evento no repite por sí solo
// (el estado del frontal cambia sin que Kubernetes se entere).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/frontal"
)

// DefaultResync es cada cuánto se refresca el estado de cada sandbox desde el
// frontal y se reintenta lo que hubiera fallado (crear, borrar, renovar).
const DefaultResync = 30 * time.Second

// defaultBackoffBase es el primer plazo de espera al reconectar tras un list
// o un watch fallidos.
const defaultBackoffBase = time.Second

// Controller reconcilia los Sandbox del clúster contra el frontal.
type Controller struct {
	Kube    *KubeClient
	Frontal *FrontalClient
	Log     *log.Logger
	// Resync es el periodo del resync; 0 usa DefaultResync.
	Resync time.Duration
	// BackoffBase es el primer plazo de espera al reconectar; 0 usa
	// defaultBackoffBase. Campo (no variable de paquete) para que los tests
	// puedan encogerlo por instancia sin un global mutable que una
	// reconciliación en marcha de otro test pudiera leer a la vez.
	BackoffBase time.Duration

	// cerrojos serializa las reconciliaciones DE UN MISMO Sandbox (vengan de
	// un evento de watch o del resync): es lo que impide crear dos veces su
	// sandbox si ambos caminos lo procesan casi a la vez. Por objeto y no uno
	// global: un frontal lento con un Sandbox no para a los demás.
	cerrojos map[string]*cerrojoCR

	mu    sync.Mutex
	cache map[string]*Sandbox
}

// cerrojoCR es el cerrojo de un Sandbox y cuántos lo esperan o lo tienen, para
// borrarlo del mapa cuando nadie lo usa.
type cerrojoCR struct {
	mu sync.Mutex
	n  int
}

// bloquear toma el cerrojo del Sandbox key y devuelve cómo soltarlo.
func (c *Controller) bloquear(key string) func() {
	c.mu.Lock()
	if c.cerrojos == nil {
		c.cerrojos = map[string]*cerrojoCR{}
	}
	l := c.cerrojos[key]
	if l == nil {
		l = &cerrojoCR{}
		c.cerrojos[key] = l
	}
	l.n++
	c.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		c.mu.Lock()
		if l.n--; l.n == 0 {
			delete(c.cerrojos, key)
		}
		c.mu.Unlock()
	}
}

// NewController construye el controlador. kube y frontal ya vienen
// configurados (ver main.go): el operador no sabe leer flags ni entorno.
func NewController(kube *KubeClient, frontal *FrontalClient, logger *log.Logger) *Controller {
	if logger == nil {
		logger = log.Default()
	}
	return &Controller{Kube: kube, Frontal: frontal, Log: logger, cache: map[string]*Sandbox{}}
}

func (c *Controller) resync() time.Duration {
	if c.Resync > 0 {
		return c.Resync
	}
	return DefaultResync
}

// backoffBase da el plazo inicial de reconexión configurado, o el valor por
// defecto si no se fijó ninguno.
func (c *Controller) backoffBase() time.Duration {
	if c.BackoffBase > 0 {
		return c.BackoffBase
	}
	return defaultBackoffBase
}

// Run bloquea hasta que ctx se cancele. list, luego watch desde el
// resourceVersion de la lista; si el watch se corta, reconecta desde el
// último resourceVersion visto; si el API server contesta 410 (el
// resourceVersion ya no está en su histórico), relista desde cero.
func (c *Controller) Run(ctx context.Context) error {
	go c.resyncLoop(ctx)
	backoff := c.backoffBase()
	for ctx.Err() == nil {
		rv, err := c.relist(ctx)
		if err != nil {
			c.Log.Printf("operator: list: %v", err)
			if !sleepCtx(ctx, backoff) {
				break
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = c.backoffBase()
		c.watchUntilGone(ctx, rv)
	}
	return ctx.Err()
}

// relist trae todos los Sandbox, reconcilia cada uno (por si el operador
// estuvo parado y algo cambió mientras tanto) y devuelve el resourceVersion
// desde el que arrancar el watch.
func (c *Controller) relist(ctx context.Context) (string, error) {
	list, err := c.Kube.List(ctx)
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for i := range list.Items {
		sb := &list.Items[i]
		c.remember(sb)
		seen[sb.Key()] = true
	}
	for key := range seen {
		if latest := c.lookup(key); latest != nil {
			c.reconcile(ctx, latest)
		}
	}
	// Lo que estaba en caché y ya no aparece en la lista se borró mientras el
	// operador no miraba (o perdió su último finalizer): fuera de la caché.
	c.mu.Lock()
	for k := range c.cache {
		if !seen[k] {
			delete(c.cache, k)
		}
	}
	c.mu.Unlock()
	return list.Metadata.ResourceVersion, nil
}

// watchUntilGone mantiene el watch abierto, reconectando con backoff cuando
// se corta, hasta que ctx se cancele o el server pida un relist (410).
func (c *Controller) watchUntilGone(ctx context.Context, rv string) {
	backoff := c.backoffBase()
	for ctx.Err() == nil {
		abierto := time.Now()
		err := c.Kube.Watch(ctx, rv, func(kind string, sb *Sandbox) {
			if sb.Metadata.ResourceVersion != "" {
				rv = sb.Metadata.ResourceVersion
			}
			if kind == "DELETED" {
				c.forget(sb.Key())
				return
			}
			if kind == "BOOKMARK" {
				return
			}
			c.remember(sb)
			// Se reconcilia la copia más nueva que la caché conoce, no el sb
			// tal cual llegó: remember puede haberlo descartado por
			// anticuado (ver su comentario), y reconciliar el sb crudo de
			// todos modos volvería a procesar un paso ya superado.
			if latest := c.lookup(sb.Key()); latest != nil {
				c.reconcile(ctx, latest)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errGone) {
			return // Run relista.
		}
		if err != nil {
			c.Log.Printf("operator: watch: %v", err)
		}
		// Un watch que aguantó abierto un buen rato y se cerró es lo normal (el
		// API server los corta por tiempo): no cuenta como racha de fallos, y
		// arrastrar el backoff de fallos anteriores solo retrasaría el
		// siguiente sin motivo.
		if time.Since(abierto) > time.Minute {
			backoff = c.backoffBase()
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// resyncLoop reconcilia periódicamente todo lo que hay en caché: es lo que
// mantiene status.state al día (el frontal cambia sin avisar a Kubernetes) y
// reintenta lo que una reconciliación anterior dejó a medias.
func (c *Controller) resyncLoop(ctx context.Context) {
	t := time.NewTicker(c.resync())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.resyncPasada(ctx)
		}
	}
}

// resyncPasada reconcilia todo lo que hay en caché con UNA lista del frontal.
// Antes cada Sandbox hacía su GET (con 30 s de plazo) y su lista completa si
// tenía que adoptar o borrar: con el frontal caído, 100 objetos eran 50 min de
// pasada en serie. Si la lista falla, la pasada no puede hacer nada útil y se
// salta entera; la siguiente lo vuelve a intentar.
func (c *Controller) resyncPasada(ctx context.Context) {
	v, err := c.tomarVista(ctx)
	if err != nil {
		c.Log.Printf("operator: resync: listing the frontal's sandboxes: %v", err)
		return
	}
	for _, sb := range c.snapshot() {
		if ctx.Err() != nil {
			return
		}
		c.reconcileCon(ctx, sb, v)
	}
}

// vistaFrontal es una foto de los sandboxes del tenant, tomada una vez por
// pasada de resync.
type vistaFrontal struct {
	todos    []frontal.Sandbox
	porID    map[string]frontal.Sandbox
	completa bool // contestaron todos los hosts

	// caidos son los hosts cuyo GET ya falló en esta pasada, con su error.
	// Con un host caído, cada objeto suyo hacía su GET en serie (plazo
	// incluido): uno basta para saber que no contesta hasta la próxima.
	mu     sync.Mutex
	caidos map[string]error
}

// caido es el error con que el host de id ya falló en esta pasada, o nil.
func (v *vistaFrontal) caido(id string) error {
	host, _, _ := strings.Cut(id, "/")
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.caidos[host]
}

// apuntarCaido recuerda que el GET de id falló con err (no un 404).
func (v *vistaFrontal) apuntarCaido(id string, err error) {
	host, _, _ := strings.Cut(id, "/")
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.caidos == nil {
		v.caidos = map[string]error{}
	}
	v.caidos[host] = err
}

func (c *Controller) tomarVista(ctx context.Context) (*vistaFrontal, error) {
	todos, completa, err := c.Frontal.List(ctx)
	if err != nil {
		return nil, err
	}
	v := &vistaFrontal{todos: todos, porID: make(map[string]frontal.Sandbox, len(todos)), completa: completa}
	for _, sb := range todos {
		v.porID[sb.ID] = sb
	}
	return v, nil
}

// ver trae el sandbox id: de la vista si está en ella, si no del frontal. Un
// "no está" de la vista no se cree: la foto es del principio de la pasada, y
// el camino del watch pudo crearlo después (o estar en un host que no
// contestó). Se confirma con su GET, que es lo raro.
//
// En una pasada, un GET que falla (y no es un 404) marca su host: los demás
// objetos de ese host reciben el mismo error sin preguntar.
func (c *Controller) ver(ctx context.Context, v *vistaFrontal, id string) (*frontal.Sandbox, error) {
	if v == nil {
		return c.Frontal.Get(ctx, id)
	}
	if sb, ok := v.porID[id]; ok {
		return &sb, nil
	}
	if err := v.caido(id); err != nil {
		return nil, err
	}
	sb, err := c.Frontal.Get(ctx, id)
	if err != nil && !IsFrontalNotFound(err) {
		if _, errRuta := rutaSandbox(id); errRuta == nil {
			v.apuntarCaido(id, err)
		}
	}
	return sb, err
}

// deUID son los sandboxes etiquetados con uid, y si la lista estaba completa.
// Como en ver, un "ninguno" de la vista se confirma con una lista nueva.
func (c *Controller) deUID(ctx context.Context, v *vistaFrontal, uid string) ([]frontal.Sandbox, bool, error) {
	if v != nil {
		if sbs := conUID(v.todos, uid); len(sbs) > 0 || !v.completa {
			// Con la foto parcial, otra lista lo sería también (el host que
			// no contestó sigue sin contestar) y costaba una por objeto: el
			// "ninguno" queda en parcial, y adoptar no crea con él.
			return sbs, v.completa, nil
		}
	}
	return c.Frontal.ByUID(ctx, uid)
}

// esDe dice si sb es el sandbox que este operador creó para cur: lleva su UID.
// status.id lo escribe quien tenga permiso sobre sandboxes/status, y el token
// del frontal es uno para todos los namespaces, así que sin esto un objeto
// que apunte su status.id al sandbox de otro lo renovaba y, al borrarse, se lo
// llevaba por delante.
func esDe(sb *frontal.Sandbox, cur *Sandbox) bool {
	return sb.Labels[LabelK8sUID] == cur.Metadata.UID
}

// remember guarda sb en la caché, PERO solo si es al menos tan nuevo como lo
// que ya había (por resourceVersion). Sin esto, un evento de watch que
// reparte el eco de un escrito propio anterior (más lento en volver que los
// pasos siguientes de la misma reconciliación) podría hacer retroceder la
// caché a un estado ya superado — y, en concreto, a un "finalizer puesto,
// aún sin crear" después de que la creación ya hubiera terminado, disparando
// una segunda creación en el frontal.
func (c *Controller) remember(sb *Sandbox) {
	cp := *sb
	key := cp.Key()
	c.mu.Lock()
	if old, ok := c.cache[key]; ok && rvOf(old) > rvOf(&cp) {
		c.mu.Unlock()
		return
	}
	c.cache[key] = &cp
	c.mu.Unlock()
}

// rvOf interpreta el resourceVersion como el entero que es en todo
// Kubernetes real (un contador de etcd); un valor vacío o no numérico (solo
// puede pasar en pruebas que construyen el objeto a mano) cuenta como el más
// antiguo posible, nunca como el más nuevo.
func rvOf(sb *Sandbox) int64 {
	n, _ := strconv.ParseInt(sb.Metadata.ResourceVersion, 10, 64)
	return n
}

func (c *Controller) lookup(key string) *Sandbox {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cache[key]
}

func (c *Controller) forget(key string) {
	c.mu.Lock()
	delete(c.cache, key)
	c.mu.Unlock()
}

func (c *Controller) snapshot() []*Sandbox {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Sandbox, 0, len(c.cache))
	for _, sb := range c.cache {
		out = append(out, sb)
	}
	return out
}

// ---- reconciliación

// reconcile decide y aplica UN paso para un Sandbox, preguntando al frontal
// por él (el camino del watch). Ver reconcileCon.
func (c *Controller) reconcile(ctx context.Context, sb *Sandbox) { c.reconcileCon(ctx, sb, nil) }

// reconcileCon decide y aplica UN paso para un Sandbox. Nunca muta sb (puede
// ser el mismo puntero que vive en la caché mientras otra goroutine lo lee):
// todo cambio se hace sobre una copia local que, si algo se escribió de verdad
// en Kubernetes, se guarda de vuelta en la caché con remember. El cerrojo del
// objeto asegura que dos llamadas (una de watch, una de resync) no se crucen,
// y sb solo dice QUÉ Sandbox reconciliar: el estado se relee de la caché
// dentro del candado. v es la foto del frontal de la pasada de resync (nil:
// se le pregunta).
func (c *Controller) reconcileCon(ctx context.Context, sb *Sandbox, v *vistaFrontal) {
	defer c.bloquear(sb.Key())()

	// sb se leyó de la caché ANTES de tomar reconcileMu, y mientras se
	// esperaba el candado otra reconciliación pudo avanzar este mismo
	// Sandbox: el resync, por ejemplo, fotografía la caché con "finalizer
	// puesto, aún sin crear" justo cuando la reconciliación del watch está
	// creándolo, y al entrar después volvería a ver status.id vacío y lo
	// crearía dos veces. Así que se reconcilia lo más nuevo que la caché
	// conoce ya dentro del candado; si ya no está (un DELETED lo sacó), no
	// queda nada que hacer.
	latest := c.lookup(sb.Key())
	if latest == nil {
		return
	}
	if rvOf(latest) >= rvOf(sb) {
		sb = latest
	}

	cur := *sb
	ns, name := cur.Metadata.Namespace, cur.Metadata.Name
	changed := false

	if cur.Metadata.DeletionTimestamp != nil {
		c.reconcileDeleting(ctx, &cur, v)
		return
	}

	if !cur.Metadata.HasFinalizer() {
		fin := append(append([]string{}, cur.Metadata.Finalizers...), CleanupFinalizer)
		updated, err := c.Kube.PatchFinalizers(ctx, ns, name, fin)
		if err != nil {
			c.Log.Printf("operator: %s/%s: adding finalizer: %v", ns, name, err)
			return
		}
		cur.Metadata.Finalizers = fin
		// El resourceVersion de la respuesta, no el de antes del patch: si
		// no se actualiza aquí, un watch que le devuelva a esta misma
		// reconciliación su PROPIO escrito (Kubernetes hace eco de lo que el
		// operador cambia porque está suscrito a ello) parecería tan nuevo
		// como esta copia local aunque sea un paso ya superado, y remember
		// no lo rechazaría — reabriendo la puerta a crear el sandbox dos
		// veces (ver el comentario de mergePatch en kube.go).
		cur.Metadata.ResourceVersion = updated.Metadata.ResourceVersion
		changed = true
		c.remember(&cur)
	}

	if cur.Status.ID == "" {
		// Antes de crear, se busca uno ya creado para este objeto: un Create
		// cuya respuesta se perdió, o un status que no llegó a escribirse.
		out, err := c.adoptar(ctx, &cur, v)
		if err != nil {
			c.Log.Printf("operator: %s/%s: looking for its sandbox: %v", ns, name, err)
			c.setMessage(ctx, &cur, err.Error())
			return
		}
		if out == nil {
			out, err = c.Frontal.Create(ctx, cur.Spec, cur.Metadata.UID)
		}
		if err != nil {
			c.Log.Printf("operator: %s/%s: creating sandbox: %v", ns, name, err)
			c.setMessage(ctx, &cur, err.Error())
			return
		}
		cur.Status = SandboxStatus{
			ID: out.ID, Host: out.Host, State: string(out.State),
			ExpiresAt: out.ExpiresAt, ObservedGeneration: cur.Metadata.Generation,
		}
		if updated, err := c.Kube.PatchStatus(ctx, ns, name, cur.Status); err != nil {
			c.Log.Printf("operator: %s/%s: writing status after create: %v", ns, name, err)
			// El sandbox YA nace en el frontal aunque esto falle: no se
			// reintenta el create (dejaría un sandbox huérfano más), el
			// próximo resync reintentará solo escribir el status con el
			// id que ahora mismo solo conoce esta goroutine... y se
			// perdería si el proceso muere aquí. Es el hueco que un CRD
			// sin más estado que Kubernetes no puede cerrar del todo;
			// se acepta porque el resync de 30s deja el status como
			// mucho ese tiempo desfasado, nunca duplica la creación
			// (status.id ya no está vacío en esta copia local).
		} else {
			cur.Metadata.ResourceVersion = updated.Metadata.ResourceVersion
		}
		c.remember(&cur)
		c.Log.Printf("operator: %s/%s: created sandbox %s", ns, name, out.ID)
		return
	}

	// Ya existe. Refresco periódico: lo que el frontal sabe manda. Si
	// desapareció (lo borró la limpieza por abandono, o alguien a mano), se
	// marca "gone" sin volver a crearlo: status.id ya no está vacío, y no se
	// recrea nada solo porque el frontal lo olvidó.
	out, err := c.ver(ctx, v, cur.Status.ID)
	switch {
	case err == nil && !esDe(out, &cur):
		// status.id apunta a un sandbox que no es de este objeto: no se
		// renueva ni se borra nunca, y se dice.
		msg := fmt.Sprintf("status.id %s is not this object's sandbox; ignoring it", cur.Status.ID)
		c.Log.Printf("operator: %s/%s: %s", ns, name, msg)
		c.setMessage(ctx, &cur, msg)
		return
	case err == nil:
		if cur.Status.State != string(out.State) || !sameTime(cur.Status.ExpiresAt, out.ExpiresAt) || hasMessage(cur.Status.Message) {
			cur.Status.State = string(out.State)
			cur.Status.ExpiresAt = out.ExpiresAt
			cur.Status.Host = out.Host
			cur.Status.Message = strPtr("")
			changed = true
		}
		// Si spec cambió (hoy, solo ttlSeconds es mutable) se renueva.
		if cur.Status.ObservedGeneration != cur.Metadata.Generation {
			out, err := c.Frontal.Renew(ctx, cur.Status.ID, cur.Spec.TTLSeconds)
			if err != nil {
				c.Log.Printf("operator: %s/%s: renewing: %v", ns, name, err)
				c.setMessage(ctx, &cur, err.Error())
				return
			}
			cur.Status.State = string(out.State)
			cur.Status.ExpiresAt = out.ExpiresAt
			cur.Status.ObservedGeneration = cur.Metadata.Generation
			cur.Status.Message = strPtr("")
			changed = true
		}
	case IsFrontalNotFound(err):
		if cur.Status.State != StateGone {
			cur.Status.State = StateGone
			changed = true
		}
	default:
		c.Log.Printf("operator: %s/%s: refreshing status: %v", ns, name, err)
		if cur.Status.ObservedGeneration != cur.Metadata.Generation {
			// Sin verlo no se sabe si es suyo: la renovación espera.
			c.setMessage(ctx, &cur, err.Error())
			return
		}
	}

	if changed {
		updated, err := c.Kube.PatchStatus(ctx, ns, name, cur.Status)
		if err != nil {
			c.Log.Printf("operator: %s/%s: writing status: %v", ns, name, err)
			return
		}
		cur.Metadata.ResourceVersion = updated.Metadata.ResourceVersion
		c.remember(&cur)
	}
}

// reconcileDeleting maneja un Sandbox con deletionTimestamp: borra en el
// frontal y quita el finalizer. Si el borrado falla, se deja el finalizer
// puesto a propósito — el próximo resync (o el próximo evento de watch)
// vuelve a intentarlo, y así nunca se pierde un sandbox huérfano por un fallo
// pasajero del frontal.
func (c *Controller) reconcileDeleting(ctx context.Context, cur *Sandbox, v *vistaFrontal) {
	ns, name := cur.Metadata.Namespace, cur.Metadata.Name
	if !cur.Metadata.HasFinalizer() {
		return // nada que limpiar, Kubernetes ya puede borrarlo.
	}
	if id := cur.Status.ID; id != "" {
		// Solo se borra si es suyo (esDe): un status.id ajeno no se toca, y
		// uno que ni es un id tampoco.
		if _, err := rutaSandbox(id); err != nil {
			c.Log.Printf("operator: %s/%s: not deleting %v", ns, name, err)
		} else {
			sb, err := c.ver(ctx, v, id)
			switch {
			case IsFrontalNotFound(err):
			case err != nil:
				c.Log.Printf("operator: %s/%s: looking at sandbox %s: %v", ns, name, id, err)
				return
			case !esDe(sb, cur):
				c.Log.Printf("operator: %s/%s: status.id %s is not this object's sandbox; not deleting it", ns, name, id)
			default:
				if err := c.Frontal.Delete(ctx, id); err != nil {
					c.Log.Printf("operator: %s/%s: deleting sandbox %s: %v", ns, name, id, err)
					return
				}
			}
		}
	}
	// Y los que lleven su UID sin estar en el status (un Create cuya respuesta
	// se perdió): sin esto, un objeto borrado antes de escribir su status
	// soltaba el finalizer y dejaba el sandbox vivo hasta el abandono. Con una
	// lista nueva y no la de la pasada: antes de soltar el finalizer, nada que
	// se haya creado después de la foto puede quedarse fuera.
	if uid := cur.Metadata.UID; uid != "" {
		sbs, completa, err := c.deUID(ctx, nil, uid)
		if err != nil {
			c.Log.Printf("operator: %s/%s: looking for its sandboxes: %v", ns, name, err)
			return
		}
		for _, sb := range sbs {
			if err := c.Frontal.Delete(ctx, sb.ID); err != nil {
				c.Log.Printf("operator: %s/%s: deleting sandbox %s: %v", ns, name, sb.ID, err)
				return
			}
		}
		if !completa {
			// Un host caído no deja colgados a todos los objetos en
			// Terminating: el sandbox del status ya se resolvió arriba, y un
			// duplicado en ese host (un Create perdido) lo acaba borrando la
			// limpieza por abandono del frontal. Sin status.id no hay nada
			// resuelto, y se espera a una lista completa.
			if cur.Status.ID == "" {
				c.Log.Printf("operator: %s/%s: partial sandbox list from the frontal; waiting for every host to answer", ns, name)
				return
			}
			c.Log.Printf("operator: %s/%s: partial sandbox list; a duplicate on a host that did not answer is left to the frontal's cleanup", ns, name)
		}
	}
	fin := removeFinalizer(cur.Metadata.Finalizers)
	updated, err := c.Kube.PatchFinalizers(ctx, ns, name, fin)
	if err != nil {
		c.Log.Printf("operator: %s/%s: removing finalizer: %v", ns, name, err)
		return
	}
	cur.Metadata.Finalizers = fin
	cur.Metadata.ResourceVersion = updated.Metadata.ResourceVersion
	c.remember(cur)
	c.Log.Printf("operator: %s/%s: cleaned up sandbox %s", ns, name, cur.Status.ID)
}

// adoptar busca en el frontal el sandbox ya creado para cur (por su UID). Si
// hay más de uno (duplicados de un operador anterior), se queda el primero y
// borra el resto. nil sin error: no hay ninguno, hay que crearlo.
//
// Falla cerrado: con una lista parcial y ninguno a la vista, el suyo puede
// estar en el host que no contestó, y crear otro lo duplicaría.
func (c *Controller) adoptar(ctx context.Context, cur *Sandbox, v *vistaFrontal) (*frontal.Sandbox, error) {
	uid := cur.Metadata.UID
	if uid == "" {
		return nil, nil
	}
	sbs, completa, err := c.deUID(ctx, v, uid)
	if err != nil {
		return nil, err
	}
	if len(sbs) == 0 {
		if !completa {
			return nil, errors.New("frontal: partial sandbox list; not creating until every host answers")
		}
		return nil, nil
	}
	sort.Slice(sbs, func(i, j int) bool { return sbs[i].ID < sbs[j].ID })
	for _, sobra := range sbs[1:] {
		if err := c.Frontal.Delete(ctx, sobra.ID); err != nil {
			c.Log.Printf("operator: %s/%s: deleting duplicate sandbox %s: %v", cur.Metadata.Namespace, cur.Metadata.Name, sobra.ID, err)
		}
	}
	c.Log.Printf("operator: %s/%s: adopting sandbox %s", cur.Metadata.Namespace, cur.Metadata.Name, sbs[0].ID)
	return &sbs[0], nil
}

func (c *Controller) setMessage(ctx context.Context, cur *Sandbox, msg string) {
	if cur.Status.Message != nil && *cur.Status.Message == msg {
		return
	}
	cur.Status.Message = strPtr(msg)
	updated, err := c.Kube.PatchStatus(ctx, cur.Metadata.Namespace, cur.Metadata.Name, SandboxStatus{Message: strPtr(msg)})
	if err != nil {
		c.Log.Printf("operator: %s/%s: writing error message: %v", cur.Metadata.Namespace, cur.Metadata.Name, err)
		return
	}
	cur.Metadata.ResourceVersion = updated.Metadata.ResourceVersion
	c.remember(cur)
}

// strPtr y hasMessage existen por status.message (ver su comentario en
// types.go): un puntero a "" limpia el mensaje de verdad en un merge patch,
// donde un string en cero con omitempty jamás saldría en el JSON.
func strPtr(s string) *string { return &s }

func hasMessage(m *string) bool { return m != nil && *m != "" }

func removeFinalizer(finalizers []string) []string {
	out := make([]string, 0, len(finalizers))
	for _, f := range finalizers {
		if f != CleanupFinalizer {
			out = append(out, f)
		}
	}
	return out
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// ---- utilidades

// sleepCtx espera d o hasta que ctx se cancele; devuelve false si fue el
// contexto quien lo cortó.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// nextBackoff dobla el plazo con un poco de aleatoriedad (evita que, si el
// API server se recupera, todos los operadores del clúster reconecten en el
// mismo instante) y lo tapa en 30 s.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(d)/4 + 1))
	return d + jitter
}
