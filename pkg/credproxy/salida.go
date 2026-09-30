package credproxy

// La salida hacia el proveedor: resolver a IPv4 públicas y no conectar jamás a
// una privada. En Linux el daemon inyecta su propio resolver (el mismo que
// siembra el ipset); este fichero trae uno equivalente para quien no lo tenga
// (el backend vz) y la barrera de IPs que se aplica siempre.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DefaultDNS es el resolver público por el que PublicIPv4Lookup pregunta si no
// se le da otro. Es el mismo que internal/net impone al invitado en allowlist.
const DefaultDNS = "1.1.1.1"

// LookupFunc resuelve un host a IPv4 en texto. Una lista vacía es "no se
// puede": el proxy contesta 502 y no sale (falla CERRADO).
type LookupFunc func(ctx context.Context, host string) []string

// blocked son los destinos a los que el proxy no conecta nunca. Es copia de la
// lista de internal/net (firewall.go), que no se puede importar desde vz/; un
// test de internal/net comprueba que las dos siguen iguales.
var blocked = []string{
	"10.0.0.0/8",     // privada
	"172.16.0.0/12",  // privada
	"192.168.0.0/16", // privada: la LAN de casa vive aquí
	"169.254.0.0/16", // link-local y metadatos de cloud
	"127.0.0.0/8",    // loopback del host
	"100.64.0.0/10",  // CGNAT
}

// BlockedCIDRs devuelve una copia de los rangos que IsBlockedIP rechaza.
func BlockedCIDRs() []string { return append([]string(nil), blocked...) }

// IsBlockedIP dice si ip cae en alguno de los rangos a los que la clave no
// debe viajar nunca, por mucho que un DNS hostil los devuelva.
func IsBlockedIP(ip net.IP) bool {
	for _, cidr := range blocked {
		if _, n, err := net.ParseCIDR(cidr); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// PublicIPv4Lookup resuelve preguntando a dnsServer (puerto 53) y se queda
// solo con las IPv4 que no son de IsBlockedIP (ver LookupPublicIPv4).
func PublicIPv4Lookup(dnsServer string) LookupFunc {
	server := net.JoinHostPort(dnsServer, "53")
	return func(ctx context.Context, host string) []string {
		return LookupPublicIPv4(ctx, server, host)
	}
}

// Reintentos de DNS. El resolver de Go espera 5 s por intento; con un plazo
// total de 5 s, un solo datagrama perdido agotaba el plazo sin reintento y la
// petición salía con 502 a los 5000 ms (medido: 2 de 60 consultas a 1.1.1.1
// perdidas desde el lab). Tres intentos de 2 s cada uno.
var (
	dnsIntentos     = 3
	dnsPlazoIntento = 2 * time.Second
)

// LookupPublicIPv4 pregunta a server ("ip:puerto") por los A de host y se queda
// con las IPv4 que no son de IsBlockedIP. Solo pide A: el proxy y el ipset son
// IPv4. Reintenta si un intento no llega a respuesta; un "no existe" es
// definitivo. Vacío si no hay ninguna (falla CERRADO).
func LookupPublicIPv4(ctx context.Context, server, host string) []string {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, server)
		},
	}
	for i := 0; i < dnsIntentos && ctx.Err() == nil; i++ {
		ictx, cancel := context.WithTimeout(ctx, dnsPlazoIntento)
		addrs, err := r.LookupNetIP(ictx, "ip4", host)
		cancel()
		if err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				return nil
			}
			continue
		}
		var out []string
		for _, a := range addrs {
			ip := net.IP(a.Unmap().AsSlice())
			if ip.To4() != nil && !IsBlockedIP(ip) {
				out = append(out, ip.String())
			}
		}
		return out
	}
	return nil
}

// dialPublico es el dialer de toda salida del proxy (HTTP y Postgres): resuelve
// con lookup y se niega a conectar a una IP bloqueada, aunque lookup la
// devuelva (un DNS envenenado no debe llevar la clave a la LAN ni a los
// metadatos del cloud). Solo tcp4.
func dialPublico(lookup LookupFunc, d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var ips []string
		for _, a := range lookup(ctx, host) {
			if ip := net.ParseIP(a); ip != nil && ip.To4() != nil && !IsBlockedIP(ip) {
				ips = append(ips, ip.String())
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("credential proxy: %s does not resolve to a public IPv4", host)
		}
		var last error
		for _, ip := range ips {
			c, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(ip, port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
}

// salidaSegura es el transporte hacia el proveedor: TLS con las raíces del
// sistema y el dialer de dialPublico.
func salidaSegura(lookup LookupFunc) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialPublico(lookup, &net.Dialer{Timeout: 10 * time.Second}),
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: HeaderTimeout,
	}
}
