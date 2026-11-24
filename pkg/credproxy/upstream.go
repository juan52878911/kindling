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
//
// CREDENCIALES HTTP CON UPSTREAM: lo mismo para HTTP (la réplica de un modelo
// que llama al gateway de IA del host con un token de inquilino, o un servicio
// de la VPC). El invitado habla HTTP en claro con el nombre de la credencial,
// que su resolver desvía al proxy, y el proxy marca la dirección fijada con
// los mismos vetos de arriba (validar y marcar). Diferencias con Postgres:
//   - Allow es obligatorio. El proxy es un oráculo de la clave, y hacia un
//     servicio del host o de la LAN lo único que acota qué se hace con ella es
//     la lista de peticiones; con Postgres lo acotan el rol y la base.
//   - "disable" solo hacia el loopback del host (una IP de loopback o
//     "localhost", nunca un nombre que resuelva ahí): aquí no hay SCRAM, la
//     clave va en una cabecera y fuera del host cruzaría la red en claro.
//     Hacia la LAN o la VPC, TLS verificado contra TLSServerName o Domain, con
//     las raíces del sistema más CAPEM.
//   - En el loopback el razonamiento de Postgres («un servicio HTTP no
//     completa SCRAM») no aplica: lo que protege el resto del loopback es el
//     rango de reenvíos (vetado) y que Allow solo deja pasar las peticiones
//     que el operador escribió.
//   - Todas las credenciales HTTP de un dominio van al mismo sitio
//     (ValidarDestinosHTTP): el destino es del dominio, no de cada clave.
//   - Al invitado no le llega la dirección fijada, ni en un error.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/lazyre"
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
var reNombreHost = lazyre.New(`^([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?)(\.[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?)*$`)

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
// Una IP del loopback va por loop si no es nil (Options.DialLoopback).
func dialFijado(lookup lookupUpstream, d *net.Dialer, loop func(context.Context, int) (net.Conn, error)) func(ctx context.Context, addr string) (net.Conn, error) {
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
			if loop != nil && ip.Unmap().IsLoopback() {
				// Todas las IP del loopback son la misma petición: el daemon
				// marca la dirección de la credencial.
				return loop(ctx, pn)
			}
			c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
}

// validarUpstreamHTTP comprueba y normaliza Upstream, UpstreamTLS,
// TLSServerName y CAPEM de una credencial HTTP (ver la cabecera). Sin
// Upstream ninguno vale: la credencial sale al dominio como siempre.
func validarUpstreamHTTP(c *Credential) error {
	d := c.Domain
	if c.Upstream == "" {
		if c.UpstreamTLS != "" || c.TLSServerName != "" || c.CAPEM != "" {
			return fmt.Errorf("credential for %s: -upstream-tls, -tls-server-name and -ca-file on an HTTP credential need -upstream", d)
		}
		return nil
	}
	if len(c.Allow) == 0 {
		return fmt.Errorf("credential for %s: an HTTP credential with -upstream needs -allow-request (the only requests that may carry the key there)", d)
	}
	if err := validarUpstream(c); err != nil {
		return err
	}
	if c.UpstreamTLS == UpstreamTLSDisable && !UpstreamLoopback(c.Upstream) {
		return fmt.Errorf("credential for %s: -upstream-tls disable on an HTTP credential is only for the host's loopback (an IP or localhost): the key would cross the network unencrypted to %s", d, c.Upstream)
	}
	if len(c.CAPEM) > MaxCAPEM {
		return fmt.Errorf("credential for %s: the CA is larger than %d bytes", d, MaxCAPEM)
	}
	if c.CAPEM != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(c.CAPEM)) {
		return fmt.Errorf("credential for %s: the CA is not a PEM certificate", d)
	}
	return nil
}

// sirveHTTP dice si c la atiende el proxy HTTP (ServeHTTP).
func sirveHTTP(c Credential) bool {
	return c.Kind == "" || c.Kind == KindHTTP || c.Kind == KindSigV4
}

// ValidarDestinosHTTP exige que las credenciales HTTP (y sigv4) de un mismo
// dominio vayan al mismo sitio: mismo Upstream, UpstreamTLS, TLSServerName y
// CAPEM. El proxy elige la salida por el Host, antes de saber qué credencial
// se usará. Los campos deben venir ya normalizados (ValidarTipo). Lo usa
// ValidarCredenciales y el manager con el juego fusionado de una plantilla.
func ValidarDestinosHTTP(creds []Credential) error {
	type destino struct{ up, tls, nombre, ca string }
	vistos := map[string]destino{}
	for _, c := range creds {
		if !sirveHTTP(c) {
			continue
		}
		d := destino{c.Upstream, c.UpstreamTLS, c.TLSServerName, c.CAPEM}
		if prev, ok := vistos[c.Domain]; ok && prev != d {
			return fmt.Errorf("credential for %s: every HTTP credential of a domain must have the same -upstream, -upstream-tls, -tls-server-name and -ca-file", c.Domain)
		}
		vistos[c.Domain] = d
	}
	return nil
}

// salidaUpstream es la salida de las credenciales HTTP de un dominio con
// Upstream: un cliente cuyo transporte marca SIEMPRE la dirección fijada (con
// dialFijado y sus vetos), con el esquema de su TLS.
type salidaUpstream struct {
	upstream string
	scheme   string
	tr       *http.Transport
	client   *http.Client
}

// nuevaSalidaUpstream arma la salida de c (validada). El TLS se verifica
// contra TLSServerName o Domain, nunca contra el host del upstream.
func (p *Proxy) nuevaSalidaUpstream(c Credential) (*salidaUpstream, error) {
	up := c.Upstream
	tr := &http.Transport{
		Proxy: nil,
		// Pida lo que pida el transporte (el Host es el dominio), se marca la
		// dirección fijada. p.dialUp se lee aquí: los tests cambian su resolver.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return p.dialUp(ctx, up)
		},
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: HeaderTimeout,
	}
	s := &salidaUpstream{upstream: up, scheme: "https", tr: tr}
	if c.UpstreamTLS == UpstreamTLSDisable {
		// Ya lo exige la validación; aquí otra vez porque es lo que decide
		// si la clave sale en claro.
		if !UpstreamLoopback(up) {
			return nil, fmt.Errorf("credential for %s: upstream without TLS outside the loopback", c.Domain)
		}
		s.scheme = "http"
	} else {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if c.CAPEM != "" && !pool.AppendCertsFromPEM([]byte(c.CAPEM)) {
			return nil, fmt.Errorf("credential for %s: the CA is not a PEM certificate", c.Domain)
		}
		nombre := c.Domain
		if c.TLSServerName != "" {
			nombre = c.TLSServerName
		}
		tr.TLSClientConfig = &tls.Config{ServerName: nombre, MinVersion: tls.VersionTLS12, RootCAs: pool}
		tr.ForceAttemptHTTP2 = true
	}
	s.client = &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return s, nil
}
