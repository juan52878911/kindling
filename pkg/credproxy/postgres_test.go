package credproxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests del proxy de Postgres contra un servidor falso que habla el protocolo
// v3 con TLS y SCRAM de servidor (escrito aquí, independiente del cliente).

const (
	pgDominio = "db.example.com"
	pgUser    = "app"
	pgDB      = "appdb"
	pgClave   = "s3cr3t-real-password"
	pgMarca   = PlaceholderPrefix + "0123456789abcdef"
)

// certPG genera un certificado autofirmado para host: su tls.Certificate y
// el PEM que hace de CA.
func certPG(t *testing.T, host string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// servidorPG es un PostgreSQL de mentira.
type servidorPG struct {
	t    *testing.T
	ln   net.Listener
	cert tls.Certificate
	// modo de autenticación: scram, scram-plus, password, md5, trust, error,
	// ok-sin-final (SCRAM sin SASLFinal), firma-mala (v= que no vale).
	modo string
	// ssl es lo que contesta al SSLRequest ('S' por defecto); inyectar se
	// manda justo detrás de la 'S', antes del TLS.
	ssl     byte
	inyecta []byte

	mu        sync.Mutex
	params    map[string]string
	consultas []string
	cancelado [][2]uint32
	cbVisto   string
	clavesVis []string
	conns     atomic.Int32
}

func nuevoServidorPG(t *testing.T, modo string) *servidorPG {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := certPG(t, pgDominio)
	s := &servidorPG{t: t, ln: ln, cert: cert, modo: modo, ssl: 'S'}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns.Add(1)
			go s.atender(c)
		}
	}()
	return s
}

func (s *servidorPG) atender(raw net.Conn) {
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	code, _, err := leerArranque(raw)
	if err != nil || code != pgSSLRequest {
		return
	}
	raw.Write(append([]byte{s.ssl}, s.inyecta...))
	if s.ssl != 'S' {
		return
	}
	c := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{s.cert}})
	if err := c.Handshake(); err != nil {
		return
	}
	code, cuerpo, err := leerArranque(c)
	if err != nil {
		return
	}
	if code == pgCancelRequest {
		s.mu.Lock()
		s.cancelado = append(s.cancelado, [2]uint32{binary.BigEndian.Uint32(cuerpo[:4]), binary.BigEndian.Uint32(cuerpo[4:])})
		s.mu.Unlock()
		return
	}
	ps, _, err := parsearParams(cuerpo)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.params = map[string]string{}
	for _, p := range ps {
		s.params[p[0]] = p[1]
	}
	s.mu.Unlock()
	br := bufio.NewReader(c)
	if !s.autenticar(c, br) {
		return
	}
	c.Write(mensajePG('R', []byte{0, 0, 0, 0}))
	c.Write(mensajePG('S', []byte("server_version\x0017.0\x00")))
	c.Write(mensajePG('K', []byte{0, 0, 0, 42, 0xde, 0xad, 0xbe, 0xef}))
	c.Write(mensajePG('Z', []byte{'I'}))
	for {
		tipo, msg, err := leerMensaje(br, 1<<20)
		if err != nil || tipo == 'X' {
			return
		}
		if tipo != 'Q' {
			continue
		}
		q := strings.TrimSuffix(string(msg), "\x00")
		s.mu.Lock()
		s.consultas = append(s.consultas, q)
		s.mu.Unlock()
		if q == "SELECT pg_sleep(60)" {
			// Hasta que llegue la cancelación.
			for i := 0; i < 500; i++ {
				s.mu.Lock()
				n := len(s.cancelado)
				s.mu.Unlock()
				if n > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			c.Write(errorResponsePG("57014", "canceling statement due to user request"))
			c.Write(mensajePG('Z', []byte{'I'}))
			continue
		}
		// Devuelve la consulta como una fila: "eco".
		fila := binary.BigEndian.AppendUint16(nil, 1)
		fila = binary.BigEndian.AppendUint32(fila, uint32(len(q)))
		fila = append(fila, q...)
		c.Write(mensajePG('D', fila))
		c.Write(mensajePG('C', []byte("SELECT 1\x00")))
		c.Write(mensajePG('Z', []byte{'I'}))
	}
}

func (s *servidorPG) autenticar(c *tls.Conn, br *bufio.Reader) bool {
	switch s.modo {
	case "trust":
		return true
	case "error":
		c.Write(errorResponsePG("28P01", "password authentication failed for user \"app\" SECRETO-DEL-SERVIDOR"))
		return false
	case "md5":
		c.Write(mensajePG('R', []byte{0, 0, 0, 5, 1, 2, 3, 4}))
		return false
	case "password":
		c.Write(mensajePG('R', []byte{0, 0, 0, 3}))
		tipo, msg, err := leerMensaje(br, 1024)
		if err != nil || tipo != 'p' {
			return false
		}
		clave := strings.TrimSuffix(string(msg), "\x00")
		s.mu.Lock()
		s.clavesVis = append(s.clavesVis, clave)
		s.mu.Unlock()
		if clave != pgClave {
			c.Write(errorResponsePG("28P01", "bad password"))
			return false
		}
		return true
	}
	// SCRAM de servidor.
	mecs := scramSHA256 + "\x00"
	if s.modo == "scram-plus" {
		mecs = scramSHA256Plus + "\x00" + mecs
	}
	c.Write(mensajePG('R', append([]byte{0, 0, 0, 10}, mecs+"\x00"...)))
	tipo, msg, err := leerMensaje(br, 4096)
	if err != nil || tipo != 'p' {
		return false
	}
	mec, resto, _ := cadenaC(msg)
	primero := string(resto[4:])
	gs2 := primero[:strings.Index(primero, "n=")]
	desnudo := primero[len(gs2):]
	if s.modo == "scram-plus" && (mec != scramSHA256Plus || gs2 != "p=tls-server-end-point,,") {
		return false
	}
	if s.modo != "scram-plus" && (mec != scramSHA256 || gs2 != "y,,") {
		return false
	}
	cnonce := desnudo[strings.Index(desnudo, "r=")+2:]
	sal := []byte("salsalsal")
	sf := "r=" + cnonce + "srv-nonce,s=" + base64.StdEncoding.EncodeToString(sal) + ",i=4096"
	c.Write(mensajePG('R', append([]byte{0, 0, 0, 11}, sf...)))
	tipo, msg, err = leerMensaje(br, 4096)
	if err != nil || tipo != 'p' {
		return false
	}
	cf := string(msg)
	i := strings.LastIndex(cf, ",p=")
	sinPrueba, prueba64 := cf[:i], cf[i+3:]
	attrs, _ := scramAtributos(sinPrueba)
	cbEsperado := []byte(gs2)
	if s.modo == "scram-plus" {
		cbEsperado = append(cbEsperado, tlsServerEndPoint(s.cert.Leaf)...)
	}
	s.mu.Lock()
	s.cbVisto = attrs['c']
	s.mu.Unlock()
	if attrs['c'] != base64.StdEncoding.EncodeToString(cbEsperado) || attrs['r'] != cnonce+"srv-nonce" {
		c.Write(errorResponsePG("28000", "channel binding"))
		return false
	}
	salted, _ := pbkdf2.Key(sha256.New, pgClave, sal, 4096, 32)
	mac := func(k []byte, m string) []byte { h := hmac.New(sha256.New, k); h.Write([]byte(m)); return h.Sum(nil) }
	authMsg := desnudo + "," + sf + "," + sinPrueba
	stored := sha256.Sum256(mac(salted, "Client Key"))
	firma := mac(stored[:], authMsg)
	prueba, _ := base64.StdEncoding.DecodeString(prueba64)
	if len(prueba) != 32 {
		return false
	}
	for i := range prueba {
		prueba[i] ^= firma[i]
	}
	if got := sha256.Sum256(prueba); !hmac.Equal(got[:], stored[:]) {
		c.Write(errorResponsePG("28P01", "bad proof"))
		return false
	}
	if s.modo == "ok-sin-final" {
		return true
	}
	v := mac(mac(salted, "Server Key"), authMsg)
	if s.modo == "firma-mala" {
		v[0] ^= 1
	}
	c.Write(mensajePG('R', append([]byte{0, 0, 0, 12}, "v="+base64.StdEncoding.EncodeToString(v)...)))
	return true
}

// entornoPG es un proxy con su PGServer y su registro.
type entornoPG struct {
	addr  string
	p     *Proxy
	ps    *PGServer
	audit string
	// dialed son las direcciones que el proxy pidió al dialer.
	mu     sync.Mutex
	dialed []string
}

// registro cierra el servidor (espera a que cada sesión deje su línea) y
// devuelve el registro.
func (e *entornoPG) registro(t *testing.T) ([]Record, string) {
	t.Helper()
	_ = e.ps.Close()
	return leerRegistro(t, e.p, e.audit)
}

// proxyPG monta un proxy con la credencial Postgres (y la CA del servidor) y
// un PGServer en 127.0.0.1. El dialer inyectado lleva siempre al servidor
// falso, sea cual sea el nombre.
func proxyPG(t *testing.T, srv *servidorPG, mod func(*Credential), opts ...any) *entornoPG {
	t.Helper()
	_, ca := certPG(t, pgDominio)
	if srv != nil {
		ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.cert.Certificate[0]}))
	}
	e := &entornoPG{audit: filepath.Join(t.TempDir(), AuditFile)}
	o := Options{AuditPath: e.audit, DialPG: func(ctx context.Context, network, addr string) (net.Conn, error) {
		e.mu.Lock()
		e.dialed = append(e.dialed, addr)
		e.mu.Unlock()
		if srv == nil {
			return nil, errors.New("sin servidor")
		}
		var d net.Dialer
		return d.DialContext(ctx, network, srv.ln.Addr().String())
	}}
	for _, f := range opts {
		if f, ok := f.(func(*Options)); ok {
			f(&o)
		}
	}
	p := New(o)
	for _, f := range opts {
		if f, ok := f.(func(*Proxy)); ok {
			f(p)
		}
	}
	e.p = p
	cred := Credential{Env: "PGPASSWORD", Domain: pgDominio, Placeholder: pgMarca, Secret: pgClave,
		Kind: KindPostgres, User: pgUser, Database: pgDB, CAPEM: ca}
	if mod != nil {
		mod(&cred)
	}
	if _, err := p.SetCredentials([]Credential{cred}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.ps = NewPGServer(p)
	e.addr = ln.Addr().String()
	go e.ps.Serve(ln)
	t.Cleanup(func() { e.ps.Close(); p.Close() })
	return e
}

// clientePG es un cliente mínimo del lado del invitado.
type clientePG struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

func conectarPG(t *testing.T, addr string) *clientePG {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	t.Cleanup(func() { c.Close() })
	return &clientePG{t: t, c: c, br: bufio.NewReader(c)}
}

func (k *clientePG) arranque(minor uint16, pares ...string) {
	b := binary.BigEndian.AppendUint32(make([]byte, 4), 3<<16|uint32(minor))
	for _, p := range pares {
		b = append(append(b, p...), 0)
	}
	b = append(b, 0)
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	k.c.Write(b)
}

func (k *clientePG) leer() (byte, []byte) {
	k.t.Helper()
	tipo, msg, err := leerMensaje(k.br, 1<<20)
	if err != nil {
		k.t.Fatalf("leyendo del proxy: %v", err)
	}
	return tipo, msg
}

// login hace el arranque estándar y manda pass; devuelve el primer mensaje
// tras la contraseña.
func (k *clientePG) login(pass string, pares ...string) (byte, []byte) {
	k.t.Helper()
	if len(pares) == 0 {
		pares = []string{"user", pgUser, "database", pgDB, "application_name", "test"}
	}
	k.arranque(0, pares...)
	tipo, msg := k.leer()
	if tipo != 'R' || binary.BigEndian.Uint32(msg) != 3 {
		k.t.Fatalf("esperaba AuthenticationCleartextPassword, llegó %q %x", tipo, msg)
	}
	k.c.Write(mensajePG('p', append([]byte(pass), 0)))
	return k.leer()
}

// hastaListo lee hasta ReadyForQuery y devuelve la clave de BackendKeyData.
func (k *clientePG) hastaListo() (pid, clave uint32) {
	k.t.Helper()
	for {
		tipo, msg := k.leer()
		switch tipo {
		case 'K':
			pid, clave = binary.BigEndian.Uint32(msg[:4]), binary.BigEndian.Uint32(msg[4:])
		case 'Z':
			return
		case 'E':
			k.t.Fatalf("error del proxy: %q", msg)
		}
	}
}

func (k *clientePG) consulta(q string) (byte, []byte) {
	k.c.Write(mensajePG('Q', append([]byte(q), 0)))
	return k.leer()
}

// Camino feliz con SCRAM: el invitado se autentica con el marcador, el servidor
// recibe la clave real (por SCRAM, con "y" porque no ofrece -PLUS), una
// consulta va y vuelve, el marcador en el SQL NO se sustituye, BackendKeyData
// llega con una clave falsa y el registro dice lo justo.
func TestPGSCRAMConsultaYRegistro(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	e := proxyPG(t, srv, nil)
	k := conectarPG(t, e.addr)
	tipo, msg := k.login(pgMarca)
	if tipo != 'R' || binary.BigEndian.Uint32(msg) != 0 {
		t.Fatalf("esperaba AuthenticationOk, llegó %q %q", tipo, msg)
	}
	pid, clave := k.hastaListo()
	if pid != 42 || clave == 0xdeadbeef {
		t.Fatalf("BackendKeyData pid=%d clave=%x: la clave real no debe llegar al invitado", pid, clave)
	}
	q := "SELECT '" + pgMarca + "'"
	tipo, msg = k.consulta(q)
	if tipo != 'D' || !strings.Contains(string(msg), pgMarca) {
		t.Fatalf("fila %q %q", tipo, msg)
	}
	k.leer() // C
	k.leer() // Z
	k.c.Write(mensajePG('X', nil))
	k.c.Close()

	srv.mu.Lock()
	params, consultas := srv.params, srv.consultas
	srv.mu.Unlock()
	if params["user"] != pgUser || params["database"] != pgDB || params["application_name"] != "test" {
		t.Errorf("parámetros en el servidor: %v", params)
	}
	if len(consultas) != 1 || consultas[0] != q {
		t.Errorf("el SQL no llegó tal cual (sin sustituir el marcador): %q", consultas)
	}
	if len(e.dialed) != 1 || e.dialed[0] != net.JoinHostPort(pgDominio, "5432") {
		t.Errorf("el proxy marcó %v", e.dialed)
	}
	recs, crudo := e.registro(t)
	if len(recs) != 1 {
		t.Fatalf("registros: %v", recs)
	}
	r := recs[0]
	if r.Kind != KindPostgres || r.Host != pgDominio || r.User != pgUser || r.Database != pgDB ||
		r.Auth != AuthSCRAM || r.Reason != "" || r.Denied || len(r.Creds) != 1 || r.Creds[0] != "PGPASSWORD" ||
		r.ReqBytes == 0 || r.RespBytes == 0 {
		t.Errorf("registro %+v", r)
	}
	for _, prohibido := range []string{pgClave, pgMarca, "SELECT"} {
		if strings.Contains(crudo, prohibido) {
			t.Errorf("el registro contiene %q: %s", prohibido, crudo)
		}
	}
}

// Con -PLUS ofrecido, el proxy lo usa y los datos del canal son el hash del
// certificado del servidor.
func TestPGSCRAMPlus(t *testing.T) {
	srv := nuevoServidorPG(t, "scram-plus")
	e := proxyPG(t, srv, nil)
	k := conectarPG(t, e.addr)
	if tipo, msg := k.login(pgMarca); tipo != 'R' || binary.BigEndian.Uint32(msg) != 0 {
		t.Fatalf("esperaba AuthenticationOk, llegó %q %q", tipo, msg)
	}
	k.hastaListo()
	k.c.Close()
	recs, _ := e.registro(t)
	if len(recs) != 1 || recs[0].Auth != AuthSCRAMPlus {
		t.Fatalf("registro %+v", recs)
	}
}

// Contraseña en claro hacia el servidor: solo dentro del TLS verificado, y lo
// que llega es la clave real.
func TestPGPasswordDentroDelTLS(t *testing.T) {
	srv := nuevoServidorPG(t, "password")
	e := proxyPG(t, srv, nil)
	k := conectarPG(t, e.addr)
	if tipo, msg := k.login(pgMarca); tipo != 'R' || binary.BigEndian.Uint32(msg) != 0 {
		t.Fatalf("esperaba AuthenticationOk, llegó %q %q", tipo, msg)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.clavesVis) != 1 || srv.clavesVis[0] != pgClave {
		t.Fatalf("el servidor recibió %q", srv.clavesVis)
	}
}

// esperarError lee hasta un ErrorResponse y devuelve su SQLSTATE y el mensaje
// entero en crudo. Falla si llega AuthenticationOk.
func (k *clientePG) esperarError(tipo byte, msg []byte) (string, string) {
	k.t.Helper()
	for {
		switch tipo {
		case 'E':
			return sqlstate(msg), string(msg)
		case 'R':
			if len(msg) >= 4 && binary.BigEndian.Uint32(msg) == 0 {
				k.t.Fatal("AuthenticationOk cuando debía fallar")
			}
		}
		tipo, msg = k.leer()
	}
}

// Todo lo que debe acabar en error y sin AuthenticationOk: el SQLSTATE que ve
// el invitado, el motivo del registro y que nunca se filtre el texto del
// servidor.
func TestPGRechazos(t *testing.T) {
	casos := []struct {
		nombre, modo string
		ssl          byte
		inyecta      []byte
		pass         string
		pares        []string
		mod          func(*Credential)
		code, motivo string
		denied       bool
		sinServidor  bool // el proxy no debe ni conectar
		sinClave     bool // el rechazo llega antes de pedir la contraseña
	}{
		{nombre: "marcador malo", modo: "scram", pass: PlaceholderPrefix + "otro", code: "28P01", motivo: ReasonBadPlaceholder, denied: true, sinServidor: true},
		{nombre: "clave real como contraseña", modo: "scram", pass: pgClave, code: "28P01", motivo: ReasonBadPlaceholder, denied: true, sinServidor: true},
		{nombre: "otro rol", modo: "scram", pass: pgMarca, pares: []string{"user", "postgres", "database", pgDB}, code: "28000", motivo: ReasonUserMismatch, denied: true, sinServidor: true},
		{nombre: "otra base", modo: "scram", pass: pgMarca, pares: []string{"user", pgUser, "database", "postgres"}, code: "28000", motivo: ReasonDatabaseMismatch, denied: true, sinServidor: true},
		{nombre: "base por defecto = rol", modo: "scram", pass: pgMarca, pares: []string{"user", pgUser}, code: "28000", motivo: ReasonDatabaseMismatch, denied: true, sinServidor: true},
		{nombre: "replicación", modo: "scram", pass: pgMarca, pares: []string{"user", pgUser, "replication", "database"}, code: "28000", motivo: ReasonReplication, denied: true, sinServidor: true, sinClave: true},
		{nombre: "parámetro repetido", modo: "scram", pass: pgMarca, pares: []string{"user", pgUser, "user", "postgres"}, code: "08P01", motivo: ReasonBadStartup, sinServidor: true, sinClave: true},
		{nombre: "md5", modo: "md5", pass: pgMarca, code: "28P01", motivo: ReasonUpstreamAuth},
		{nombre: "error del servidor", modo: "error", pass: pgMarca, code: "28P01", motivo: ReasonUpstreamAuth},
		{nombre: "Ok sin SASLFinal", modo: "ok-sin-final", pass: pgMarca, code: "28P01", motivo: ReasonUpstreamAuth},
		{nombre: "firma del servidor mala", modo: "firma-mala", pass: pgMarca, code: "28P01", motivo: ReasonUpstreamAuth},
		{nombre: "servidor sin TLS", modo: "scram", ssl: 'N', pass: pgMarca, code: "08006", motivo: ReasonUpstreamTLS},
		{nombre: "bytes inyectados tras la S", modo: "scram", inyecta: []byte("R\x00\x00\x00\x08\x00\x00\x00\x00"), pass: pgMarca, code: "08006", motivo: ReasonUpstreamTLS},
		{nombre: "CA que no es la del servidor", modo: "scram", pass: pgMarca, mod: func(c *Credential) {
			_, otra := certPG(t, pgDominio)
			c.CAPEM = otra
		}, code: "08006", motivo: ReasonUpstreamTLS},
		{nombre: "certificado de otro nombre", modo: "scram", pass: pgMarca, mod: func(c *Credential) {
			c.Domain = "otro.example.com"
		}, code: "08006", motivo: ReasonUpstreamTLS},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			srv := nuevoServidorPG(t, c.modo)
			if c.ssl != 0 {
				srv.ssl = c.ssl
			}
			srv.inyecta = c.inyecta
			e := proxyPG(t, srv, c.mod)
			k := conectarPG(t, e.addr)
			pares := c.pares
			if pares == nil {
				pares = []string{"user", pgUser, "database", pgDB}
			}
			var tipo byte
			var msg []byte
			if c.sinClave {
				k.arranque(0, pares...)
				tipo, msg = k.leer()
			} else {
				tipo, msg = k.login(c.pass, pares...)
			}
			code, crudo := k.esperarError(tipo, msg)
			if code != c.code {
				t.Errorf("SQLSTATE %q, quería %q (%q)", code, c.code, crudo)
			}
			if strings.Contains(crudo, "SECRETO-DEL-SERVIDOR") || strings.Contains(crudo, "bad proof") {
				t.Errorf("el texto del servidor llegó al invitado: %q", crudo)
			}
			if c.sinServidor && srv.conns.Load() != 0 {
				t.Errorf("el proxy conectó al servidor %d veces", srv.conns.Load())
			}
			k.c.Close()
			recs, crudoReg := e.registro(t)
			if len(recs) != 1 || recs[0].Reason != c.motivo || recs[0].Denied != c.denied || recs[0].Auth != "" {
				t.Errorf("registro %+v, quería motivo %q denied %v", recs, c.motivo, c.denied)
			}
			for _, prohibido := range []string{pgClave, pgMarca, PlaceholderPrefix} {
				if strings.Contains(crudoReg, prohibido) {
					t.Errorf("el registro contiene %q: %s", prohibido, crudoReg)
				}
			}
		})
	}
}

// El tramo del invitado: SSLRequest y GSSENCRequest reciben 'N' (como mucho
// dos), un ClientHello directo se cierra, 3.2 con _pq_. recibe
// NegotiateProtocolVersion y sigue, un protocolo 2 se rechaza.
func TestPGTramoDelInvitado(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	addr := proxyPG(t, srv, nil).addr

	k := conectarPG(t, addr)
	for _, code := range []uint32{pgGSSENCRequest, pgSSLRequest} {
		k.c.Write(binary.BigEndian.AppendUint32([]byte{0, 0, 0, 8}, code))
		var b [1]byte
		if _, err := io.ReadFull(k.br, b[:]); err != nil || b[0] != 'N' {
			t.Fatalf("respuesta a %d: %q %v", code, b, err)
		}
	}
	k.arranque(2, "user", pgUser, "database", pgDB, "_pq_.algo", "1")
	tipo, msg := k.leer()
	if tipo != 'v' || binary.BigEndian.Uint32(msg) != 0 || binary.BigEndian.Uint32(msg[4:]) != 1 || !strings.Contains(string(msg), "_pq_.algo") {
		t.Fatalf("esperaba NegotiateProtocolVersion, llegó %q %q", tipo, msg)
	}
	if tipo, _ := k.leer(); tipo != 'R' {
		t.Fatalf("tras negociar esperaba la petición de contraseña, llegó %q", tipo)
	}
	k.c.Write(mensajePG('p', append([]byte(pgMarca), 0)))
	if tipo, msg := k.leer(); tipo != 'R' || binary.BigEndian.Uint32(msg) != 0 {
		t.Fatalf("esperaba AuthenticationOk, llegó %q", tipo)
	}
	srv.mu.Lock()
	if _, ok := srv.params["_pq_.algo"]; ok {
		t.Error("una opción _pq_. llegó al servidor")
	}
	srv.mu.Unlock()

	// Tres negociaciones: la tercera cierra.
	k = conectarPG(t, addr)
	for i := 0; i < 3; i++ {
		k.c.Write(binary.BigEndian.AppendUint32([]byte{0, 0, 0, 8}, pgSSLRequest))
	}
	b, _ := io.ReadAll(k.br)
	if string(b) != "NN" {
		t.Errorf("tres SSLRequest: %q", b)
	}

	// TLS directo: se cierra sin contestar.
	k = conectarPG(t, addr)
	k.c.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x10})
	if b, _ := io.ReadAll(k.br); len(b) != 0 {
		t.Errorf("TLS directo: %q", b)
	}

	// Protocolo 2.
	k = conectarPG(t, addr)
	k.c.Write([]byte{0, 0, 0, 8, 0, 2, 0, 0})
	tipo, msg = k.leer()
	if code, _ := k.esperarError(tipo, msg); code != "08P01" {
		t.Errorf("protocolo 2: %q", code)
	}
}

// CancelRequest con la clave falsa llega al servidor con la real; con una
// clave que el proxy no dio, se cierra sin conectar.
func TestPGCancelRequest(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	e := proxyPG(t, srv, nil)
	addr, p := e.addr, e.p
	k := conectarPG(t, addr)
	k.login(pgMarca)
	pid, falsa := k.hastaListo()
	k.c.Write(mensajePG('Q', []byte("SELECT pg_sleep(60)\x00")))
	time.Sleep(50 * time.Millisecond)

	cancelar := func(pid, clave uint32) {
		c := conectarPG(t, addr)
		m := binary.BigEndian.AppendUint32([]byte{0, 0, 0, 16}, pgCancelRequest)
		m = binary.BigEndian.AppendUint32(m, pid)
		m = binary.BigEndian.AppendUint32(m, clave)
		c.c.Write(m)
		io.ReadAll(c.br)
	}
	cancelar(pid, falsa^1)
	cancelar(pid, falsa)
	tipo, msg := k.leer()
	if tipo != 'E' || sqlstate(msg) != "57014" {
		t.Fatalf("la consulta no se canceló: %q %q", tipo, msg)
	}
	srv.mu.Lock()
	canc := append([][2]uint32(nil), srv.cancelado...)
	srv.mu.Unlock()
	if len(canc) != 1 || canc[0] != [2]uint32{42, 0xdeadbeef} {
		t.Fatalf("cancelaciones en el servidor: %x", canc)
	}
	k.c.Close()
	recs, _ := e.registro(t)
	var desconocida, hecha bool
	for _, r := range recs {
		if r.Method == "cancel" && r.Reason == ReasonUnknownCancel && r.Denied {
			desconocida = true
		}
		if r.Method == "cancel" && r.Reason == "" && r.Host == pgDominio {
			hecha = true
		}
	}
	if !desconocida || !hecha {
		t.Errorf("registros de cancelación: %+v", recs)
	}
	// Terminada la sesión, su clave falsa ya no vale.
	p.cancelMu.Lock()
	n := len(p.cancelaciones)
	p.cancelMu.Unlock()
	if n != 0 {
		t.Errorf("quedan %d claves de cancelación tras cerrar la sesión", n)
	}
}

// Proxy inactivo (fuera de allowlist) y sin hueco: error propio y registro.
func TestPGInactivoYOcupado(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	e := proxyPG(t, srv, nil, func(o *Options) { o.Enabled = func() bool { return false } })
	if e.p.PGActivo() {
		t.Error("PGActivo con el proxy inactivo")
	}
	k := conectarPG(t, e.addr)
	tipo, msg := k.leer()
	if code, _ := k.esperarError(tipo, msg); code != "28000" {
		t.Errorf("inactivo: %q", code)
	}
	k.c.Close()
	recs, _ := e.registro(t)
	if len(recs) != 1 || recs[0].Reason != ReasonDisabled || !recs[0].Denied {
		t.Errorf("registro %+v", recs)
	}

	e = proxyPG(t, srv, nil)
	addr, p := e.addr, e.p
	if !p.PGActivo() {
		t.Error("PGActivo falso con una credencial Postgres")
	}
	for i := 0; i < MaxPGConns; i++ {
		p.pgSem <- struct{}{}
	}
	k = conectarPG(t, addr)
	tipo, msg = k.leer()
	if code, _ := k.esperarError(tipo, msg); code != "53300" {
		t.Errorf("ocupado: %q", code)
	}
	for i := 0; i < MaxPGConns; i++ {
		<-p.pgSem
	}
}

// El invitado que no termina el arranque se corta a pgPre.
func TestPGPlazoAntesDeAutenticar(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	e := proxyPG(t, srv, nil, func(p *Proxy) { p.pgPre = 100 * time.Millisecond })
	k := conectarPG(t, e.addr)
	k.arranque(0, "user", pgUser, "database", pgDB)
	k.leer() // petición de contraseña; y no se contesta
	inicio := time.Now()
	if _, err := io.ReadAll(k.br); err != nil || time.Since(inicio) > 5*time.Second {
		t.Fatalf("el proxy no cortó a tiempo: %v %v", err, time.Since(inicio))
	}
	recs, _ := e.registro(t)
	if len(recs) != 1 || recs[0].Reason != ReasonTimeout {
		t.Errorf("registro %+v", recs)
	}
}

// Cerrar el PGServer corta una sesión autenticada y ociosa (no hay plazo de
// inactividad tras autenticar).
func TestPGServerCloseCortaSesiones(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	p := New(Options{DialPG: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, srv.ln.Addr().String())
	}})
	caSrv := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.cert.Certificate[0]}))
	if _, err := p.SetCredentials([]Credential{{Env: "PGPASSWORD", Domain: pgDominio, Placeholder: pgMarca,
		Secret: pgClave, Kind: KindPostgres, User: pgUser, Database: pgDB, CAPEM: caSrv}}); err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ps := NewPGServer(p)
	hecho := make(chan error, 1)
	go func() { hecho <- ps.Serve(ln) }()
	k := conectarPG(t, ln.Addr().String())
	k.login(pgMarca)
	k.hastaListo()
	if err := ps.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-hecho; err != nil {
		t.Errorf("Serve tras Close: %v", err)
	}
	if _, err := io.ReadAll(k.br); err != nil {
		t.Errorf("la sesión no se cerró limpia: %v", err)
	}
}

// Validación: lo que una credencial Postgres exige y lo que una HTTP no admite.
func TestValidarCredencialesPostgres(t *testing.T) {
	_, ca := certPG(t, pgDominio)
	base := func() Credential {
		return Credential{Env: "PGPASSWORD", Domain: pgDominio, Placeholder: pgMarca, Secret: pgClave,
			Kind: KindPostgres, User: pgUser}
	}
	c := []Credential{base()}
	if err := ValidarCredenciales(c); err != nil || c[0].Port != PGDefaultPort {
		t.Fatalf("válida: %v, puerto %d", err, c[0].Port)
	}
	for nombre, mod := range map[string]func(*Credential){
		"sin rol":          func(c *Credential) { c.User = "" },
		"rol largo":        func(c *Credential) { c.User = strings.Repeat("a", 64) },
		"rol con control":  func(c *Credential) { c.User = "a\x00b" },
		"base con control": func(c *Credential) { c.Database = "a\nb" },
		"puerto":           func(c *Credential) { c.Port = 70000 },
		"clave no ASCII":   func(c *Credential) { c.Secret = "contraseña" },
		"clave con salto":  func(c *Credential) { c.Secret = "a\nb" },
		"allow":            func(c *Credential) { c.Allow = []string{"GET /"} },
		"CA basura":        func(c *Credential) { c.CAPEM = "no es un PEM" },
		"CA enorme":        func(c *Credential) { c.CAPEM = ca + strings.Repeat(" ", MaxCAPEM) },
		"tipo raro":        func(c *Credential) { c.Kind = "mysql" },
		"http con rol":     func(c *Credential) { c.Kind = ""; c.Port = 0 },
	} {
		c := base()
		mod(&c)
		if err := ValidarCredenciales([]Credential{c}); err == nil {
			t.Errorf("%s: aceptada", nombre)
		}
	}
	c = []Credential{base()}
	c[0].CAPEM = ca
	c[0].Database = pgDB
	if err := ValidarCredenciales(c); err != nil {
		t.Errorf("con CA y base: %v", err)
	}
}

// Una credencial Postgres no la usa el proxy HTTP (su dominio sin
// credencial HTTP da 403), pero su dominio sí se devuelve para desviarlo.
func TestPGNoSeUsaEnHTTP(t *testing.T) {
	p := New(Options{Transport: roundTripFunc(eco)})
	defer p.Close()
	doms, err := p.SetCredentials([]Credential{
		{Env: "PGPASSWORD", Domain: pgDominio, Placeholder: pgMarca, Secret: pgClave, Kind: KindPostgres, User: pgUser},
		{Env: "API", Domain: "api.example.com", Placeholder: PlaceholderPrefix + "api", Secret: "k"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(doms, ",") != "api.example.com,"+pgDominio {
		t.Errorf("dominios %v", doms)
	}
	req := httptest.NewRequest("GET", "http://"+pgDominio+"/", nil)
	req.Header.Set("Authorization", "Bearer "+pgMarca)
	if rec := servir(p, req); rec.Code != 403 {
		t.Errorf("HTTP a un dominio solo Postgres: %d", rec.Code)
	}
}
