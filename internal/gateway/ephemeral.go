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
		if err := a.gw.client.Remove(context.WithoutCancel(ctx), mc.ID); err != nil {
			log.Printf("efímera %s: no pude destruirla: %v", mc.ID[:8], err)
		}
	}()

	base := "http://" + mc.IP + ":" + fmt.Sprint(GuestPort)
	if err := waitReady(ctx, mc.IP, GuestPort, readyTimeout); err != nil {
		return nil, &rpcFault{-32000, fmt.Sprintf("%s no empezó a escuchar: %v", t.Service, err)}
	}

	sid, err := mcpInit(ctx, base)
	if err != nil {
		return nil, &rpcFault{-32000, fmt.Sprintf("%s: %v", t.Service, err)}
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	raw, err := mcpCall(ctx, base, sid, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
		t.Name, args))
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
	log.Printf("efímera %s: %s en %s", mc.ID[:8], t.Qualified, time.Since(start).Round(time.Millisecond))

	if resp.Error != nil {
		return nil, &rpcFault{resp.Error.Code, resp.Error.Message}
	}
	return json.RawMessage(resp.Result), nil
}

// snapshotOf resuelve qué snapshot dorado corresponde a un servicio.
func (a *aggregator) snapshotOf(ctx context.Context, service string) (string, *rpcFault) {
	snaps, err := a.gw.client.Snapshots(ctx)
	if err != nil {
		return "", &rpcFault{-32000, err.Error()}
	}
	for _, s := range snaps {
		if s.Service() == service || s.Name == service {
			return s.Name, nil
		}
	}
	return "", &rpcFault{-32602, "no hay snapshot para el servicio " + service}
}
