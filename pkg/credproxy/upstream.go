package credproxy

// Upstream fijado por el operador para una credencial Postgres: la base de
// datos de un Docker del mismo host (publicada en 127.0.0.1) o de la LAN/VPC,
// que dialPublico no alcanza a propósito.
//
// QUIÉN LO FIJA: solo el operador, con la CLI o la API del host (-upstream).
// El invitado no lo ve ni lo elige: sigue conectando al nombre de la
// credencial (Domain), que su resolver desvía al proxy, y el proxy marca la
// dirección fijada. Por eso aquí SÍ se permiten IPs privadas y el loopback:
// el operador sabe dónde está su base de datos, y un DNS hostil no interviene
// en la elección del destino (como mucho en la resolución de un nombre fijado,
// y para eso está la barrera de abajo).
//
// LO QUE NO SE MARCA NUNCA, ni fijado: los metadatos del cloud y todo el
// link-local (169.254.0.0/16, fe80::/10, fd00:ec2::254), "esta red"
// (0.0.0.0/8, ::), multicast y broadcast, y los rangos internos de kindling
// (el enlace del invitado 172.16.0.0/30 y los veth del host 172.30.0.0/16):
// ahí están el propio proxy y los invitados de otras máquinas. Un nombre se
// resuelve al marcar y basta UNA IP prohibida entre sus respuestas para no
// marcar ninguna: un nombre que apunta a los metadatos no es de fiar a medias.
// "localhost" no se resuelve: es el loopback (RFC 6761).
//
// NI LOS REENVÍOS DE KINDLING: en macOS cada kling-vz expone los puertos de su
// invitado en 127.0.0.1 (api.Machine.Forwards). Un upstream en el loopback
// que diera con el reenvío de OTRA máquina llevaría la conexión (y, sin TLS,
// el intercambio SCRAM) al invitado de otro. Los reenvíos se abren SOLO en un
// rango de puertos reservado (ForwardPortMin-ForwardPortMax) y ningún
// upstream del loopback puede apuntar a ese rango: ni al validar ni al marcar.
// El rango es una constante y no una lista de reenvíos vivos a propósito:
// quien marca (el kling-vz de cada máquina) no conoce los reenvíos de las
// demás, y una lista consultada al validar se queda vieja en cuanto otra
// máquina arranca (TOCTOU). Con el rango, lo que se comprueba al marcar es
// cierto siempre. El precio: un Postgres de Docker publicado en ese rango del
// loopback no vale como upstream (se publica en otro puerto).
//
// Por qué un rango y no otra IP de loopback (127.0.0.2): macOS solo configura
// 127.0.0.1 en lo0, y dar de alta otra exige root (ifconfig lo0 alias) en cada
// arranque del Mac; el daemon y kling-vz corren sin privilegios.
//
// En Linux no hay reenvíos al loopback: al invitado se llega por la IP del
// veth (172.30.0.0/16, prohibida arriba), los DNAT de cada netns solo se
// aplican al tráfico que entra en ESE netns (PREROUTING), no al que el daemon
// origina en el host, y la API del daemon es un socket Unix. Lo único de
// kindling que puede escuchar en el loopback del host es opcional y habla
// HTTP con token (el gateway MCP o `kling ai up`, 127.0.0.1:8080 por defecto);
// el proxy no le da al invitado ni un byte de esa conexión hasta que el
// servidor completa SCRAM-SHA-256 (sin TLS) o presenta un certificado válido
// (verify-full), cosa que un servicio HTTP no hace. El rango reservado se
// aplica igual en Linux: la regla es la misma en los dos sistemas.
//
// DESDE DÓNDE SE MARCA: en Linux el proxy es una goroutine del daemon, en el
// netns del host (escucha en el lado host del veth, no dentro del netns de la
// máquina): 127.0.0.1 es el loopback del host. En macOS lo sirve kling-vz con
// la pila de red del Mac (no la de gVisor del invitado): 127.0.0.1 es el
// loopback del Mac, donde Docker Desktop publica los puertos. kling-vz corre
// confinado (kling-vz.sb) y desde ahí el resolver del Mac no contesta: en
// macOS el daemon resuelve el nombre al entregar la credencial y le pasa la
// IP (ResolverUpstream), que kling-vz vuelve a comprobar al marcar.
//
// UpstreamTLS "disable" apaga el TLS del tramo del servidor, solo con
// Upstream: la contraseña no cruza la red porque solo se admite SCRAM-SHA-256
// (sin -PLUS, que necesita TLS), que prueba que se conoce la clave sin
// mandarla y exige además que el servidor pruebe que la conoce; ni contraseña
// en claro ni md5 ni trust. Lo que sí viaja en claro son las consultas y sus
// resultados: para un Docker en el mismo host no sale de la máquina; para una
// base de datos de la LAN, la CLI lo advierte.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// UpstreamTLSVerifyFull es el valor por defecto de UpstreamTLS (se guarda
	// como ""): TLS verificado contra TLSServerName o Domain.
	UpstreamTLSVerifyFull = "verify-full"
	// UpstreamTLSDisable: sin TLS hacia el servidor, solo con Upstream y
	// SCRAM-SHA-256.
	UpstreamTLSDisable = "disable"
	// CapPostgresUpstream es la capacidad que kling-vz anuncia en
	// credential_kinds cuando entiende Upstream, UpstreamTLS y TLSServerName.
	// No es un Kind de credencial.
	CapPostgresUpstream = "postgres-upstream"
	// CapHTTPPlaces es la capacidad que kling-vz anuncia cuando cambia el
	// marcador solo donde dicen Headers, Query y Body. Uno anterior lo
	// cambiaría también en el cuerpo y la query de toda petición.
	CapHTTPPlaces = "http-places"

	// ForwardPortMin y ForwardPortMax delimitan el rango de puertos del
	// loopback reservado a los reenvíos de kindling (kling-vz, macOS). Por
	// debajo del rango efímero de macOS (49152+) y de Linux (32768+), para que
	// un puerto que el sistema da al azar no caiga dentro. Ver la cabecera.
	ForwardPortMin = 29000
	ForwardPortMax = 29999
)

// PuertoReservado dice si port es del rango de reenvíos de kindling.
func PuertoReservado(port int) bool {
	return port >= ForwardPortMin && port <= ForwardPortMax
}

// errUpstreamReenvio: un upstream del loopback apunta al rango de reenvíos.
func errUpstreamReenvio(u string) error {
	return fmt.Errorf("%w: %s is in kindling's reserved forward range (loopback ports %d-%d, where other machines' guests are exposed); publish the database on another port",
		errUpstreamProhibido, u, ForwardPortMin, ForwardPortMax)
}

// destinoReenvio dice si marcar ip:port podría llegar al reenvío de un
// invitado de kindling: loopback y puerto del rango reservado.
func destinoReenvio(ip netip.Addr, port int) bool {
	return ip.Unmap().IsLoopback() && PuertoReservado(port)
}

// upstreamProhibido son los destinos a los que el proxy no marca nunca, ni con
// un upstream fijado por el operador. Un test de internal/net comprueba que
// incluye los rangos internos de kindling.
var upstreamProhibido = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),           // "esta red"
	netip.MustParsePrefix("169.254.0.0/16"),      // link-local y metadatos de cloud
	netip.MustParsePrefix("224.0.0.0/4"),         // multicast
	netip.MustParsePrefix("240.0.0.0/4"),         // reservada, y 255.255.255.255
	netip.MustParsePrefix("172.16.0.0/30"),       // el enlace del invitado (kindling)
	netip.MustParsePrefix("172.30.0.0/16"),       // los veth del host (kindling, Linux)
	netip.MustParsePrefix("::/128"),              // sin especificar
	netip.MustParsePrefix("fe80::/10"),           // link-local v6
	netip.MustParsePrefix("ff00::/8"),            // multicast v6
	netip.MustParsePrefix("fd00:ec2::254/128"),   // metadatos de AWS por IPv6
	netip.MustParsePrefix("64:ff9b::a9fe:0/112"), // NAT64 de 169.254.0.0/16
}

// UpstreamProhibidos devuelve los rangos a los que un upstream fijado no puede
// apuntar, en texto.
func UpstreamProhibidos() []string {
	out := make([]string, len(upstreamProhibido))
	for i, p := range upstreamProhibido {
		out[i] = p.String()
	}
	return out
}

// UpstreamIPProhibida dice si ip cae en algún rango de upstreamProhibido. Una
// IPv4 escrita como IPv6 (::ffff:a.b.c.d) se mira como la IPv4 que es.
func UpstreamIPProhibida(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	for _, p := range upstreamProhibido {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// reNombreHost es un nombre de host para Upstream y TLSServerName: etiquetas
// DNS, sin exigir dominio de primer nivel ("localhost", "db", "postgres.lan").
var reNombreHost = regexp.MustCompile(`^([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?)(\.[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?)*$`)

// validarNombreHost acepta un nombre DNS (hasta 253 bytes) o una IP.
func validarNombreHost(h string) error {
	if _, err := netip.ParseAddr(h); err == nil {
		return nil
	}
	if len(h) == 0 || len(h) > 253 || !reNombreHost.MatchString(h) {
		return fmt.Errorf("%q is not a host name or IP", h)
	}
	return nil
}

// normalizarUpstream valida "host:puerto" y lo devuelve normalizado (host en
// minúsculas, IPv6 entre corchetes). Una IP literal prohibida es un error
// ya aquí; un nombre se comprueba además al marcar.
func normalizarUpstream(u string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(u))
	if err != nil {
		return "", fmt.Errorf("upstream %q must be host:port", u)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", fmt.Errorf("upstream %q: port must be 1-65535", u)
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return "", fmt.Errorf("upstream %q: no IPv6 zones", u)
		}
		if UpstreamIPProhibida(ip) {
			return "", fmt.Errorf("upstream %q: %s is a forbidden destination (metadata, link-local, multicast or kindling's own network)", u, ip)
		}
		if destinoReenvio(ip, n) {
			return "", errUpstreamReenvio(u)
		}
		return net.JoinHostPort(ip.String(), port), nil
	}
	if err := validarNombreHost(host); err != nil {
		return "", fmt.Errorf("upstream: %w", err)
	}
	if host == "localhost" && PuertoReservado(n) {
		return "", errUpstreamReenvio(u)
	}
	return net.JoinHostPort(host, port), nil
}

// UpstreamLoopback dice si un upstream ("host:puerto") es el loopback del
// host: una IP de loopback o "localhost". La CLI no advierte del tráfico sin
// cifrar en ese caso. Un nombre cualquiera no cuenta: no se resuelve aquí.
func UpstreamLoopback(u string) bool {
	host, _, err := net.SplitHostPort(u)
	if err != nil {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// UpstreamPuertoLoopback devuelve el puerto de u ("host:puerto") si u es el
// loopback del host (ver UpstreamLoopback). El daemon lo cruza con los
// reenvíos vivos de sus máquinas al validar (el rango reservado lo cubre ya;
// esto cubre además un kling-vz anterior que abriera puertos fuera de él).
func UpstreamPuertoLoopback(u string) (int, bool) {
	if !UpstreamLoopback(u) {
		return 0, false
	}
	_, port, _ := net.SplitHostPort(u)
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0, false
	}
	return n, true
}

// UpstreamNecesitaDNS dice si marcar el upstream u ("host:puerto") exige
// resolver un nombre: no es una IP ni "localhost". En macOS el daemon los
// resuelve él (ResolverUpstream): kling-vz corre confinado (kling-vz.sb) y
// desde ahí el resolver del Mac no contesta.
func UpstreamNecesitaDNS(u string) bool {
	host, _, err := net.SplitHostPort(u)
	if err != nil || u == "" {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" {
		return false
	}
	_, err = netip.ParseAddr(host)
	return err != nil
}

// validarUpstream comprueba Upstream, UpstreamTLS y TLSServerName de una
// credencial Postgres y los normaliza.
func validarUpstream(c *Credential) error {
	d := c.Domain
	if c.UpstreamMachine != "" || c.UpstreamOwner != "" {
		return validarUpstreamMaquina(c)
	}
	if c.Upstream != "" {
		u, err := normalizarUpstream(c.Upstream)
		if err != nil {
			return fmt.Errorf("credential for %s: %w", d, err)
		}
		c.Upstream = u
	}
	switch strings.ToLower(c.UpstreamTLS) {
	case "", UpstreamTLSVerifyFull:
		c.UpstreamTLS = ""
	case UpstreamTLSDisable:
		c.UpstreamTLS = UpstreamTLSDisable
		if c.Upstream == "" {
			return fmt.Errorf("credential for %s: -upstream-tls disable needs -upstream (an address the operator pins)", d)
		}
		if c.TLSServerName != "" || c.CAPEM != "" {
			return fmt.Errorf("credential for %s: -tls-server-name and -ca-file make no sense with -upstream-tls disable", d)
		}
	default:
		return fmt.Errorf("credential for %s: upstream TLS %q must be verify-full or disable", d, c.UpstreamTLS)
	}
	if c.TLSServerName != "" {
		n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(c.TLSServerName), "."))
		if err := validarNombreHost(n); err != nil {
			return fmt.Errorf("credential for %s: tls server name: %w", d, err)
		}
		c.TLSServerName = n
	}
	return nil
}

// destinoPG es la dirección que se marca para cred: el upstream fijado o
// Domain:Port.
func (c Credential) destinoPG() string {
	if c.UpstreamMachine != "" {
		return "machine " + c.UpstreamMachine
	}
	if c.Upstream != "" {
		return c.Upstream
	}
	return net.JoinHostPort(c.Domain, strconv.Itoa(c.Port))
}

// lookupUpstream resuelve el nombre de un upstream fijado. Es el resolver del
// sistema: el operador puede fijar un nombre de su LAN o de /etc/hosts.
type lookupUpstream func(ctx context.Context, host string) ([]netip.Addr, error)

func lookupSistema(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// errUpstreamProhibido: el upstream (o una de las IPs de su nombre) cae en
// upstreamProhibido.
var errUpstreamProhibido = errors.New("forbidden upstream destination")

// ipsFijado son las IPs a las que se puede marcar para el host de un upstream
// fijado: la IP literal, el loopback para "localhost" o lo que resuelva el
// nombre. Basta UNA IP prohibida (o del rango de reenvíos, con el puerto pn)
// para no devolver ninguna.
func ipsFijado(ctx context.Context, lookup lookupUpstream, host string, pn int) ([]netip.Addr, error) {
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if host == "localhost" {
		// RFC 6761: localhost es el loopback, sin preguntar a nadie. Así
		// funciona también en un kling-vz confinado, que no llega al
		// resolver del Mac (ver UpstreamNecesitaDNS).
		ips = []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1}), netip.IPv6Loopback()}
	} else {
		if lookup == nil {
			lookup = lookupSistema
		}
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ips, err = lookup(lctx, host)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("resolving upstream %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("upstream %s does not resolve", host)
		}
	}
	port := strconv.Itoa(pn)
	for _, ip := range ips {
		if UpstreamIPProhibida(ip) {
			return nil, fmt.Errorf("%w: %s resolves to %s", errUpstreamProhibido, host, ip.Unmap())
		}
		// Otra vez aquí, y no solo al validar: un nombre de la LAN puede
		// resolver al loopback, y es aquí donde se decide a dónde se va.
		if destinoReenvio(ip, pn) {
			return nil, errUpstreamReenvio(net.JoinHostPort(ip.Unmap().String(), port))
		}
	}
	return ips, nil
}

// ResolverUpstream fija a una IP el upstream u ("host:puerto") si es un
// nombre, con las mismas comprobaciones que al marcar (ipsFijado): ninguna de
// sus IPs puede estar prohibida ni caer en el rango de reenvíos. Devuelve
// "ip:puerto" con la primera IP que dio el resolver; una IP o "localhost" se
// devuelven tal cual. lookup nil es el resolver del sistema.
//
// Es para macOS: kling-vz corre confinado y no llega al resolver del Mac, así
// que el daemon resuelve el nombre al entregarle la credencial y le pasa la
// IP, que kling-vz vuelve a comprobar al marcar. El TLS no cambia: se verifica
// contra TLSServerName o Domain, nunca contra el host del upstream.
func ResolverUpstream(ctx context.Context, lookup func(ctx context.Context, host string) ([]netip.Addr, error), u string) (string, error) {
	if !UpstreamNecesitaDNS(u) {
		return u, nil
	}
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		return "", err
	}
	pn, err := strconv.Atoi(port)
	if err != nil {
		return "", fmt.Errorf("upstream %q: bad port", u)
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	ips, err := ipsFijado(ctx, lookup, host, pn)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(ips[0].Unmap().String(), port), nil
}

// dialFijado marca un upstream fijado por el operador: resuelve el nombre (si
// lo es), se niega si alguna IP está prohibida y prueba las demás en orden.
func dialFijado(lookup lookupUpstream, d *net.Dialer) func(ctx context.Context, addr string) (net.Conn, error) {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		pn, err := strconv.Atoi(port)
		if err != nil {
			return nil, fmt.Errorf("upstream %q: bad port", addr)
		}
		ips, err := ipsFijado(ctx, lookup, host, pn)
		if err != nil {
			return nil, err
		}
		var last error
		for _, ip := range ips {
			c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
}
