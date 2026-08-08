package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/juan52878911/kindling/internal/api"
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
	if vm := a.gw.pool.take(t.Service); vm != nil {
		defer func() {
			// Destruir y reponer EN SEGUNDO PLANO. Hacerlo antes de responder
			// obligaba al cliente a esperar el desmontaje del namespace y el
			// borrado de ficheros: ~100 ms sobre una llamada de 2 ms.
			//
			// La garantía efímera se mantiene: la máquina muere igual, solo que
			// el cliente ya no espera a que ocurra.
			go func() {
				bg := context.WithoutCancel(ctx)
				_ = a.gw.client.Remove(bg, vm.id)
				a.gw.pool.fill(bg, t.Service, snap)
			}()
		}()
		res, fault := a.invoke(ctx, "http://"+vm.ip+":"+itoa(GuestPort), vm.session, t, args)
		log.Printf("efímera %s: %s en %s (del fondo)", vm.id[:8], t.Qualified,
			time.Since(start).Round(time.Millisecond))
		return res, fault
	}

	// Camino lento: no había nada preparado. Se instancia ahora y, de paso, se
	// pide que el fondo se rellene para las siguientes.
	defer a.gw.pool.fill(context.WithoutCancel(ctx), t.Service, snap)
	mc, err := a.gw.client.Run(ctx, api.RunRequest{
		From: snap,
		Labels: map[string]string{
			api.LabelService: t.Service,
			"ephemeral":      "true",
			"tool":           t.Name,
		},
		// Red de seguridad: si el gateway muriera antes de destruirla, el daemon
		// la congela sola en vez de dejarla corriendo para siempre.
		TTLSeconds: 120,
	})
	if err != nil {
		return nil, &rpcFault{-32000, fmt.Sprintf("no pude instanciar %s: %v", t.Service, err)}
	}

	// Pase lo que pase, la máquina muere. Es la promesa del modo efímero.
	defer func() {
		go func() {
			if err := a.gw.client.Remove(context.WithoutCancel(ctx), mc.ID); err != nil {
				log.Printf("efímera %s: no pude destruirla: %v", mc.ID[:8], err)
			}
		}()
	}()

	base := "http://" + mc.IP + ":" + fmt.Sprint(GuestPort)
	if err := waitReady(ctx, mc.IP, GuestPort, readyTimeout); err != nil {
		return nil, &rpcFault{-32000, fmt.Sprintf("%s no empezó a escuchar: %v", t.Service, err)}
	}

	sid, err := mcpInit(ctx, base)
	if err != nil {
		return nil, &rpcFault{-32000, fmt.Sprintf("%s: %v", t.Service, err)}
	}
	res, fault := a.invoke(ctx, base, sid, t, args)
	log.Printf("efímera %s: %s en %s (en frío)", mc.ID[:8], t.Qualified,
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
		return nil, &rpcFault{-32000, "respuesta ilegible de " + t.Service}
	}
	if resp.Error != nil {
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

	snaps, err := a.gw.client.Snapshots(ctx)
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
		return "", &rpcFault{-32602, "no hay snapshot para el servicio " + service}
	}
	return name, nil
}
