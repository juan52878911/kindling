package net

import (
	"encoding/binary"
	"errors"
	"io"
	stdnet "net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Estos tests ejercen SOLO el parseo DNS a mano y la lógica de allowlist: son
// funciones puras, sin root ni iptables, así que corren en cualquier plataforma.

// buildQuery arma una consulta DNS mínima (una pregunta) para un nombre y tipo.
func buildQuery(id uint16, name string, qtype uint16) []byte {
	msg := make([]byte, 12)
	binary.BigEndian.PutUint16(msg[0:2], id)
	binary.BigEndian.PutUint16(msg[2:4], 0x0100) // RD
	binary.BigEndian.PutUint16(msg[4:6], 1)      // QDCOUNT
	msg = append(msg, encodeName(name)...)
	var tc [4]byte
	binary.BigEndian.PutUint16(tc[0:2], qtype)
	binary.BigEndian.PutUint16(tc[2:4], 1) // IN
	return append(msg, tc[:]...)
}

func encodeName(name string) []byte {
	var out []byte
	if name != "" {
		for len(name) > 0 {
			i := 0
			for i < len(name) && name[i] != '.' {
				i++
			}
			out = append(out, byte(i))
			out = append(out, name[:i]...)
			if i < len(name) {
				name = name[i+1:]
			} else {
				name = ""
			}
		}
	}
	return append(out, 0)
}

func TestParseQuestion(t *testing.T) {
	q := buildQuery(0x1234, "www.example.com", 1)
	name, qtype, ok := parseQuestion(q)
	if !ok || name != "www.example.com" || qtype != 1 {
		t.Fatalf("parseQuestion = %q, %d, %v", name, qtype, ok)
	}
	if _, _, ok := parseQuestion([]byte{0, 1, 2}); ok {
		t.Fatal("una cabecera truncada no debería parsear")
	}
	// QNAME que se sale del buffer: no debe parsear ni entrar en pánico.
	bad := buildQuery(1, "a.b", 1)
	bad = bad[:14] // corta en mitad del nombre
	if _, _, ok := parseQuestion(bad); ok {
		t.Fatal("un nombre truncado no debería parsear")
	}
}

func TestReadNameCompressionLoop(t *testing.T) {
	// Un puntero que apunta a sí mismo no debe colgar el resolver.
	msg := make([]byte, 14)
	binary.BigEndian.PutUint16(msg[4:6], 1)
	msg[12] = 0xC0
	msg[13] = 12 // apunta al propio offset 12
	if _, _, ok := readName(msg, 12); ok {
		t.Fatal("un bucle de punteros debería fallar, no colgarse")
	}
}

func TestIsAllowed(t *testing.T) {
	r := &dnsResolver{allowed: normalizeDomains([]string{"Example.com", "cdn.net.", " "})}
	cases := map[string]bool{
		"example.com":         true,
		"EXAMPLE.COM":         true,
		"www.example.com":     true,
		"a.b.cdn.net":         true,
		"cdn.net":             true,
		"notexample.com":      false, // no es sufijo con punto
		"example.com.evil.io": false,
		"evil.io":             false,
	}
	for name, want := range cases {
		if got := r.isAllowed(name); got != want {
			t.Errorf("isAllowed(%q) = %v, quería %v", name, got, want)
		}
	}
}

func TestExtractA(t *testing.T) {
	// Respuesta con una pregunta, un A público (1.2.3.4, ttl 300), un A privado
	// (192.168.0.1, debe descartarse) y un AAAA (debe ignorarse).
	msg := buildQuery(0x1, "example.com", 1)
	binary.BigEndian.PutUint16(msg[6:8], 3) // ANCOUNT = 3
	msg[2] |= 0x80                          // QR

	add := func(typ uint16, ttl uint32, rdata []byte) {
		msg = append(msg, 0xC0, 12) // puntero al nombre de la pregunta
		var h [10]byte
		binary.BigEndian.PutUint16(h[0:2], typ)
		binary.BigEndian.PutUint16(h[2:4], 1) // IN
		binary.BigEndian.PutUint32(h[4:8], ttl)
		binary.BigEndian.PutUint16(h[8:10], uint16(len(rdata)))
		msg = append(msg, h[:]...)
		msg = append(msg, rdata...)
	}
	add(1, 300, []byte{1, 2, 3, 4})    // A público
	add(1, 60, []byte{192, 168, 0, 1}) // A privado -> descartado
	add(28, 300, make([]byte, 16))     // AAAA -> ignorado (TODO)

	recs := extractA(msg)
	if len(recs) != 1 {
		t.Fatalf("esperaba 1 registro A público, obtuve %d: %+v", len(recs), recs)
	}
	if recs[0].ip != "1.2.3.4" || recs[0].ttl != 300 {
		t.Fatalf("registro inesperado: %+v", recs[0])
	}
}

func TestRespondError(t *testing.T) {
	q := buildQuery(0xABCD, "example.com", 1)
	resp := respondError(q, 5) // REFUSED
	if resp == nil {
		t.Fatal("respondError devolvió nil")
	}
	if binary.BigEndian.Uint16(resp[0:2]) != 0xABCD {
		t.Error("el ID de la respuesta no coincide con el de la consulta")
	}
	if resp[2]&0x80 == 0 {
		t.Error("QR debería estar puesto en la respuesta")
	}
	if resp[3]&0x0F != 5 {
		t.Errorf("RCODE = %d, quería 5 (REFUSED)", resp[3]&0x0F)
	}
	if binary.BigEndian.Uint16(resp[6:8]) != 0 {
		t.Error("ANCOUNT debería ser 0")
	}
}

func TestNormalizeDomains(t *testing.T) {
	got := normalizeDomains([]string{" Foo.COM ", "bar.net.", "", "  "})
	want := []string{"foo.com", "bar.net"}
	if len(got) != len(want) {
		t.Fatalf("normalizeDomains = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizeDomains = %v, quería %v", got, want)
		}
	}
}

// Tests de N4 (resolver acotado): en-vuelo, dedupe del sembrado, y el filtro
// txid+pregunta sobre la respuesta upstream. No necesitan root ni netns: el
// "upstream" es un servidor UDP de juguete en localhost y `ejecutar` se
// sustituye por un fake, así que nunca se llama a `ip`/`ipset` de verdad.

// buildAnswerFor fabrica una respuesta con un único A que casa (mismo txid,
// misma pregunta) con query, o con un txid/pregunta distintos si se le pasa
// una query ya alterada, para probar responseMatches.
func buildAnswerFor(query []byte, ip string, ttl uint32) []byte {
	id := binary.BigEndian.Uint16(query[0:2])
	name, qtype, ok := parseQuestion(query)
	if !ok {
		return nil
	}
	msg := buildQuery(id, name, qtype)
	msg[2] |= 0x80                          // QR: es una respuesta
	binary.BigEndian.PutUint16(msg[6:8], 1) // ANCOUNT = 1
	msg = append(msg, 0xC0, 12)             // puntero al nombre de la pregunta
	var h [10]byte
	binary.BigEndian.PutUint16(h[0:2], 1) // TYPE A
	binary.BigEndian.PutUint16(h[2:4], 1) // CLASS IN
	binary.BigEndian.PutUint32(h[4:8], ttl)
	ip4 := stdnet.ParseIP(ip).To4()
	binary.BigEndian.PutUint16(h[8:10], uint16(len(ip4)))
	msg = append(msg, h[:]...)
	msg = append(msg, ip4...)
	return msg
}

// fakeUpstream levanta un servidor UDP de juguete en localhost que responde
// con lo que devuelva handle. Sirve de "upstream" para process()/forward() sin
// tocar la red real ni depender de resolvers públicos.
func fakeUpstream(t *testing.T, handle func(query []byte) []byte) (addr string, closeFn func()) {
	t.Helper()
	conn, err := stdnet.ListenUDP("udp4", &stdnet.UDPAddr{IP: stdnet.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("fakeUpstream: %v", err)
	}
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				select {
				case <-done:
				default:
				}
				return
			}
			q := append([]byte(nil), buf[:n]...)
			go func() {
				resp := handle(q)
				if resp != nil {
					_, _ = conn.WriteToUDP(resp, from)
				}
			}()
		}
	}()
	return conn.LocalAddr().String(), func() {
		close(done)
		conn.Close()
	}
}

func newTestResolver(upstream string) *dnsResolver {
	return &dnsResolver{
		ns:       "kl-test",
		set:      "kl-test",
		allowed:  normalizeDomains([]string{"example.com"}),
		upstream: upstream,
		sem:      make(chan struct{}, dnsMaxInFlight),
		tcpSem:   make(chan struct{}, dnsMaxTCPConns),
		limiter:  newTokenBucket(1e6, 1e6), // sin límite de tasa en estos tests
		seeded:   make(map[string]time.Time),
	}
}

// fakeEjecutar sustituye ejecutar por un no-op contador de llamadas y
// devuelve la función de restauración; el test debe defer-earla.
func fakeEjecutar(calls *int32) func() {
	old := ejecutar
	ejecutar = func(args ...string) error {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		return nil
	}
	return func() { ejecutar = old }
}

// TestProcessInFlightBounded lanza muchas más consultas concurrentes que
// dnsMaxInFlight contra un upstream que tarda en responder; comprueba que
// nunca hay más de dnsMaxInFlight forwards a la vez y que el resto recibe
// SERVFAIL en el sitio, sin llegar a tocar el upstream.
func TestProcessInFlightBounded(t *testing.T) {
	var inFlight, maxSeen, upstreamHits int32
	addr, closeFn := fakeUpstream(t, func(q []byte) []byte {
		atomic.AddInt32(&upstreamHits, 1)
		n := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxSeen)
			if n <= old {
				break
			}
			if atomic.CompareAndSwapInt32(&maxSeen, old, n) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return buildAnswerFor(q, "1.2.3.4", 300)
	})
	defer closeFn()
	restore := fakeEjecutar(nil)
	defer restore()

	r := newTestResolver(addr)

	const total = dnsMaxInFlight * 4
	var wg sync.WaitGroup
	var servfail int32
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(id uint16) {
			defer wg.Done()
			q := buildQuery(id, "example.com", 1)
			resp := r.process(q, false)
			if resp != nil && len(resp) >= 4 && resp[3]&0x0F == 2 {
				atomic.AddInt32(&servfail, 1)
			}
		}(uint16(i))
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxSeen); got > dnsMaxInFlight {
		t.Fatalf("en-vuelo máximo observado = %d, quería <= %d", got, dnsMaxInFlight)
	}
	if atomic.LoadInt32(&servfail) == 0 {
		t.Fatal("esperaba SERVFAIL para el exceso de consultas por encima del semáforo")
	}
	if hits := atomic.LoadInt32(&upstreamHits); hits > total {
		t.Fatalf("el upstream vio %d peticiones, más que las %d lanzadas", hits, total)
	}
}

// TestSeedDedupe comprueba que sembrar la misma IP vigente no repite el
// fork+exec, y que sí lo repite una vez caduca en el caché en memoria.
func TestSeedDedupe(t *testing.T) {
	var calls int32
	restore := fakeEjecutar(&calls)
	defer restore()

	r := newTestResolver("")
	r.seed("1.2.3.4", 300)
	r.seed("1.2.3.4", 300) // misma IP, sigue vigente: no debe volver a llamar a ejecutar
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("llamadas a ejecutar tras sembrar dos veces la misma IP vigente = %d, quería 1", got)
	}

	// Forzar la caducidad en memoria: debe volver a sembrar.
	r.seedMu.Lock()
	r.seeded["1.2.3.4"] = time.Now().Add(-time.Second)
	r.seedMu.Unlock()
	r.seed("1.2.3.4", 300)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("llamadas a ejecutar tras caducar el caché = %d, quería 2", got)
	}

	// Una IP distinta siempre siembra.
	r.seed("5.6.7.8", 300)
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("llamadas a ejecutar para una IP nueva = %d, quería 3", got)
	}
}

// TestResponseMatches cubre N-02: mismo txid + misma pregunta casa; un txid
// distinto o una pregunta distinta no casan.
func TestResponseMatches(t *testing.T) {
	q := buildQuery(0x1234, "example.com", 1)
	good := buildAnswerFor(q, "1.2.3.4", 300)
	if !responseMatches(q, good) {
		t.Fatal("una respuesta con el mismo txid y la misma pregunta debería casar")
	}

	otroTxid := buildQuery(0x9999, "example.com", 1)
	respOtroTxid := buildAnswerFor(otroTxid, "1.2.3.4", 300)
	if responseMatches(q, respOtroTxid) {
		t.Fatal("un txid distinto no debería casar")
	}

	otraPregunta := buildQuery(0x1234, "evil.io", 1)
	respOtraPregunta := buildAnswerFor(otraPregunta, "1.2.3.4", 300)
	if responseMatches(q, respOtraPregunta) {
		t.Fatal("una pregunta distinta no debería casar aunque el txid coincida")
	}
}

// TestProcessRejectsMismatchedAnswer verifica el camino completo: si el
// upstream responde algo que no casa con la consulta (txid ajeno), process
// devuelve SERVFAIL y NO siembra el ipset con esa IP.
func TestProcessRejectsMismatchedAnswer(t *testing.T) {
	addr, closeFn := fakeUpstream(t, func(q []byte) []byte {
		ajena := buildQuery(0xFFFF, "example.com", 1) // txid que no es el de la consulta real
		return buildAnswerFor(ajena, "9.9.9.9", 300)
	})
	defer closeFn()
	var calls int32
	restore := fakeEjecutar(&calls)
	defer restore()

	r := newTestResolver(addr)
	resp := r.process(buildQuery(0x1234, "example.com", 1), false)
	if resp == nil || len(resp) < 4 || resp[3]&0x0F != 2 {
		t.Fatalf("esperaba SERVFAIL ante una respuesta que no casa, obtuve %v", resp)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("no debería sembrar el ipset con una respuesta que no casa; llamadas = %d", got)
	}
}

// TestTokenBucketAllow comprueba el cubo de tokens en isolamiento: agota la
// ráfaga, deniega, y tras esperar recupera al menos un token.
func TestTokenBucketAllow(t *testing.T) {
	b := newTokenBucket(100, 2) // 100 tok/s, ráfaga 2
	if !b.allow() || !b.allow() {
		t.Fatal("los primeros allow() dentro de la ráfaga deberían pasar")
	}
	if b.allow() {
		t.Fatal("agotada la ráfaga, el siguiente allow() debería denegar")
	}
	time.Sleep(30 * time.Millisecond) // a 100 tok/s, sobran ~3 tokens en 30ms
	if !b.allow() {
		t.Fatal("tras esperar debería haber recuperado al menos un token")
	}
}

// Si el ipset add falla, la IP no queda marcada como sembrada: la siguiente
// respuesta con esa IP vuelve a intentarlo en vez de esperar todo el TTL.
func TestSeedReintentaSiEjecutarFalla(t *testing.T) {
	var calls int32
	old := ejecutar
	defer func() { ejecutar = old }()
	falla := true
	ejecutar = func(args ...string) error {
		atomic.AddInt32(&calls, 1)
		if falla {
			return errors.New("ipset: boom")
		}
		return nil
	}

	r := newTestResolver("")
	r.seed("1.2.3.4", 300)
	r.seedMu.Lock()
	_, marcada := r.seeded["1.2.3.4"]
	r.seedMu.Unlock()
	if marcada {
		t.Fatal("una siembra fallida quedó marcada como hecha")
	}
	falla = false
	r.seed("1.2.3.4", 300)
	r.seed("1.2.3.4", 300) // ya sí sembrada: dedupe
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("llamadas a ejecutar = %d, quería 2 (fallo + reintento)", got)
	}
}

// Por encima de dnsMaxTCPConns conexiones abiertas a la vez, las nuevas se
// cierran en el acto: un invitado que abre conexiones y no manda nada no
// acumula sockets ni goroutines en el daemon.
func TestServeTCPAcotaConexiones(t *testing.T) {
	ln, err := stdnet.ListenTCP("tcp4", &stdnet.TCPAddr{IP: stdnet.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	r := newTestResolver("")
	r.tcp = ln
	r.quit = make(chan struct{})
	r.wg.Add(1)
	go r.serveTCP()
	defer func() {
		close(r.quit)
		ln.Close()
		r.wg.Wait()
	}()

	var abiertas []stdnet.Conn
	defer func() {
		for _, c := range abiertas {
			c.Close()
		}
	}()
	for i := 0; i < dnsMaxTCPConns; i++ {
		c, err := stdnet.Dial("tcp4", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		abiertas = append(abiertas, c)
	}
	// Esperar a que el servidor haya aceptado las dnsMaxTCPConns.
	plazo := time.Now().Add(5 * time.Second)
	for len(r.tcpSem) < dnsMaxTCPConns && time.Now().Before(plazo) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(r.tcpSem); n != dnsMaxTCPConns {
		t.Fatalf("conexiones atendidas = %d, quería %d", n, dnsMaxTCPConns)
	}

	extra, err := stdnet.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	_ = extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("la conexión por encima del tope debía cerrarse en el acto (EOF), fue %v", err)
	}

	// Al cerrar una de las atendidas, su plaza se libera.
	abiertas[0].Close()
	abiertas = abiertas[1:]
	plazo = time.Now().Add(5 * time.Second)
	for len(r.tcpSem) >= dnsMaxTCPConns && time.Now().Before(plazo) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(r.tcpSem); n >= dnsMaxTCPConns {
		t.Fatalf("la plaza de una conexión cerrada no se liberó: %d", n)
	}
}
