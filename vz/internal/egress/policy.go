// Package egress decide qué puede alcanzar el invitado fuera de su red.
//
// Replica la semántica de internal/net/firewall.go y dnsresolver.go del núcleo
// (none | internet | allowlist) sin iptables ni ipset: aquí cada conexión que el
// invitado abre pasa por la pila de red del propio proceso, que pregunta a
// Policy antes de marcar nada en el host. El modelo de amenaza es el mismo: el
// código del invitado es HOSTIL.
package egress

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

type Mode string

const (
	// None: sin salida. Solo MMDS, que se sirve dentro del proceso.
	None Mode = "none"
	// Internet: sale por el host, nunca a redes privadas ni al propio host.
	Internet Mode = "internet"
	// Allowlist: solo a las IPs públicas de los dominios listados.
	Allowlist Mode = "allowlist"
)

// ParseMode acepta los mismos valores que ParseEgress del núcleo. Vacío es
// none: la política por defecto es no tener salida.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case None, Internet, Allowlist:
		return Mode(s), nil
	case "":
		return None, nil
	}
	return "", fmt.Errorf("unknown egress policy: %q (use none, internet, or allowlist)", s)
}

// blocked es la lista de firewall.go del núcleo MÁS tres rangos que en Linux no
// hacen falta porque el kernel ya no los enruta (0/8, multicast y reservados):
// aquí es este proceso quien abre el socket, y un connect() a 0.0.0.0 en macOS
// llega a localhost.
var blocked = mustPrefixes(
	"10.0.0.0/8",     // privada
	"172.16.0.0/12",  // privada (incluye la 172.16.0.0/30 del invitado)
	"192.168.0.0/16", // privada: la LAN de casa
	"169.254.0.0/16", // enlace local y metadatos de cloud
	"127.0.0.0/8",    // loopback del host
	"100.64.0.0/10",  // CGNAT
	"0.0.0.0/8",      // "esta red": en el host acaba en localhost
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reservada y broadcast
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// IsBlockedIP dice si una IP cae en un rango que jamás se permite. Solo IPv4:
// el invitado no tiene IPv6 y cualquier otra cosa se trata como bloqueada.
func IsBlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.Is4() {
		return true
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// hostAddrs son las IPs de las interfaces del propio Mac. Con una IP pública
// asignada directamente (sin NAT), no estaría en ningún rango privado, y el
// invitado podría alcanzar servicios del host que solo escuchan "hacia fuera".
var (
	hostAddrsMu   sync.Mutex
	hostAddrs     map[netip.Addr]bool
	hostAddrsTime time.Time
)

func isHostAddr(ip netip.Addr) bool {
	hostAddrsMu.Lock()
	defer hostAddrsMu.Unlock()
	// Se refresca cada poco: el Mac cambia de red (wifi, VPN) sin que el
	// ayudante se entere.
	if hostAddrs == nil || time.Since(hostAddrsTime) > 30*time.Second {
		hostAddrs = map[netip.Addr]bool{}
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok {
					if ip, ok := netip.AddrFromSlice(n.IP); ok {
						hostAddrs[ip.Unmap()] = true
					}
				}
			}
		}
		hostAddrsTime = time.Now()
	}
	return hostAddrs[ip.Unmap()]
}

// Policy es la política de salida de una máquina. Es de la instancia, no del
// snapshot: dos réplicas del mismo snapshot pueden tener políticas distintas.
type Policy struct {
	mu      sync.RWMutex
	mode    Mode
	domains []string
	set     *IPSet
}

// NewPolicy empieza en none: si el núcleo nunca manda PUT /kling/network, la
// máquina no tiene salida.
func NewPolicy() *Policy {
	return &Policy{mode: None, set: NewIPSet()}
}

// Set cambia la política. Cambiar de allowlist vacía el conjunto: las IPs de
// una lista anterior no deben seguir abiertas con la nueva.
func (p *Policy) Set(mode Mode, domains []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = mode
	p.domains = NormalizeDomains(domains)
	p.set = NewIPSet()
}

func (p *Policy) Mode() Mode {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mode
}

func (p *Policy) Domains() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.domains...)
}

// Set devuelve el conjunto de IPs permitidas en modo allowlist.
func (p *Policy) IPSet() *IPSet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.set
}

// AllowConn decide si el invitado puede abrir una conexión TCP o un flujo UDP
// hacia dst. El DNS no pasa por aquí: se contesta dentro del proceso (ver
// AllowDNSName).
func (p *Policy) AllowConn(dst netip.Addr) bool {
	p.mu.RLock()
	mode, set := p.mode, p.set
	p.mu.RUnlock()
	if IsBlockedIP(dst) || isHostAddr(dst) {
		return false
	}
	switch mode {
	case Internet:
		return true
	case Allowlist:
		return set.Contains(dst)
	}
	return false
}

// AllowDNSName decide si una consulta DNS se reenvía. En none no hay DNS; en
// internet, cualquier nombre; en allowlist, solo los listados y sus
// subdominios, como isAllowed de dnsresolver.go: no reenviar nombres
// arbitrarios cierra el túnel por DNS hacia un NS del atacante.
func (p *Policy) AllowDNSName(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	switch p.mode {
	case Internet:
		return true
	case Allowlist:
		name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
		for _, d := range p.domains {
			if name == d || strings.HasSuffix(name, "."+d) {
				return true
			}
		}
	}
	return false
}

// NormalizeDomains pasa a minúsculas y quita el punto final y los vacíos.
func NormalizeDomains(domains []string) []string {
	var out []string
	for _, d := range domains {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// MinTTL es el suelo del plazo con que se permite una IP resuelta, como
// dnsMinTTL del núcleo: un TTL de 0 podría caducar la IP entre la respuesta DNS
// y el SYN del invitado.
const MinTTL = 60 * time.Second

// maxIPSet acota el conjunto: un invitado que resuelva sin parar nombres de un
// dominio permitido con DNS rotatorio no debe hacer crecer el mapa sin límite.
const maxIPSet = 4096

// IPSet es el equivalente del ipset con timeout del núcleo: IPs permanentes
// (sembrado estático) o con caducidad (sembrado dinámico por DNS).
type IPSet struct {
	mu  sync.Mutex
	ips map[netip.Addr]time.Time // cero = permanente
	now func() time.Time
}

func NewIPSet() *IPSet {
	return &IPSet{ips: map[netip.Addr]time.Time{}, now: time.Now}
}

// Add mete una IP. ttl<=0 la deja permanente. Las bloqueadas se descartan aquí
// también, para que un resolver envenenado no cuele 169.254 ni 192.168.
func (s *IPSet) Add(ip netip.Addr, ttl time.Duration) {
	ip = ip.Unmap()
	if IsBlockedIP(ip) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var exp time.Time
	if ttl > 0 {
		if ttl < MinTTL {
			ttl = MinTTL
		}
		exp = s.now().Add(ttl)
	}
	if old, ok := s.ips[ip]; ok {
		// Una permanente no se degrada a temporal, y una temporal solo se alarga.
		if old.IsZero() || (!exp.IsZero() && old.After(exp)) {
			return
		}
	}
	if len(s.ips) >= maxIPSet {
		s.pruneLocked()
		if len(s.ips) >= maxIPSet {
			return
		}
	}
	s.ips[ip] = exp
}

func (s *IPSet) Contains(ip netip.Addr) bool {
	ip = ip.Unmap()
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.ips[ip]
	if !ok {
		return false
	}
	if !exp.IsZero() && s.now().After(exp) {
		delete(s.ips, ip)
		return false
	}
	return true
}

func (s *IPSet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ips)
}

func (s *IPSet) pruneLocked() {
	now := s.now()
	for ip, exp := range s.ips {
		if !exp.IsZero() && now.After(exp) {
			delete(s.ips, ip)
		}
	}
}
