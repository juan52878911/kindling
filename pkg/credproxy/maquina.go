package credproxy

// Upstream que es OTRA MÁQUINA de kindling: el modelo A de `kling db`, una
// copia de base de datos compartida por agentes que viven en otras microVMs.
// El agente conecta al proxy de su máquina con su marcador, como con cualquier
// credencial Postgres, y el proxy marca a la copia con la contraseña que el
// agente no ve.
//
// LO QUE NO SE HACE: fijar la dirección de la copia al entregar la
// credencial. Una dirección (la IP del netns en Linux) se reutiliza: si la
// copia se para, se congela o se borra, su índice de red pasa a otra máquina,
// y una credencial con la dirección de ayer llevaría la contraseña (y el SQL
// del agente) al invitado de otro. Es un TOCTOU entre "cuando se dio" y
// "cuando se usa".
//
// LO QUE SE HACE: la credencial guarda el ID de la máquina (UpstreamMachine,
// que nunca se reutiliza) y el dueño que la entrega declaró (UpstreamOwner).
// En CADA conexión (y en cada CancelRequest) el proxy pide la dirección a
// Options.ResolveMachine, que el daemon implementa bajo el candado de su
// manager: la máquina con ese ID exacto existe, corre, está lista para `kling
// db` y es del mismo dueño que el agente y que la credencial. Si algo no
// cuadra, error y no se marca nada. La dirección resuelta es la ÚNICA
// excepción a upstreamProhibido (la red interna de kindling), y no viene
// nunca de fuera: ni la API ni la CLI aceptan una dirección para esto, solo
// un ID de máquina, y aun así destinoMaquinaValido comprueba que lo resuelto
// es de verdad un destino de kindling y no otra cosa.
//
// CORTAR LO QUE YA ESTÁ: resolver en cada dial cubre las conexiones nuevas;
// las vivas se cortan con Invalidar(id), que el daemon llama al congelar,
// pausar, parar, borrar o reetiquetar la copia (sin eso, una sesión abierta
// antes de un freeze seguiría "viva" contra un invitado dormido, y la del
// agente tras un detach seguiría hablando con la base). SetCredentials corta
// también las sesiones cuya credencial desaparece o cambia de máquina.
//
// SIN TLS Y CON SCRAM: el tramo del proxy a la copia es el veth del propio
// host (Linux). UpstreamTLS "disable" es obligatorio y con él solo se admite
// SCRAM-SHA-256: la copia tiene que probar que conoce la clave (ver
// autenticarPG), así que si, pese a todo, la dirección llevara a otra
// máquina, esa otra no podría fingir ser la copia ni aprender la contraseña.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/juan52878911/kindling/pkg/lazyre"
)

// ResolveMachineFunc da la dirección ("ip:puerto") por la que el proxy llega
// al puerto port de la máquina id, si en este momento la puede usar una
// credencial declarada para el dueño owner. Lo implementa el daemon; un error
// es "no marques".
type ResolveMachineFunc func(id, owner string, port int) (addr string, err error)

// DialMachineFunc da la conexión ya abierta al puerto port de la máquina id
// para una credencial del dueño owner (Options.DialMachine). Un error es "no
// marques".
type DialMachineFunc func(ctx context.Context, id, owner string, port int) (net.Conn, error)

// CapGraphLink es la capacidad que kling-vz anuncia en credential_kinds cuando
// puede pedir al daemon conexiones a otras máquinas (aristas link y
// credential de un grafo, kling db attach): ver pkg/linkbroker. No es un Kind
// de credencial.
const CapGraphLink = "graph-link"

// ReasonMachineUnavailable: la máquina de UpstreamMachine no se puede usar
// ahora (parada, congelada, borrada, de otro dueño o no lista).
const ReasonMachineUnavailable = "machine_unavailable"

var (
	// reIDMaquina es un ID de máquina de kindling: hexadecimal en minúsculas.
	// Ni puntos ni dos puntos: una IP o un host:puerto no casan nunca.
	reIDMaquina = lazyre.New(`^[0-9a-f]{16,64}$`)
	// reDueño es api.KeyPattern (el valor de kling.db.owner). Copiado: este
	// paquete solo depende de la biblioteca estándar.
	reDueño = lazyre.New(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// errMaquinaNoDisponible marca los fallos de resolución de UpstreamMachine.
var errMaquinaNoDisponible = errors.New("upstream machine unavailable")

// ValidarIDMaquina dice si id tiene forma de ID de máquina (no de dirección).
func ValidarIDMaquina(id string) error {
	if !reIDMaquina.MatchString(id) {
		return fmt.Errorf("upstream machine %q must be a machine id (lowercase hex), never an address", id)
	}
	return nil
}

// validarUpstreamMaquina: ver validarUpstream.
func validarUpstreamMaquina(c *Credential) error {
	d := c.Domain
	if c.Upstream != "" {
		return fmt.Errorf("credential for %s: upstream machine and -upstream exclude each other", d)
	}
	if err := ValidarIDMaquina(c.UpstreamMachine); err != nil {
		return fmt.Errorf("credential for %s: %w", d, err)
	}
	if !reDueño.MatchString(c.UpstreamOwner) {
		return fmt.Errorf("credential for %s: an upstream machine needs its owner (lowercase letters, digits, '.', '_' and '-'), got %q", d, c.UpstreamOwner)
	}
	if !strings.EqualFold(c.UpstreamTLS, UpstreamTLSDisable) {
		return fmt.Errorf("credential for %s: an upstream machine needs upstream TLS %q (SCRAM-SHA-256 over the host's own link)", d, UpstreamTLSDisable)
	}
	c.UpstreamTLS = UpstreamTLSDisable
	if c.TLSServerName != "" || c.CAPEM != "" {
		return fmt.Errorf("credential for %s: -tls-server-name and -ca-file make no sense with an upstream machine", d)
	}
	if c.AnyDatabase || c.Database == "" {
		return fmt.Errorf("credential for %s: an upstream machine needs -database (not -any-database)", d)
	}
	return nil
}

// upstreamAuditado es lo que el registro dice del destino: la dirección
// fijada, o "machine:<id>" (nunca la dirección resuelta, que cambia).
func (c Credential) upstreamAuditado() string {
	if c.UpstreamMachine != "" {
		return "machine:" + c.UpstreamMachine
	}
	return c.Upstream
}

// rangoVethKindling es donde viven las IPs de los netns de las máquinas en
// Linux (internal/net: hostPrefix). Es de upstreamProhibido: aquí se admite
// solo como dirección resuelta por el daemon para UpstreamMachine.
var rangoVethKindling = netip.MustParsePrefix("172.30.0.0/16")

// DestinoMaquinaValido es destinoMaquinaValido para quien resuelve fuera del
// proxy (el daemon, al marcar él mismo para kling-vz: ver pkg/linkbroker).
func DestinoMaquinaValido(ap netip.AddrPort) error { return destinoMaquinaValido(ap) }

// destinoMaquinaValido comprueba que lo que devolvió ResolveMachine es un
// destino de kindling y nada más: la IP de un netns (Linux) o un reenvío del
// rango reservado en el loopback (macOS). Todo lo demás, también lo que un
// upstream fijado sí podría ser (la LAN, el loopback fuera del rango), se
// rechaza: la excepción es la red interna de kindling, no una puerta a otra.
func destinoMaquinaValido(ap netip.AddrPort) error {
	ip := ap.Addr().Unmap()
	switch {
	case !ap.IsValid() || ap.Port() == 0:
		return errors.New("not an ip:port")
	case ip.Zone() != "":
		return errors.New("no IPv6 zones")
	case rangoVethKindling.Contains(ip):
		return nil
	case destinoReenvio(ip, int(ap.Port())):
		return nil
	}
	return fmt.Errorf("%s is not a kindling machine address", ap)
}

// dialMaquina resuelve y marca la máquina de cred. La dirección se pide ahora,
// no antes: ver la cabecera.
func (p *Proxy) dialMaquina(ctx context.Context, cred credPG) (net.Conn, error) {
	if p.abrirMaq != nil {
		c, err := p.abrirMaq(ctx, cred.UpstreamMachine, cred.UpstreamOwner, cred.Port)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errMaquinaNoDisponible, err)
		}
		return c, nil
	}
	if p.resolveMaq == nil {
		return nil, fmt.Errorf("%w: this proxy can't resolve kindling machines", errMaquinaNoDisponible)
	}
	addr, err := p.resolveMaq(cred.UpstreamMachine, cred.UpstreamOwner, cred.Port)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errMaquinaNoDisponible, err)
	}
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: machine %s resolved to %q, not ip:port", errUpstreamProhibido, cred.UpstreamMachine, addr)
	}
	if err := p.destinoMaq(ap); err != nil {
		return nil, fmt.Errorf("%w: machine %s: %v", errUpstreamProhibido, cred.UpstreamMachine, err)
	}
	return p.dialMaq(ctx, "tcp", ap.String())
}

// sesionMaquina es lo que el registro de sesiones sabe de una: a qué máquina
// va y con qué marcador entró.
type sesionMaquina struct {
	maquina, marcador string
}

// registrarSesion apunta s como sesión hacia la máquina de cred, para que
// Invalidar y SetCredentials la puedan cortar. Se llama ANTES de resolver: así
// una invalidación que llegue entre la resolución y el dial encuentra la
// sesión (y ponerUp se niega). Devuelve false si la credencial ya no está en
// el juego vigente (la quitaron entre elegirla y aquí). quitar la borra.
func (p *Proxy) registrarSesion(s *sesionPG, cred credPG) (quitar func(), ok bool) {
	p.sesMu.Lock()
	defer p.sesMu.Unlock()
	p.mu.RLock()
	vigente := false
	for _, c := range p.pg {
		if c.Placeholder == cred.Placeholder && c.UpstreamMachine == cred.UpstreamMachine {
			vigente = true
			break
		}
	}
	p.mu.RUnlock()
	if !vigente {
		return func() {}, false
	}
	p.sesiones[s] = sesionMaquina{maquina: cred.UpstreamMachine, marcador: cred.Placeholder}
	return func() {
		p.sesMu.Lock()
		delete(p.sesiones, s)
		p.sesMu.Unlock()
	}, true
}

// SoloMaquinas dice si el proxy tiene credenciales y TODAS van a otra
// máquina de kindling (UpstreamMachine): nada de lo que sirve sale a
// internet. kling-vz lo usa para atender las aristas credential de un nodo
// sin egress allowlist.
func (p *Proxy) SoloMaquinas() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.pg) == 0 || len(p.creds) > 0 {
		return false
	}
	for _, c := range p.pg {
		if c.UpstreamMachine == "" {
			return false
		}
	}
	return true
}

// Invalidar corta todas las sesiones vivas hacia la máquina id (las dos
// mitades: la del invitado y la de la copia) y devuelve cuántas. Con id "" las
// corta todas, vayan a la máquina que vayan: el agente mismo cambió (de dueño,
// por ejemplo). Las conexiones nuevas no las para esto sino la resolución en
// cada dial.
func (p *Proxy) Invalidar(id string) int {
	p.sesMu.Lock()
	defer p.sesMu.Unlock()
	n := 0
	for s, sm := range p.sesiones {
		if id == "" || sm.maquina == id {
			s.cerrar()
			delete(p.sesiones, s)
			n++
		}
	}
	return n
}

// cortarSesionesHuerfanasLocked corta las sesiones hacia una máquina cuya
// credencial (marcador y máquina) ya no está en pg. Con p.sesMu tomado.
func (p *Proxy) cortarSesionesHuerfanasLocked(pg []credPG) {
	vigentes := make(map[sesionMaquina]bool, len(pg))
	for _, c := range pg {
		if c.UpstreamMachine != "" {
			vigentes[sesionMaquina{maquina: c.UpstreamMachine, marcador: c.Placeholder}] = true
		}
	}
	for s, sm := range p.sesiones {
		if !vigentes[sm] {
			s.cerrar()
			delete(p.sesiones, s)
		}
	}
}
