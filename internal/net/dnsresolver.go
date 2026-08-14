package net

// Resolver dinámico DNS -> ipset del modo allowlist.
//
// EL HUECO QUE CIERRA (ver applyAllowlist en firewall.go): el ipset se sembraba
// UNA vez al arrancar, resolviendo los dominios en el host. Un dominio tras CDN
// o con DNS rotatorio (TTL corto) le devuelve al invitado una IP distinta de la
// sembrada, y esa conexión se caía (falla cerrado, molesto). Este resolver mete
// en el ipset la IP JUSTO ANTES de que el invitado la use, con el TTL real.
//
// MODELO (una instancia por microVM, atada al netns):
//   - Cada microVM tiene su propio resolver, una goroutine del daemon que
//     escucha en la IP del lado HOST de SU veth (n.HostIP), única por máquina.
//   - El DNS del invitado (puerto 53) se DNATea hacia n.HostIP:dnsPort, así que
//     cada microVM habla SOLO con su resolver. Un query de la microVM B no puede
//     llegar al socket de A: son IPs distintas en veths distintos.
//   - El resolver guarda EN MEMORIA solo los dominios de SU microVM y solo toca
//     SU ipset (ip netns exec <su-ns> ipset add ...). Aislamiento total: A nunca
//     ve los dominios de B ni siembra su set.
//
// MODELO DE AMENAZA (el invitado es HOSTIL):
//   - Dominio no listado: el resolver ni lo reenvía; responde REFUSED. Esto
//     cierra el DNS-tunneling por nombres arbitrarios que menciona el TODO de
//     DNSResolver: el invitado ya no puede hacer que el host reenvíe
//     datos-en-subdominios a un NS autoritativo cualquiera.
//   - Resolver ajeno: imposible, el 53 está DNATeado a este resolver (firewall).
//   - IP directa / IP no resuelta por aquí: sigue cayendo en el DROP del FORWARD,
//     porque solo lo que este resolver siembra entra en el ipset.
//   - Resolver upstream envenenado: las IP privadas/link-local se descartan antes
//     de sembrar (isBlockedIP), igual que en el sembrado estático.
//
// QUÉ QUEDA PENDIENTE (TODO):
//   - AAAA / IPv6: solo sembramos A (IPv4). El ipset es hash:ip v4 y las reglas
//     son iptables v4; cubrir AAAA pide un ipset family inet6 y reglas ip6tables
//     paralelas. Ver extractA.
//   - Tunneling por SUBDOMINIOS de un dominio permitido: se reenvían (un CDN los
//     necesita), así que un dominio permitido cuyo NS autoritativo controle el
//     atacante sigue siendo un canal. Cerrarlo del todo exige un resolver
//     autoritativo propio, fuera de alcance aquí.
//   - DoT/DoH del invitado: si el invitado hablara DNS-sobre-TLS/HTTPS a un
//     resolver externo, iría por el 443, no por el 53, y este resolver no lo ve.
//     Pero ese 443 solo sale si su IP está en el ipset, y su IP solo entra en el
//     ipset si ESTE resolver la sembró por un dominio permitido: el DoH a un
//     resolver ajeno no listado queda igualmente bloqueado por el FORWARD.

import (
	"encoding/binary"
	"fmt"
	"io"
	stdnet "net"
	"strings"
	"sync"
	"time"
)

// dnsPort es el puerto del host donde escucha el resolver de cada microVM. No es
// el 53 a propósito: en el host el 53 suele estar tomado (systemd-resolved u
// otro), y aquí atamos un socket por microVM a n.HostIP, que ya es único por
// máquina, así que un puerto alto fijo compartido no colisiona.
const dnsPort = 5333

// dnsMinTTL (segundos) es el suelo del timeout con que sembramos cada IP en el
// ipset. Respetamos el TTL real del registro, pero nunca por debajo de esto: un
// TTL de 0 o de pocos segundos podría evaporar la IP del set entre la respuesta
// DNS y el SYN del invitado, cerrando en falso una conexión legítima.
const dnsMinTTL = 60

// registro de resolvers vivos, por netns. La vida del resolver se ata a la de la
// microVM: Teardown/TeardownNamespace lo paran, Setup (o StartAllowlistResolver
// tras un reinicio del daemon) lo arrancan.
var (
	resolversMu sync.Mutex
	resolvers   = map[string]*dnsResolver{}
)

type dnsResolver struct {
	ns       string   // netns de esta microVM (para ipset add dentro de él)
	set      string   // nombre del ipset de esta microVM
	allowed  []string // dominios permitidos, normalizados (minúsculas, sin punto final)
	upstream string   // resolver real al que reenviamos (DNSResolver:53)
	udp      *stdnet.UDPConn
	tcp      *stdnet.TCPListener
	wg       sync.WaitGroup
	quit     chan struct{}
}

// startDNSResolver arranca (idempotente) el resolver del netns de n. Devuelve
// error si no puede atar los sockets: sin resolver, el DNAT del invitado apunta
// a un puerto sin nadie escuchando y se quedaría sin DNS, así que el llamador
// debe tratar el fallo como fatal para el Setup.
func startDNSResolver(n *Net, domains []string) error {
	resolversMu.Lock()
	if _, ok := resolvers[n.NS]; ok {
		resolversMu.Unlock()
		return nil // ya hay uno para este netns
	}
	resolversMu.Unlock()

	ip := stdnet.ParseIP(n.HostIP)
	if ip == nil {
		return fmt.Errorf("dns resolver: invalid host IP %q", n.HostIP)
	}
	udp, err := stdnet.ListenUDP("udp4", &stdnet.UDPAddr{IP: ip, Port: dnsPort})
	if err != nil {
		return fmt.Errorf("dns resolver: could not listen on udp %s:%d: %w", n.HostIP, dnsPort, err)
	}
	tcp, err := stdnet.ListenTCP("tcp4", &stdnet.TCPAddr{IP: ip, Port: dnsPort})
	if err != nil {
		udp.Close()
		return fmt.Errorf("dns resolver: could not listen on tcp %s:%d: %w", n.HostIP, dnsPort, err)
	}

	r := &dnsResolver{
		ns:       n.NS,
		set:      n.setName(),
		allowed:  normalizeDomains(domains),
		upstream: DNSResolver + ":53",
		udp:      udp,
		tcp:      tcp,
		quit:     make(chan struct{}),
	}
	r.wg.Add(2)
	go r.serveUDP()
	go r.serveTCP()

	resolversMu.Lock()
	resolvers[n.NS] = r
	resolversMu.Unlock()
	return nil
}

// stopResolver para y desregistra el resolver de un netns, si lo hay.
func stopResolver(ns string) {
	resolversMu.Lock()
	r := resolvers[ns]
	delete(resolvers, ns)
	resolversMu.Unlock()
	if r != nil {
		r.stop()
	}
}

func (r *dnsResolver) stop() {
	close(r.quit)
	r.udp.Close()
	r.tcp.Close()
	r.wg.Wait()
}

func (r *dnsResolver) serveUDP() {
	defer r.wg.Done()
	buf := make([]byte, 1500) // una query normal cabe de sobra
	for {
		nread, from, err := r.udp.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-r.quit:
				return
			default:
				time.Sleep(10 * time.Millisecond) // error raro: evita bucle caliente
				continue
			}
		}
		query := append([]byte(nil), buf[:nread]...)
		go func() {
			resp := r.process(query, false)
			if resp == nil {
				return
			}
			_ = r.udp.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, _ = r.udp.WriteToUDP(resp, from)
		}()
	}
}

func (r *dnsResolver) serveTCP() {
	defer r.wg.Done()
	for {
		conn, err := r.tcp.Accept()
		if err != nil {
			select {
			case <-r.quit:
				return
			default:
				time.Sleep(10 * time.Millisecond)
				continue
			}
		}
		go r.handleTCP(conn)
	}
}

// handleTCP atiende DNS sobre TCP (mensajes con prefijo de longitud de 2 bytes).
// El invitado usa TCP cuando la respuesta UDP viene truncada; mantenerlo evita
// romper esos casos sin dejar de aplicar la allowlist y el sembrado.
func (r *dnsResolver) handleTCP(conn stdnet.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var l [2]byte
	if _, err := io.ReadFull(conn, l[:]); err != nil {
		return
	}
	n := binary.BigEndian.Uint16(l[:])
	if n == 0 {
		return
	}
	query := make([]byte, n)
	if _, err := io.ReadFull(conn, query); err != nil {
		return
	}
	resp := r.process(query, true)
	if resp == nil {
		return
	}
	binary.BigEndian.PutUint16(l[:], uint16(len(resp)))
	_, _ = conn.Write(l[:])
	_, _ = conn.Write(resp)
}

// process es el corazón: decide, reenvía, siembra y devuelve la respuesta cruda.
func (r *dnsResolver) process(query []byte, viaTCP bool) []byte {
	name, _, ok := parseQuestion(query)
	if !ok {
		return respondError(query, 1) // FORMERR: no lo entendemos
	}
	if !r.isAllowed(name) {
		// Dominio no listado: ni lo reenviamos. Cierra el DNS-tunneling por
		// nombres arbitrarios (el invitado no puede filtrar datos codificados en
		// subdominios de un dominio suyo hacia un NS del atacante).
		return respondError(query, 5) // REFUSED
	}
	resp, err := r.forward(query, viaTCP)
	if err != nil || len(resp) < 12 {
		return respondError(query, 2) // SERVFAIL
	}
	// Sembrar el ipset con las IPv4 que vamos a devolver, ANTES de responder: así
	// la IP que el invitado usará ya está permitida cuando abra la conexión.
	for _, rec := range extractA(resp) {
		r.seed(rec.ip, rec.ttl)
	}
	return resp
}

func (r *dnsResolver) forward(query []byte, viaTCP bool) ([]byte, error) {
	if viaTCP {
		return r.forwardTCP(query)
	}
	return r.forwardUDP(query)
}

func (r *dnsResolver) forwardUDP(query []byte) ([]byte, error) {
	c, err := stdnet.DialTimeout("udp", r.upstream, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	nread, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:nread], nil
}

func (r *dnsResolver) forwardTCP(query []byte) ([]byte, error) {
	c, err := stdnet.DialTimeout("tcp", r.upstream, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(query)))
	if _, err := c.Write(l[:]); err != nil {
		return nil, err
	}
	if _, err := c.Write(query); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	resp := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(c, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// seed mete una IP en el ipset de esta microVM con el TTL real (con suelo). El
// ipset se creó con la opción timeout activada (ver applyAllowlist), así que el
// add por-entrada con timeout caduca solo cuando expira el registro.
func (r *dnsResolver) seed(ip string, ttl uint32) {
	to := int(ttl)
	if to < dnsMinTTL {
		to = dnsMinTTL
	}
	_ = run("ip", "netns", "exec", r.ns, "ipset", "add", r.set, ip,
		"timeout", fmt.Sprintf("%d", to), "-exist")
}

func (r *dnsResolver) isAllowed(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	for _, d := range r.allowed {
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

func normalizeDomains(domains []string) []string {
	var out []string
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimSuffix(d, ".")
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// --- Parseo DNS mínimo a mano (solo lo que necesitamos) ---

// parseQuestion extrae el nombre y el tipo de la PRIMERA pregunta. No hay
// compresión en la sección de preguntas, pero readName la tolera igualmente.
func parseQuestion(msg []byte) (name string, qtype uint16, ok bool) {
	if len(msg) < 12 {
		return "", 0, false
	}
	if binary.BigEndian.Uint16(msg[4:6]) < 1 { // QDCOUNT
		return "", 0, false
	}
	name, off, ok := readName(msg, 12)
	if !ok || off+4 > len(msg) {
		return "", 0, false
	}
	return name, binary.BigEndian.Uint16(msg[off : off+2]), true
}

// readName lee un nombre DNS desde off. Devuelve el nombre en texto y el offset
// del byte siguiente al nombre (saltando el puntero de compresión si lo hubo).
// Es defensivo: los mensajes vienen de fuera (invitado y upstream) y no deben
// poder hacer que el resolver entre en bucle ni lea fuera de rango.
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
		if b&0xC0 == 0xC0 { // puntero de compresión
			if off+1 >= len(msg) {
				return "", 0, false
			}
			if origNext < 0 {
				origNext = off + 2
			}
			off = int(b&0x3F)<<8 | int(msg[off+1])
			jumps++
			if jumps > 32 {
				return "", 0, false // cadena de punteros absurda: cortamos
			}
			continue
		}
		if b&0xC0 != 0 {
			return "", 0, false // 0x40/0x80 no son longitudes válidas
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

type aRecord struct {
	ip  string
	ttl uint32
}

// extractA recorre la sección de respuestas y devuelve los registros A (IPv4)
// con su TTL. Descarta IP privadas/link-local (resolver upstream envenenado).
//
// TODO(AAAA): los registros tipo 28 (IPv6) se ignoran. Sembrarlos exige un ipset
// family inet6 y reglas ip6tables paralelas que hoy no existen.
func extractA(msg []byte) []aRecord {
	if len(msg) < 12 {
		return nil
	}
	qd := int(binary.BigEndian.Uint16(msg[4:6]))
	an := int(binary.BigEndian.Uint16(msg[6:8]))
	off := 12
	for i := 0; i < qd; i++ { // saltar preguntas
		_, next, ok := readName(msg, off)
		if !ok {
			return nil
		}
		off = next + 4 // qtype + qclass
		if off > len(msg) {
			return nil
		}
	}
	var out []aRecord
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
		if typ == 1 && rdlen == 4 { // A (IPv4)
			ip := stdnet.IP(msg[off : off+4])
			if !isBlockedIP(ip) {
				out = append(out, aRecord{ip: ip.String(), ttl: ttl})
			}
		}
		off += rdlen
	}
	return out
}

// respondError fabrica una respuesta mínima a partir de la consulta: marca QR=1
// y pone el RCODE dado, con las secciones de respuesta a cero. Sirve para
// REFUSED (dominio no listado), SERVFAIL (upstream caído) y FORMERR (query rara)
// sin construir el mensaje entero a mano.
func respondError(query []byte, rcode byte) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := append([]byte(nil), query...)
	resp[2] |= 0x80                            // QR=1 (es una respuesta)
	resp[3] = rcode & 0x0F                     // RA=0, Z=0, RCODE
	binary.BigEndian.PutUint16(resp[6:8], 0)   // ANCOUNT
	binary.BigEndian.PutUint16(resp[8:10], 0)  // NSCOUNT
	binary.BigEndian.PutUint16(resp[10:12], 0) // ARCOUNT
	return resp
}
