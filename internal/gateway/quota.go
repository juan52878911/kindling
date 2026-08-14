// Cuotas por token/tenant: reparto justo, NO seguridad.
//
// Un "tenant" es un token con nombre. Sirve para que el gateway se reparta con
// justicia entre varios clientes: limitar cuántas instancias despiertas y
// cuántas peticiones en vuelo puede acaparar cada uno.
//
// IMPORTANTE — esto NO es una frontera de seguridad. Todas las microVMs corren
// bajo el MISMO daemon y cuelgan del MISMO bridge de red, así que un tenant
// puede alcanzar lo de otro por debajo del gateway. Las cuotas existen para
// CONTENER ACCIDENTES (un cliente en bucle que abre mil conexiones y ahoga la
// memoria del anfitrión) y para REPARTIR con justicia, no para aislar. Quien
// necesite aislamiento fuerte necesita daemons separados, no cuotas.
package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
)

// errTenantInstances lo devuelve ensure() cuando despertar/crear una instancia
// más excedería la cuota de instancias del tenant. handleProxy lo traduce a un
// 429 en vez del 502 genérico: no es que el servicio falle, es que este tenant
// ya tiene todas las que le tocan.
var errTenantInstances = errors.New("tenant instance quota exceeded")

// TenantLimit describe un token con nombre y sus cuotas. main.go lo rellena
// desde la configuración; vive aquí, en tipos propios, para no acoplar el
// paquete gateway al de config.
type TenantLimit struct {
	Name         string
	Token        string
	MaxInstances int // instancias despiertas atribuibles; 0 = sin límite
	MaxInflight  int // peticiones en vuelo simultáneas; 0 = sin límite
}

// tenant es un token ya resuelto con sus cuotas.
type tenant struct {
	name         string
	maxInstances int // 0 = sin límite
	maxInflight  int // 0 = sin límite
}

// defaultTenant es a quien se atribuye el token único de siempre (el sin nombre)
// y todo el tráfico cuando la autenticación está desactivada. Sin límites.
const defaultTenant = "default"

type tenantKey struct{}

func withTenant(ctx context.Context, t *tenant) context.Context {
	return context.WithValue(ctx, tenantKey{}, t)
}

// tenantFrom recupera el tenant de la petición. NUNCA devuelve nil: sin auth
// (token vacío) o sin tenant resuelto, se cae al "default" sin límites, que es
// justo el comportamiento de siempre.
func tenantFrom(ctx context.Context) *tenant {
	if t, ok := ctx.Value(tenantKey{}).(*tenant); ok && t != nil {
		return t
	}
	return &tenant{name: defaultTenant}
}

// SetTenants registra los tokens con nombre y sus cuotas. Debe llamarse ANTES de
// Handler(). Lista vacía = solo el token único (comportamiento de siempre).
func (g *Gateway) SetTenants(ts []TenantLimit) {
	g.extraTenants = ts
}

// authHandler protege el mux resolviendo el Bearer a un tenant y colgándolo del
// contexto de la petición.
//
// El token único (el de siempre) es el tenant "default" sin límites; los tokens
// con nombre traen sus cuotas. Un token vacío y sin tokens con nombre desactiva
// la autenticación: es el `-no-auth` de siempre, y que sea seguro depende de que
// se escuche en loopback (ver resolveGatewayToken).
func (g *Gateway) authHandler(h http.Handler, token string) http.Handler {
	type known struct {
		secret string
		t      *tenant
	}
	var reg []known
	if token != "" {
		reg = append(reg, known{token, &tenant{name: defaultTenant}})
	}
	for _, tl := range g.extraTenants {
		// Un token con nombre sin secreto no protege nada: se ignora en vez de
		// registrar una entrada que aceptaría la cadena vacía.
		if tl.Token == "" {
			continue
		}
		reg = append(reg, known{tl.Token, &tenant{
			name:         tl.Name,
			maxInstances: tl.MaxInstances,
			maxInflight:  tl.MaxInflight,
		}})
	}

	// Sin ningún token: autenticación desactivada, handler sin envolver.
	if len(reg) == 0 {
		return h
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /healthz queda abierto: systemd y cualquier monitor tienen que poder
		// preguntar si el proceso vive sin que le repartamos ningún token.
		if r.URL.Path == "/healthz" {
			h.ServeHTTP(w, r)
			return
		}

		got, ok := bearer(r.Header.Get("Authorization"))
		if !ok {
			authFail(w)
			return
		}
		// Se comparan TODOS los tokens sin cortar al primer acierto: así el tiempo
		// no delata cuál coincidió ni cuánto prefijo se acertó. ConstantTimeCompare
		// ya devuelve 0 si las longitudes difieren.
		var matched *tenant
		for _, k := range reg {
			if subtle.ConstantTimeCompare([]byte(got), []byte(k.secret)) == 1 {
				matched = k.t
			}
		}
		if matched == nil {
			authFail(w)
			return
		}
		h.ServeHTTP(w, r.WithContext(withTenant(r.Context(), matched)))
	})
}

// authFail responde el 401 con la pista de cómo autenticarse.
func authFail(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="kindling"`)
	http.Error(w, "missing Authorization header: Bearer <token>, or the token is invalid.\n"+
		"The token lives in gateway.token on the host where the gateway runs.", http.StatusUnauthorized)
}

// tenantBegin contabiliza una petición en vuelo para el tenant y comprueba su
// cuota. Devuelve false si la excedería: el llamador responde 429. Con
// maxInflight 0 (el default) nunca rechaza, pero cuenta igual para que los
// contadores estén listos para exponerse.
func (g *Gateway) tenantBegin(t *tenant) bool {
	g.tenantMu.Lock()
	defer g.tenantMu.Unlock()
	if g.tenantInflight == nil {
		g.tenantInflight = map[string]int{}
	}
	if t.maxInflight > 0 && g.tenantInflight[t.name] >= t.maxInflight {
		return false
	}
	g.tenantInflight[t.name]++
	return true
}

// tenantEnd libera una petición en vuelo. No baja de cero: un contador negativo
// abriría cuota de más al siguiente.
func (g *Gateway) tenantEnd(t *tenant) {
	g.tenantMu.Lock()
	if g.tenantInflight != nil && g.tenantInflight[t.name] > 0 {
		g.tenantInflight[t.name]--
	}
	g.tenantMu.Unlock()
}

// tenantInstances cuenta las instancias despiertas atribuidas a un tenant. Se
// deriva del mapa de servicios en vez de llevar un contador aparte: así nunca se
// desincroniza cuando el segador o evictLRU retiran una instancia. Se llama con
// g.mu tomado.
func (g *Gateway) tenantInstances(name string) int {
	n := 0
	for _, e := range g.services {
		if e.tenant == name {
			n++
		}
	}
	// También las réplicas de scale-out cuentan como instancias del tenant.
	for _, es := range g.extra {
		for _, e := range es {
			if e.tenant == name {
				n++
			}
		}
	}
	return n
}

// TenantInflight devuelve una foto de las peticiones en vuelo por tenant.
//
// TODO(observabilidad): el gateway no tiene hoy superficie /metrics propia (las
// métricas viven en el daemon, ver internal/daemon/metrics.go). Cuando la tenga,
// esto y tenantInstances() son los contadores a exponer como series
// kling_gateway_tenant_inflight / _instances. De momento quedan listos y
// consultables desde código/tests.
func (g *Gateway) TenantInflight() map[string]int {
	g.tenantMu.Lock()
	defer g.tenantMu.Unlock()
	out := make(map[string]int, len(g.tenantInflight))
	for k, v := range g.tenantInflight {
		out[k] = v
	}
	return out
}
