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
//
// DESDE DÓNDE SE MARCA: en Linux el proxy es una goroutine del daemon, en el
// netns del host (escucha en el lado host del veth, no dentro del netns de la
// máquina): 127.0.0.1 es el loopback del host. En macOS lo sirve kling-vz con
// la pila de red del Mac (no la de gVisor del invitado): 127.0.0.1 es el
// loopback del Mac, donde Docker Desktop publica los puertos.
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
)

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
		return net.JoinHostPort(ip.String(), port), nil
	}
	if err := validarNombreHost(host); err != nil {
		return "", fmt.Errorf("upstream: %w", err)
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

// validarUpstream comprueba Upstream, UpstreamTLS y TLSServerName de una
// credencial Postgres y los normaliza.
func validarUpstream(c *Credential) error {
	d := c.Domain
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

// dialFijado marca un upstream fijado por el operador: resuelve el nombre (si
// lo es), se niega si alguna IP está prohibida y prueba las demás en orden.
func dialFijado(lookup lookupUpstream, d *net.Dialer) func(ctx context.Context, addr string) (net.Conn, error) {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else {
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
		for _, ip := range ips {
			if UpstreamIPProhibida(ip) {
				return nil, fmt.Errorf("%w: %s resolves to %s", errUpstreamProhibido, host, ip.Unmap())
			}
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
