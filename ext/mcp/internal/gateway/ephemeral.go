package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// EJECUCIÓN EFÍMERA.
//
// El modelo por defecto mantiene una instancia por servicio, reutilizada entre
// llamadas y congelada al quedar ociosa. Es eficiente, pero significa que dos
// llamadas distintas comparten proceso: el estado de una puede filtrarse a la
// siguiente.
//
// En modo efímero cada acción recibe SU PROPIA microVM: se instancia del snapshot
// dorado, atiende la llamada y se destruye. Nace, actúa y muere.
//
// El coste es asumible porque instanciar del snapshot dorado son ~30 ms y el
// disco propio de una instancia son cientos de kilobytes. Lo que se compra a
// cambio es aislamiento total entre acciones: ninguna herramienta puede ver lo
// que hizo la anterior, ni dejarle nada preparado a la siguiente.
//
// La contrapartida es que NO hay estado entre llamadas. Las herramientas que lo
// necesitan (memoria, razonamiento por pasos) deben usar la ruta con sesión.

// ephemeralTimeout acota lo que puede tardar una acción efímera de principio a
// fin: instanciar, llamar y destruir.
const ephemeralTimeout = 90 * time.Second

// ephemeralTTL es la red de seguridad de una efímera del camino lento: si el
// gateway muriera antes de destruirla, el daemon la congela sola al vencer en
// vez de dejarla corriendo para siempre. Mientras la llamada sigue en vuelo se
// renueva (HoldTTL), así que no acota lo que puede durar una acción. Variable
// para que los tests no tengan que esperar dos minutos.
var ephemeralTTL = 120 * time.Second

// callEphemeral ejecuta una herramienta en una microVM de un solo uso.
func (a *aggregator) callEphemeral(ctx context.Context, t *Tool, args json.RawMessage) (any, *rpcFault) {
	ctx, cancel := context.WithTimeout(ctx, ephemeralTimeout)
	defer cancel()

	snap, fault := a.snapshotOf(ctx, t.Service)
	if fault != nil {
		return nil, fault
	}
	start := time.Now()

	// Camino rápido: una instancia ya restaurada y con su sesión MCP abierta.
	//
	// En bucle, y comprobando que sigue viva: el fondo guarda máquinas que el
	// daemon puede haber congelado por TTL desde debajo. Entregar una congelada
	// hacía que el fallo llegara al cliente sin reintento, y un dial a una VM
	// viva cuesta un milisegundo — no se nota en el camino rápido.
	//
	// El TTL se renueva al sacarla y mientras dure la llamada: el fondo la retira
	// a los 2×idle de edad y su TTL es 2×idle+2m, así que una sacada justo antes
	// de la purga se congelaba debajo de una acción larga. Se renueva ANTES de
	// comprobar que responde, para no dejar una vuelta del vigilante del daemon
	// entre la comprobación y la renovación.
	for vm := a.gw.TakeWarm(t.Service); vm != nil; vm = a.gw.TakeWarm(t.Service) {
		release := a.gw.HoldTTL(ctx, vm.ID())
		if err := scheduler.WaitReadyAddr(ctx, vm.Addr(GuestPort), time.Second); err != nil {
			release()
			log.Printf("pool: %s not responding (%v); removing it and trying the next one", vm.ID()[:8], err)
			go a.gw.Client().Remove(context.WithoutCancel(ctx), vm.ID())
			continue
		}
		defer func() {
			// Destruir y reponer EN SEGUNDO PLANO. Hacerlo antes de responder
			// obligaba al cliente a esperar el desmontaje del namespace y el
			// borrado de ficheros: ~100 ms sobre una llamada de 2 ms.
			//
			// La garantía efímera se mantiene: la máquina muere igual, solo que
			// el cliente ya no espera a que ocurra.
			go func() {
				bg := context.WithoutCancel(ctx)
				_ = a.gw.Client().Remove(bg, vm.ID())
				a.gw.FillPool(bg, t.Service, snap)
			}()
		}()
		defer release() // antes que el Remove de arriba: los defer van al revés
		res, fault := a.invoke(ctx, "http://"+vm.Addr(GuestPort), vm.Token(), t, args)
		log.Printf("ephemeral %s: %s in %s (from pool)", vm.ID()[:8], t.Qualified,
			time.Since(start).Round(time.Millisecond))
		return res, fault
	}

	// Camino lento: no había nada preparado. Se instancia ahora y, de paso, se
	// pide que el fondo se rellene para las siguientes.
	defer a.gw.FillPool(context.WithoutCancel(ctx), t.Service, snap)
	mc, err := a.gw.Client().Run(ctx, api.RunRequest{
		From: snap,
		Labels: map[string]string{
			api.LabelService: t.Service,
			"ephemeral":      "true",
			"tool":           t.Name,
		},
		TTLSeconds: int(ephemeralTTL.Seconds()),
	})
	if err != nil {
		return nil, &rpcFault{-32000, fmt.Sprintf("could not instantiate %s: %v", t.Service, err)}
	}

	// Pase lo que pase, la máquina muere. Es la promesa del modo efímero.
	defer func() {
		go func() {
			if err := a.gw.Client().Remove(context.WithoutCancel(ctx), mc.ID); err != nil {
				log.Printf("ephemeral %s: could not destroy it: %v", mc.ID[:8], err)
			}
		}()
	}()
	// Una acción puede durar más que su TTL (semgrep sobre un repo grande pasa
	// del minuto): mientras siga en vuelo, se renueva como un latido. Se suelta
	// antes del Remove.
	release := a.gw.HoldTTL(ctx, mc.ID)
	defer release()

	base := "http://" + mc.Addr(GuestPort)
	if err := scheduler.WaitReadyAddr(ctx, mc.Addr(GuestPort), scheduler.ReadyTimeout); err != nil {
		return nil, &rpcFault{-32000, fmt.Sprintf("%s did not start listening: %v", t.Service, err)}
	}

	sid, err := mcpInit(ctx, base)
	if err != nil {
		return nil, &rpcFault{-32000, fmt.Sprintf("%s: %v", t.Service, err)}
	}
	res, fault := a.invoke(ctx, base, sid, t, args)
	log.Printf("ephemeral %s: %s in %s (cold)", mc.ID[:8], t.Qualified,
		time.Since(start).Round(time.Millisecond))
	return res, fault
}

// invoke manda el tools/call y traduce la respuesta.
func (a *aggregator) invoke(ctx context.Context, base, sid string, t *Tool, args json.RawMessage) (any, *rpcFault) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	// Identificador ÚNICO por llamada. Con un id fijo, dos peticiones concurrentes
	// sobre la misma sesión comparten entrada en la tabla de pendientes del
	// puente: la segunda pisa a la primera y esa se queda esperando una respuesta
	// que ya se entregó a otro. Solo se nota en paralelo, que es cuando peor
	// viene descubrirlo.
	raw, err := mcpCall(ctx, base, sid, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
		nextRPCID(), t.Name, args))
	if err != nil {
		return nil, &rpcFault{-32000, fmt.Sprintf("%s: %v", t.Service, err)}
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, &rpcFault{-32000, "unreadable response from " + t.Service}
	}
	if resp.Error != nil {
		// Un error de validación significa que los tipos no cuadraron pese a la
		// reparación. Se registra lo que se envió: sin eso es imposible saber qué
		// forma les dio el cliente.
		if strings.Contains(strings.ToLower(resp.Error.Message), "expected") {
			log.Printf("%s: server rejected the arguments (%s)\n  sent: %s",
				t.Qualified, resp.Error.Message, trunc(string(args), 400))
		}
		return nil, &rpcFault{resp.Error.Code, resp.Error.Message}
	}
	return json.RawMessage(resp.Result), nil
}

// snapshotOf resuelve qué snapshot dorado corresponde a un servicio.
//
// Cacheado porque estaba en el camino caliente: preguntárselo al daemon en cada
// acción añadía ~100 ms a una llamada cuya ejecución real son 2 ms. La relación
// servicio -> snapshot solo cambia al importar, así que una caché sirve.
func (a *aggregator) snapshotOf(ctx context.Context, service string) (string, *rpcFault) {
	a.snapMu.RLock()
	name, ok := a.snapOf[service]
	a.snapMu.RUnlock()
	if ok {
		return name, nil
	}

	snaps, err := a.gw.Client().Snapshots(ctx)
	if err != nil {
		return "", &rpcFault{-32000, err.Error()}
	}
	a.snapMu.Lock()
	for _, s := range snaps {
		n := s.Name
		if svc := s.Service(); svc != "" {
			a.snapOf[svc] = n
		}
		a.snapOf[n] = n
	}
	name, ok = a.snapOf[service]
	a.snapMu.Unlock()

	if !ok {
		return "", &rpcFault{-32602, "no snapshot for service " + service}
	}
	return name, nil
}
