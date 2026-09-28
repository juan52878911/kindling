package credproxy

// La salida hacia el proveedor: resolver a IPv4 públicas y no conectar jamás a
// una privada. En Linux el daemon inyecta su propio resolver (el mismo que
// siembra el ipset); este fichero trae uno equivalente para quien no lo tenga
// (el backend vz) y la barrera de IPs que se aplica siempre.

import (
	"context"
	"crypto/tls"
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
// solo con las IPv4 que no son de IsBlockedIP. Plazo de 5 s por consulta.
func PublicIPv4Lookup(dnsServer string) LookupFunc {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(dnsServer, "53"))
		},
	}
	return func(ctx context.Context, host string) []string {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		addrs, err := r.LookupHost(ctx, host)
		if err != nil {
			return nil
		}
		var out []string
		for _, a := range addrs {
			if ip := net.ParseIP(a); ip != nil && ip.To4() != nil && !IsBlockedIP(ip) {
				out = append(out, ip.String())
			}
		}
		return out
	}
}

// salidaSegura es el transporte hacia el proveedor: TLS con las raíces del
// sistema y un dialer que resuelve con lookup y se niega a conectar a una IP
// bloqueada, aunque lookup la devuelva (un DNS envenenado no debe llevar la
// clave a la LAN ni a los metadatos del cloud).
func salidaSegura(lookup LookupFunc) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
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
		},
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
}
