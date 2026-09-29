package egress

// El DNS del invitado se contesta en este proceso: toda consulta al puerto 53,
// vaya a la IP que vaya, llega aquí. Es el equivalente del DNAT del puerto 53 +
// resolver del host que monta el núcleo en modo allowlist, extendido a los tres
// modos porque en el Mac no hay otra forma de que el invitado resuelva.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"
)

// DefaultUpstream es el resolver al que se reenvía si el Mac no tiene uno
// legible en /etc/resolv.conf. Es el mismo que DNSResolver del núcleo.
const DefaultUpstream = "1.1.1.1:53"

// maxDNSMsg acota lo que se acepta de un mensaje DNS por TCP.
const maxDNSMsg = 64 << 10

// Resolver contesta las consultas DNS del invitado según la política.
type Resolver struct {
	Policy *Policy
	// Exchange manda una consulta cruda al upstream y devuelve la respuesta. Es
	// un campo para poder probar la lógica sin red.
	Exchange func(ctx context.Context, query []byte, viaTCP bool) ([]byte, error)
}

// NewResolver usa el primer nameserver del host (lo que el Mac tiene
// configurado, VPN incluida) y, si no hay, DefaultUpstream. Resolver "en el
// host" es eso: la consulta sale desde el Mac, no desde el invitado.
func NewResolver(p *Policy) *Resolver {
	up := HostUpstream()
	return &Resolver{Policy: p, Exchange: func(ctx context.Context, q []byte, tcp bool) ([]byte, error) {
		return exchange(ctx, up, q, tcp)
	}}
}

// HostUpstream lee /etc/resolv.conf del Mac.
func HostUpstream() string {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return DefaultUpstream
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 2 && fs[0] == "nameserver" {
			if ip, err := netip.ParseAddr(fs[1]); err == nil && ip.Is4() {
				return net.JoinHostPort(ip.String(), "53")
			}
		}
	}
	return DefaultUpstream
}

// Process decide, reenvía, siembra y devuelve la respuesta cruda. Nunca
// devuelve nil salvo que la consulta sea tan corta que no tenga cabecera.
func (r *Resolver) Process(ctx context.Context, query []byte, viaTCP bool) []byte {
	name, qtype, ok := ParseQuestion(query)
	if !ok {
		return RespondError(query, 1) // FORMERR
	}
	if ip, esGrafo, ok := r.Policy.GraphHost(name); esGrafo {
		// Un nombre de grafo: solo los de las aristas de ESTE nodo, y siempre
		// con la pasarela (la red pide al daemon a dónde va cada conexión).
		// Cualquier otro *.graph no existe, en todos los modos: ni se
		// reenvía ni se siembra. Como el resolver del núcleo en Linux.
		if !ok {
			return RespondError(query, 3) // NXDOMAIN
		}
		if qtype == 1 {
			return RespondA(query, ip, CredTTL)
		}
		return RespondError(query, 0) // NOERROR sin respuestas
	}
	if ip, ok := r.Policy.CredHost(name); ok {
		// Dominio con credencial: la única IP que el invitado debe usar para él
		// es la del proxy (la pasarela). A se contesta con ella; el resto de
		// tipos, AAAA incluido, con una respuesta vacía para que el cliente use
		// IPv4 y no busque otro camino. No se reenvía ni se siembra: la IP real
		// no entra en el conjunto, así que no hay salida directa que esquive al
		// proxy. Va antes que AllowDNSName, como en el núcleo: el dominio con
		// credencial no tiene por qué estar además en la lista.
		if qtype == 1 {
			return RespondA(query, ip, CredTTL)
		}
		return RespondError(query, 0) // NOERROR sin respuestas
	}
	if !r.Policy.AllowDNSName(name) {
		// En none y con dominios no listados se contesta REFUSED en vez de
		// callar: el invitado falla al momento en vez de esperar 5 s por
		// intento, y no sale nada del host.
		return RespondError(query, 5)
	}
	resp, err := r.Exchange(ctx, query, viaTCP)
	if err != nil || len(resp) < 12 {
		return RespondError(query, 2) // SERVFAIL
	}
	if r.Policy.Mode() == Allowlist {
		// Sembrar ANTES de responder: la IP que el invitado usará ya está
		// permitida cuando abra la conexión.
		set := r.Policy.IPSet()
		for _, rec := range ExtractA(resp) {
			set.Add(rec.IP, time.Duration(rec.TTL)*time.Second)
		}
	}
	return resp
}

// CredTTL es el TTL de la respuesta sintética de un dominio con credencial,
// el mismo que credTTL del núcleo: corto, para que un cambio se note pronto.
const CredTTL = 30

// PublicIPv4 resuelve host por el mismo upstream que ve el invitado y devuelve
// sus IPv4 permitidas como destino. Es el Lookup del proxy de credenciales: lo
// que el proxy alcanza coincide con lo que el modo allowlist dejaría ver, y
// ni un upstream envenenado ni uno que conteste 0.0.0.0 (que en macOS llega a
// localhost) ni una IP del propio Mac llevan la clave a otro sitio.
func (r *Resolver) PublicIPv4(ctx context.Context, host string) []string {
	q := BuildQuery(host, 1)
	if q == nil {
		return nil
	}
	resp, err := r.Exchange(ctx, q, false)
	if err != nil {
		return nil
	}
	var out []string
	for _, rec := range ExtractA(resp) {
		if !IsForbiddenDest(rec.IP) {
			out = append(out, rec.IP.String())
		}
	}
	return out
}

// SeedStatic resuelve los dominios de la lista y mete sus IPs públicas como
// permanentes, como el sembrado estático de applyAllowlist: es la red de
// seguridad para un invitado que conecte por una IP cacheada en el snapshot.
func (r *Resolver) SeedStatic(ctx context.Context) {
	if r.Policy.Mode() != Allowlist {
		return
	}
	set := r.Policy.IPSet()
	for _, d := range r.Policy.Domains() {
		q := BuildQuery(d, 1)
		if q == nil {
			continue
		}
		resp, err := r.Exchange(ctx, q, false)
		if err != nil {
			continue // falla CERRADO: sin IP, el dominio no se abre
		}
		for _, rec := range ExtractA(resp) {
			set.Add(rec.IP, 0)
		}
	}
}

func exchange(ctx context.Context, upstream string, query []byte, viaTCP bool) ([]byte, error) {
	network := "udp"
	if viaTCP {
		network = "tcp"
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	c, err := d.DialContext(ctx, network, upstream)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if !viaTCP {
		if _, err := c.Write(query); err != nil {
			return nil, err
		}
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
	if err := WriteTCPMsg(c, query); err != nil {
		return nil, err
	}
	return ReadTCPMsg(c)
}

// WriteTCPMsg escribe un mensaje DNS con el prefijo de longitud de TCP.
func WriteTCPMsg(w io.Writer, msg []byte) error {
	if len(msg) > 0xFFFF {
		return errors.New("dns message too large")
	}
	b := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(b, uint16(len(msg)))
	copy(b[2:], msg)
	_, err := w.Write(b)
	return err
}

// ReadTCPMsg lee un mensaje DNS con prefijo de longitud.
func ReadTCPMsg(r io.Reader) ([]byte, error) {
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(l[:]))
	if n > maxDNSMsg {
		return nil, errors.New("dns message too large")
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(io.LimitReader(r, int64(n)), msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// --- Parseo DNS mínimo, el mismo que dnsresolver.go del núcleo ---

// ParseQuestion extrae el nombre y el tipo de la PRIMERA pregunta.
func ParseQuestion(msg []byte) (name string, qtype uint16, ok bool) {
	if len(msg) < 12 {
		return "", 0, false
	}
	if binary.BigEndian.Uint16(msg[4:6]) < 1 {
		return "", 0, false
	}
	name, off, ok := readName(msg, 12)
	if !ok || off+4 > len(msg) {
		return "", 0, false
	}
	return name, binary.BigEndian.Uint16(msg[off : off+2]), true
}

// readName es defensivo: los mensajes vienen del invitado y del upstream, y no
// deben poder hacer entrar en bucle al ayudante ni leer fuera de rango.
func readName(msg []byte, off int) (name string, next int, ok bool) {
	var sb strings.Builder
	origNext := -1
	jumps := 0
	for {
		if off >= len(msg) {
			return "", 0, false
		}
		b := msg[off]
		if b == 0 {
			off++
			if origNext < 0 {
				origNext = off
			}
			return sb.String(), origNext, true
		}
		if b&0xC0 == 0xC0 {
			if off+1 >= len(msg) {
				return "", 0, false
			}
			if origNext < 0 {
				origNext = off + 2
			}
			off = int(b&0x3F)<<8 | int(msg[off+1])
			jumps++
			if jumps > 32 {
				return "", 0, false
			}
			continue
		}
		if b&0xC0 != 0 {
			return "", 0, false
		}
		l := int(b)
		off++
		if off+l > len(msg) {
			return "", 0, false
		}
		if sb.Len() > 0 {
			sb.WriteByte('.')
		}
		sb.Write(msg[off : off+l])
		off += l
	}
}

// ARecord es un registro A con su TTL en segundos.
type ARecord struct {
	IP  netip.Addr
	TTL uint32
}

// ExtractA devuelve los registros A de la sección de respuestas, sin los que
// caen en rangos bloqueados (upstream envenenado).
func ExtractA(msg []byte) []ARecord {
	if len(msg) < 12 {
		return nil
	}
	qd := int(binary.BigEndian.Uint16(msg[4:6]))
	an := int(binary.BigEndian.Uint16(msg[6:8]))
	off := 12
	for i := 0; i < qd; i++ {
		_, next, ok := readName(msg, off)
		if !ok {
			return nil
		}
		off = next + 4
		if off > len(msg) {
			return nil
		}
	}
	var out []ARecord
	for i := 0; i < an; i++ {
		_, next, ok := readName(msg, off)
		if !ok {
			return out
		}
		off = next
		if off+10 > len(msg) {
			return out
		}
		typ := binary.BigEndian.Uint16(msg[off : off+2])
		ttl := binary.BigEndian.Uint32(msg[off+4 : off+8])
		rdlen := int(binary.BigEndian.Uint16(msg[off+8 : off+10]))
		off += 10
		if off+rdlen > len(msg) {
			return out
		}
		if typ == 1 && rdlen == 4 {
			ip := netip.AddrFrom4([4]byte(msg[off : off+4]))
			if !IsBlockedIP(ip) {
				out = append(out, ARecord{IP: ip, TTL: ttl})
			}
		}
		off += rdlen
	}
	return out
}

// RespondA fabrica una respuesta con un único registro A para la pregunta de
// query, como respondA del núcleo: cabecera y pregunta copiadas (sin la
// sección adicional, p. ej. el OPT de EDNS) y la respuesta apuntando al nombre
// por compresión.
func RespondA(query []byte, ip netip.Addr, ttl uint32) []byte {
	if len(query) < 12 || !ip.Is4() {
		return RespondError(query, 2)
	}
	_, next, ok := readName(query, 12)
	if !ok || next+4 > len(query) {
		return RespondError(query, 1)
	}
	resp := append([]byte(nil), query[:next+4]...)
	resp[2] |= 0x80                          // QR=1
	resp[2] &^= 0x02                         // TC=0
	resp[3] = 0x80                           // RA=1, RCODE=0
	binary.BigEndian.PutUint16(resp[4:6], 1) // QDCOUNT
	binary.BigEndian.PutUint16(resp[6:8], 1) // ANCOUNT
	binary.BigEndian.PutUint16(resp[8:10], 0)
	binary.BigEndian.PutUint16(resp[10:12], 0)
	var rr [16]byte
	binary.BigEndian.PutUint16(rr[0:2], 0xC00C) // puntero al nombre de la pregunta
	binary.BigEndian.PutUint16(rr[2:4], 1)      // A
	binary.BigEndian.PutUint16(rr[4:6], 1)      // IN
	binary.BigEndian.PutUint32(rr[6:10], ttl)
	binary.BigEndian.PutUint16(rr[10:12], 4)
	ip4 := ip.As4()
	copy(rr[12:16], ip4[:])
	return append(resp, rr[:]...)
}

// RespondError fabrica una respuesta mínima a partir de la consulta con el
// RCODE dado y las secciones de respuesta a cero.
func RespondError(query []byte, rcode byte) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := append([]byte(nil), query...)
	resp[2] |= 0x80
	resp[3] = rcode & 0x0F
	binary.BigEndian.PutUint16(resp[6:8], 0)
	binary.BigEndian.PutUint16(resp[8:10], 0)
	binary.BigEndian.PutUint16(resp[10:12], 0)
	return resp
}

// BuildQuery construye una consulta estándar con recursión pedida. Sirve para
// el sembrado estático, que habla con el mismo upstream que el invitado para
// que las IPs permitidas coincidan con las que él verá.
func BuildQuery(name string, qtype uint16) []byte {
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return nil
	}
	b := []byte{0x4b, 0x56, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil
		}
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	b = append(b, 0, byte(qtype>>8), byte(qtype), 0, 1)
	return b
}
