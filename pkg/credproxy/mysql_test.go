package credproxy

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests del proxy de MySQL contra un servidor falso que habla el protocolo
// 4.1 (saludo, SSLRequest + TLS, native, caching_sha2 rápida y completa,
// AuthSwitch, ERR), escrito aquí e independiente del lado cliente del proxy.

const (
	myDominio = "mysql.example.com"
	myUser    = "app"
	myDB      = "appdb"
	myClave   = "s3cr3t-real-mysql-password"
	myMarca   = PlaceholderPrefix + "fedcba9876543210"
	// capsServidor son las de un MySQL 8 normal (con TLS).
	capsServidor = myOfrecidas | mySSL | myConnectAttrs | myLocalFiles | myCompress
)

// servidorMy es un MySQL de mentira.
type servidorMy struct {
	t    *testing.T
	ln   net.Listener
	cert tls.Certificate
	// modo: native, sha2 (ruta rápida), sha2-full (pide la completa),
	// switch-native (saluda con sha2 y cambia a native), clear (cambia a
	// mysql_clear_password), ed25519 (cambia a un plugin no admitido), error
	// (ERR 1045 con texto), saludo-error (ERR en lugar del saludo).
	modo string
	caps uint32

	mu        sync.Mutex
	user, db  string
	plugin    string
	capsVisto uint32
	claveVis  []string // lo que llegó en claro (full auth o clear)
	consultas []string
	tlsVisto  atomic.Bool
	autentic  atomic.Int32
	conns     atomic.Int32
}

// nuevoServidorMy arranca el servidor falso sin las capacidades quitar.
func nuevoServidorMy(t *testing.T, modo string, quitar ...uint32) *servidorMy {
	t.Helper()
	caps := capsServidor
	for _, q := range quitar {
		caps &^= q
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := certPG(t, myDominio)
	s := &servidorMy{t: t, ln: ln, cert: cert, modo: modo, caps: caps}
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

func (s *servidorMy) saludo(nonce []byte, plugin string) []byte {
	b := []byte{10}
	b = append(append(b, "8.0.36-falso"...), 0)
	b = binary.LittleEndian.AppendUint32(b, 4242)
	b = append(b, nonce[:8]...)
	b = append(b, 0)
	b = binary.LittleEndian.AppendUint16(b, uint16(s.caps&0xffff))
	b = append(b, 255)
	b = binary.LittleEndian.AppendUint16(b, 2)
	b = binary.LittleEndian.AppendUint16(b, uint16(s.caps>>16))
	b = append(b, 21)
	b = append(b, make([]byte, 10)...)
	b = append(append(b, nonce[8:]...), 0)
	return append(append(b, plugin...), 0)
}

func okMy() []byte { return []byte{0, 0, 0, 2, 0, 0, 0} }

func (s *servidorMy) atender(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if s.modo == "saludo-error" {
		c.Write(paqueteMy(0, append([]byte{0xff, 0x10, 0x04}, "Too many connections SECRETO-DEL-SERVIDOR"...)))
		return
	}
	nonce, _ := nonceMy()
	plugin := pluginNative
	if strings.HasPrefix(s.modo, "sha2") || s.modo == "switch-native" {
		plugin = pluginSHA2
	}
	c.Write(paqueteMy(0, s.saludo(nonce, plugin)))
	var conn net.Conn = c
	seq, b, err := leerPaqueteMy(conn, 1<<16)
	if err != nil {
		return
	}
	caps := binary.LittleEndian.Uint32(b)
	if caps&mySSL != 0 && len(b) == 32 {
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{s.cert}})
		if err := tc.Handshake(); err != nil {
			return
		}
		s.tlsVisto.Store(true)
		conn = tc
		if seq, b, err = leerPaqueteMy(conn, 1<<16); err != nil {
			return
		}
	}
	r, err := parsearRespuestaServidor(b)
	if err != nil {
		s.t.Errorf("servidor falso: respuesta: %v", err)
		return
	}
	s.mu.Lock()
	s.user, s.db, s.plugin, s.capsVisto = r.user, r.db, r.plugin, caps
	s.mu.Unlock()
	enviar := func(p []byte) {
		seq++
		conn.Write(paqueteMy(seq, p))
	}
	leer := func() []byte {
		var p []byte
		seq, p, err = leerPaqueteMy(conn, 1<<16)
		if err != nil {
			return nil
		}
		return p
	}
	claro := func(p []byte) {
		s.mu.Lock()
		s.claveVis = append(s.claveVis, string(bytes.TrimSuffix(p, []byte{0})))
		s.mu.Unlock()
	}
	denegar := func() {
		enviar(errMy(1045, "28000", "Access denied for user 'app' SECRETO-DEL-SERVIDOR"))
	}
	switch s.modo {
	case "error":
		denegar()
		return
	case "native":
		if !bytes.Equal(r.auth, scrambleNative(nonce, myClave)) {
			denegar()
			return
		}
	case "sha2", "sha2-full":
		if !bytes.Equal(r.auth, scrambleSHA2(nonce, myClave)) {
			denegar()
			return
		}
		if s.modo == "sha2" {
			enviar([]byte{1, 3})
			break
		}
		enviar([]byte{1, 4})
		p := leer()
		if p == nil {
			return
		}
		claro(p)
		if string(bytes.TrimSuffix(p, []byte{0})) != myClave {
			denegar()
			return
		}
	case "switch-native", "clear", "ed25519":
		nuevo := map[string]string{"switch-native": pluginNative, "clear": pluginLimpio, "ed25519": "client_ed25519"}[s.modo]
		n2, _ := nonceMy()
		enviar(append(append(append([]byte{0xfe}, nuevo...), 0), append(n2, 0)...))
		p := leer()
		if p == nil {
			return
		}
		switch nuevo {
		case pluginNative:
			if !bytes.Equal(p, scrambleNative(n2, myClave)) {
				denegar()
				return
			}
		case pluginLimpio:
			claro(p)
			if string(bytes.TrimSuffix(p, []byte{0})) != myClave {
				denegar()
				return
			}
		default:
			denegar()
			return
		}
	}
	s.autentic.Add(1)
	enviar(okMy())
	// Órdenes: COM_QUERY devuelve un OK con el texto de la consulta como info
	// (así se ve lo que llegó); COM_QUIT cierra.
	for {
		p := leer()
		if len(p) == 0 {
			return
		}
		switch p[0] {
		case 0x01:
			return
		case 0x03:
			s.mu.Lock()
			s.consultas = append(s.consultas, string(p[1:]))
			s.mu.Unlock()
			seq = 0
			ok := append([]byte{0, 0, 0, 2, 0, 0, 0}, p[1:]...)
			conn.Write(paqueteMy(1, ok))
		}
	}
}

// parsearRespuestaServidor es el lado servidor de la respuesta: la misma
// estructura, pero sin las exigencias del proxy (aquí sí llega CLIENT_SSL).
func parsearRespuestaServidor(b []byte) (respuestaMy, error) {
	caps := binary.LittleEndian.Uint32(b)
	r, err := parsearRespuestaMy(append(binary.LittleEndian.AppendUint32(nil, caps&^mySSL), b[4:]...))
	return r, err
}

// entornoMy es un proxy con su servidor (DestPort 0, como el listener
// genérico de Linux) y su registro.
type entornoMy struct {
	addr  string
	p     *Proxy
	ps    *PGServer
	audit string
}

func (e *entornoMy) registro(t *testing.T) ([]Record, string) {
	t.Helper()
	_ = e.ps.Close()
	return leerRegistro(t, e.p, e.audit)
}

// proxyMy monta un proxy con la credencial MySQL. opts son func(*Options)
// (antes de crearlo) o func(*Proxy) (antes de servir).
func proxyMy(t *testing.T, srv *servidorMy, mod func(*Credential), opts ...any) *entornoMy {
	t.Helper()
	e := &entornoMy{audit: filepath.Join(t.TempDir(), AuditFile)}
	o := Options{AuditPath: e.audit, DialPG: func(ctx context.Context, network, _ string) (net.Conn, error) {
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
	ca := ""
	if srv != nil {
		ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.cert.Certificate[0]}))
	}
	cred := Credential{Env: "MYSQL_PWD", Domain: myDominio, Placeholder: myMarca, Secret: myClave,
		Kind: KindMySQL, User: myUser, Database: myDB, CAPEM: ca}
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

// clienteMy es un cliente mínimo del lado del invitado.
type clienteMy struct {
	t     *testing.T
	c     net.Conn
	nonce []byte
	seq   byte
}

func conectarMy(t *testing.T, addr string) *clienteMy {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	t.Cleanup(func() { c.Close() })
	return &clienteMy{t: t, c: c}
}

func (k *clienteMy) leer() []byte {
	k.t.Helper()
	seq, p, err := leerPaqueteMy(k.c, 1<<20)
	if err != nil {
		k.t.Fatalf("leyendo del proxy: %v", err)
	}
	k.seq = seq
	return p
}

// saludo lee el saludo del proxy y guarda su nonce.
func (k *clienteMy) saludo() saludoServidorMy {
	k.t.Helper()
	sv, err := parsearSaludoMy(k.leer())
	if err != nil {
		k.t.Fatalf("saludo del proxy: %v", err)
	}
	k.nonce = sv.nonce
	return sv
}

// responder manda la respuesta al saludo con esas capacidades, plugin y
// prueba de pass.
func (k *clienteMy) responder(caps uint32, user, db, plugin, pass string) {
	var auth []byte
	switch plugin {
	case pluginSHA2:
		auth = scrambleSHA2(k.nonce, pass)
	case pluginNative:
		auth = scrambleNative(k.nonce, pass)
	default:
		auth = append([]byte(pass), 0)
	}
	b := binary.LittleEndian.AppendUint32(nil, caps)
	b = binary.LittleEndian.AppendUint32(b, 1<<24)
	b = append(b, 255)
	b = append(b, make([]byte, 23)...)
	b = append(append(b, user...), 0)
	b = append(append(b, byte(len(auth))), auth...)
	if caps&myConnectWithDB != 0 {
		b = append(append(b, db...), 0)
	}
	if caps&myPluginAuth != 0 {
		b = append(append(b, plugin...), 0)
	}
	k.c.Write(paqueteMy(k.seq+1, b))
}

// capsCliente son las de un cliente MySQL 8 típico (pide LOCAL_FILES y
// atributos, que el proxy no ofrece).
const capsCliente = myLongPassword | myFoundRows | myLongFlag | myConnectWithDB | myLocalFiles |
	myProtocol41 | myTransactions | mySecureConn | myMultiStatements | myMultiResults |
	myPSMultiResults | myPluginAuth | myConnectAttrs | mySessionTrack | myDeprecateEOF

// login hace saludo y respuesta estándar y devuelve el primer paquete.
func (k *clienteMy) login(plugin, pass string) []byte {
	k.t.Helper()
	k.saludo()
	k.responder(capsCliente, myUser, myDB, plugin, pass)
	return k.leer()
}

func (k *clienteMy) consulta(q string) []byte {
	k.c.Write(paqueteMy(0, append([]byte{0x03}, q...)))
	return k.leer()
}

// esperarErr comprueba que p es un ERR y devuelve código, SQLSTATE y texto.
func esperarErr(t *testing.T, p []byte) (uint16, string, string) {
	t.Helper()
	if len(p) < 9 || p[0] != 0xff || p[3] != '#' {
		t.Fatalf("esperaba un ERR, llegó %q", p)
	}
	return binary.LittleEndian.Uint16(p[1:3]), string(p[4:9]), string(p[9:])
}

// Camino feliz con native y TLS: el servidor ve el usuario y la base, recibe
// la clave real por scramble dentro del TLS, las consultas van y vuelven sin
// que se sustituya nada, y el registro dice lo justo.
func TestMyNativeTLSConsultaYRegistro(t *testing.T) {
	srv := nuevoServidorMy(t, "native")
	e := proxyMy(t, srv, nil)
	k := conectarMy(t, e.addr)
	sv := k.saludo()
	if sv.plugin != pluginNative || sv.caps&mySSL != 0 || sv.caps&myLocalFiles != 0 {
		t.Fatalf("saludo del proxy: plugin %q caps %#x", sv.plugin, sv.caps)
	}
	k.responder(capsCliente, myUser, myDB, pluginNative, myMarca)
	if p := k.leer(); p[0] != 0 {
		t.Fatalf("esperaba OK, llegó %q", p)
	}
	q := "SELECT '" + myMarca + "'"
	if p := k.consulta(q); !bytes.Contains(p, []byte(myMarca)) {
		t.Fatalf("la consulta no volvió tal cual: %q", p)
	}
	k.c.Write(paqueteMy(0, []byte{0x01}))
	k.c.Close()
	recs, crudo := e.registro(t)
	if !srv.tlsVisto.Load() {
		t.Error("el servidor no vio TLS")
	}
	srv.mu.Lock()
	user, db, caps, consultas := srv.user, srv.db, srv.capsVisto, srv.consultas
	srv.mu.Unlock()
	if user != myUser || db != myDB {
		t.Errorf("el servidor vio %q@%q", user, db)
	}
	if caps&(myLocalFiles|myConnectAttrs|myCompress) != 0 || caps&mySSL == 0 {
		t.Errorf("capacidades hacia el servidor %#x", caps)
	}
	if len(consultas) != 1 || consultas[0] != q {
		t.Errorf("consultas %q", consultas)
	}
	if len(recs) != 1 {
		t.Fatalf("registro %+v", recs)
	}
	r := recs[0]
	if r.Kind != KindMySQL || r.Reason != "" || r.Auth != AuthMySQLNative || r.User != myUser ||
		r.Database != myDB || r.Host != myDominio || len(r.Creds) != 1 || r.Creds[0] != "MYSQL_PWD" {
		t.Errorf("registro %+v", r)
	}
	if strings.Contains(crudo, myClave) || strings.Contains(crudo, myMarca) {
		t.Errorf("el registro lleva la clave o el marcador: %s", crudo)
	}
}

// El invitado habla caching_sha2 (ruta rápida sobre el marcador) y el
// servidor pide la autenticación completa: el proxy manda la clave en claro
// DENTRO del TLS, y al invitado le llega "ruta rápida vale" y el OK.
func TestMySHA2CompletaDentroDelTLS(t *testing.T) {
	srv := nuevoServidorMy(t, "sha2-full")
	e := proxyMy(t, srv, nil)
	k := conectarMy(t, e.addr)
	p := k.login(pluginSHA2, myMarca)
	if !bytes.Equal(p, []byte{1, 3}) {
		t.Fatalf("esperaba la ruta rápida, llegó %q", p)
	}
	if p = k.leer(); p[0] != 0 {
		t.Fatalf("esperaba OK, llegó %q", p)
	}
	srv.mu.Lock()
	vis := srv.claveVis
	srv.mu.Unlock()
	if len(vis) != 1 || vis[0] != myClave || !srv.tlsVisto.Load() {
		t.Fatalf("clave vista %q (tls %v)", vis, srv.tlsVisto.Load())
	}
	k.c.Close()
	recs, _ := e.registro(t)
	if len(recs) != 1 || recs[0].Auth != AuthCachingSHA2 {
		t.Errorf("registro %+v", recs)
	}
}

// Ruta rápida del servidor y cambio de plugin: los dos terminan en OK.
func TestMySHA2RapidaYCambio(t *testing.T) {
	for modo, auth := range map[string]string{"sha2": AuthCachingSHA2Fast, "switch-native": AuthMySQLNative, "clear": AuthMySQLClear} {
		t.Run(modo, func(t *testing.T) {
			srv := nuevoServidorMy(t, modo)
			e := proxyMy(t, srv, nil)
			k := conectarMy(t, e.addr)
			if p := k.login(pluginNative, myMarca); p[0] != 0 {
				t.Fatalf("esperaba OK, llegó %q", p)
			}
			k.c.Close()
			recs, _ := e.registro(t)
			if len(recs) != 1 || recs[0].Auth != auth || recs[0].Reason != "" {
				t.Errorf("registro %+v", recs)
			}
		})
	}
}

// Un cliente con otro plugin (mysql_clear_password en el tramo del invitado,
// por ejemplo) recibe un AuthSwitchRequest a native con el mismo nonce.
func TestMyCambioDePluginDelInvitado(t *testing.T) {
	srv := nuevoServidorMy(t, "native")
	e := proxyMy(t, srv, nil)
	k := conectarMy(t, e.addr)
	k.saludo()
	k.responder(capsCliente, myUser, myDB, "mysql_clear_password", myMarca)
	p := k.leer()
	if p[0] != 0xfe {
		t.Fatalf("esperaba AuthSwitchRequest, llegó %q", p)
	}
	nombre, datos, _ := cadenaMy(p[1:])
	if nombre != pluginNative || !bytes.Equal(sinNulFinal(datos), k.nonce) {
		t.Fatalf("cambio a %q con nonce %q", nombre, datos)
	}
	k.c.Write(paqueteMy(k.seq+1, scrambleNative(k.nonce, myMarca)))
	if p = k.leer(); p[0] != 0 {
		t.Fatalf("esperaba OK, llegó %q", p)
	}
}

// Rechazos del lado del invitado y del servidor: ni la clave ni el texto del
// servidor llegan nunca al invitado, y el servidor no recibe la clave de un
// invitado que no probó el marcador.
func TestMyRechazos(t *testing.T) {
	casos := []struct {
		nombre  string
		modo    string
		mod     func(*Credential)
		login   func(k *clienteMy) []byte
		code    uint16
		motivo  string
		denied  bool
		llegaSv bool
	}{
		{nombre: "marcador malo", modo: "native", login: func(k *clienteMy) []byte { return k.login(pluginNative, "otra") },
			code: 1045, motivo: ReasonBadPlaceholder, denied: true},
		{nombre: "clave real como marcador", modo: "native", login: func(k *clienteMy) []byte { return k.login(pluginNative, myClave) },
			code: 1045, motivo: ReasonBadPlaceholder, denied: true},
		{nombre: "usuario", modo: "native", login: func(k *clienteMy) []byte {
			k.saludo()
			k.responder(capsCliente, "root", myDB, pluginNative, myMarca)
			return k.leer()
		}, code: 1045, motivo: ReasonUserMismatch, denied: true},
		{nombre: "base", modo: "native", login: func(k *clienteMy) []byte {
			k.saludo()
			k.responder(capsCliente, myUser, "mysql", pluginNative, myMarca)
			return k.leer()
		}, code: 1044, motivo: ReasonDatabaseMismatch, denied: true},
		{nombre: "servidor rechaza", modo: "error", login: func(k *clienteMy) []byte { return k.login(pluginNative, myMarca) },
			code: 1045, motivo: ReasonUpstreamAuth, llegaSv: true},
		{nombre: "saludo con error", modo: "saludo-error", login: func(k *clienteMy) []byte { return k.login(pluginNative, myMarca) },
			code: 2003, motivo: ReasonUpstreamAuth, llegaSv: true},
		{nombre: "plugin no admitido", modo: "ed25519", login: func(k *clienteMy) []byte { return k.login(pluginNative, myMarca) },
			code: 1045, motivo: ReasonUpstreamAuth, llegaSv: true},
		{nombre: "sin TLS en el servidor", modo: "native", mod: func(c *Credential) {}, login: func(k *clienteMy) []byte { return k.login(pluginNative, myMarca) },
			code: 2003, motivo: ReasonUpstreamTLS, llegaSv: true},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			var quitar uint32
			if c.nombre == "sin TLS en el servidor" {
				quitar = mySSL
			}
			srv := nuevoServidorMy(t, c.modo, quitar)
			e := proxyMy(t, srv, c.mod)
			k := conectarMy(t, e.addr)
			code, _, msg := esperarErr(t, c.login(k))
			if code != c.code {
				t.Errorf("código %d, quería %d (%s)", code, c.code, msg)
			}
			if strings.Contains(msg, "SECRETO") || strings.Contains(msg, myClave) {
				t.Errorf("el invitado ve texto del servidor o la clave: %q", msg)
			}
			k.c.Close()
			recs, _ := e.registro(t)
			if len(recs) != 1 || recs[0].Reason != c.motivo || recs[0].Denied != c.denied {
				t.Errorf("registro %+v", recs)
			}
			if got := srv.conns.Load() > 0; got != c.llegaSv {
				t.Errorf("el servidor recibió conexión: %v", got)
			}
			if srv.autentic.Load() != 0 && c.nombre != "servidor rechaza" {
				t.Errorf("el servidor autenticó")
			}
			srv.mu.Lock()
			vis := srv.claveVis
			srv.mu.Unlock()
			if len(vis) != 0 {
				t.Errorf("la clave llegó en claro: %q", vis)
			}
		})
	}
}

// Sin TLS (UpstreamTLS disable, solo con Upstream): native y la ruta rápida
// valen; la autenticación completa y mysql_clear_password se rechazan sin
// mandar la clave.
func TestMyUpstreamSinTLS(t *testing.T) {
	for modo, bien := range map[string]bool{"native": true, "sha2": true, "sha2-full": false, "clear": false} {
		t.Run(modo, func(t *testing.T) {
			srv := nuevoServidorMy(t, modo, mySSL)
			e := proxyMy(t, srv, func(c *Credential) {
				c.Upstream, c.UpstreamTLS, c.CAPEM = srv.ln.Addr().String(), UpstreamTLSDisable, ""
			})
			k := conectarMy(t, e.addr)
			p := k.login(pluginNative, myMarca)
			if bien != (p[0] == 0) {
				t.Fatalf("respuesta %q", p)
			}
			if !bien {
				esperarErr(t, p)
			}
			k.c.Close()
			recs, _ := e.registro(t)
			if len(recs) != 1 || (bien && recs[0].Reason != "") || (!bien && recs[0].Reason != ReasonUpstreamAuth) ||
				recs[0].Upstream != srv.ln.Addr().String() {
				t.Errorf("registro %+v", recs)
			}
			srv.mu.Lock()
			vis := srv.claveVis
			srv.mu.Unlock()
			if len(vis) != 0 {
				t.Errorf("sin TLS la clave cruzó en claro: %q", vis)
			}
		})
	}
}

// El tramo del invitado no habla TLS: un cliente que pide SSL se rechaza. Y
// el servidor que no tiene una capacidad de formato que eligió el invitado
// no recibe la clave.
func TestMyTLSDelInvitadoYCapacidades(t *testing.T) {
	srv := nuevoServidorMy(t, "native")
	e := proxyMy(t, srv, nil)
	k := conectarMy(t, e.addr)
	k.saludo()
	b := binary.LittleEndian.AppendUint32(nil, capsCliente|mySSL)
	b = binary.LittleEndian.AppendUint32(b, 1<<24)
	b = append(append(b, 255), make([]byte, 23)...)
	k.c.Write(paqueteMy(1, b))
	if code, _, _ := esperarErr(t, k.leer()); code != 1043 {
		t.Errorf("SSLRequest: código %d", code)
	}

	srv2 := nuevoServidorMy(t, "native", myDeprecateEOF)
	e2 := proxyMy(t, srv2, nil)
	k2 := conectarMy(t, e2.addr)
	if code, _, _ := esperarErr(t, k2.login(pluginNative, myMarca)); code != 2003 {
		t.Errorf("capacidades: código %d", code)
	}
	k2.c.Close()
	recs, _ := e2.registro(t)
	if len(recs) != 1 || recs[0].Reason != ReasonCapabilities {
		t.Errorf("registro %+v", recs)
	}
	if srv2.autentic.Load() != 0 {
		t.Error("el servidor autenticó")
	}
	e.registro(t)
}

// ServeDB: con credenciales de los dos tipos, MySQL es el 3306 y lo demás
// Postgres; con uno solo, ese para todo. Y la validación que lo sostiene.
func TestServeDBDespacho(t *testing.T) {
	p := New(Options{})
	pg := Credential{Env: "PGPASSWORD", Domain: pgDominio, Placeholder: pgMarca, Secret: pgClave,
		Kind: KindPostgres, User: pgUser, Database: pgDB}
	my := Credential{Env: "MYSQL_PWD", Domain: myDominio, Placeholder: myMarca, Secret: myClave,
		Kind: KindMySQL, User: myUser, Database: myDB}
	habla := func(port int) string {
		a, b := net.Pipe()
		defer a.Close()
		go p.ServeDB(context.Background(), b, port)
		_ = a.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		buf := make([]byte, 5)
		n, _ := io.ReadFull(a, buf)
		if n >= 5 && buf[4] == 10 {
			return "mysql"
		}
		return "postgres"
	}
	if _, err := p.SetCredentials([]Credential{my}); err != nil {
		t.Fatal(err)
	}
	if habla(0) != "mysql" || habla(5432) != "mysql" {
		t.Error("solo MySQL: todo debería ser MySQL")
	}
	if _, err := p.SetCredentials([]Credential{pg, my}); err != nil {
		t.Fatal(err)
	}
	if habla(0) != "postgres" || habla(5432) != "postgres" || habla(3306) != "mysql" {
		t.Error("los dos tipos: el 3306 es MySQL y lo demás Postgres")
	}
	my2 := my
	my2.Port = 3307
	if _, err := p.SetCredentials([]Credential{pg, my2}); err == nil {
		t.Error("MySQL fuera del 3306 junto a Postgres debería rechazarse")
	}
	pg2 := pg
	pg2.Port = 3306
	if _, err := p.SetCredentials([]Credential{pg2, my}); err == nil {
		t.Error("Postgres en el 3306 junto a MySQL debería rechazarse")
	}
	if _, err := p.SetCredentials([]Credential{my2}); err != nil {
		t.Errorf("solo MySQL en el 3307: %v", err)
	}
}

// Validación de una credencial MySQL.
func TestValidarCredencialesMySQL(t *testing.T) {
	base := func() Credential {
		return Credential{Env: "MYSQL_PWD", Domain: myDominio, Placeholder: myMarca, Secret: myClave,
			Kind: KindMySQL, User: myUser, Database: myDB}
	}
	c := base()
	if err := ValidarCredenciales([]Credential{c}); err != nil {
		t.Fatal(err)
	}
	cs := []Credential{c}
	_ = ValidarCredenciales(cs)
	if cs[0].Port != MySQLDefaultPort {
		t.Errorf("puerto por defecto %d", cs[0].Port)
	}
	malas := map[string]func(*Credential){
		"sin usuario":          func(c *Credential) { c.User = "" },
		"sin base":             func(c *Credential) { c.Database = "" },
		"base y cualquiera":    func(c *Credential) { c.AnyDatabase = true },
		"usuario con NUL":      func(c *Credential) { c.User = "a\x00b" },
		"clave no ASCII":       func(c *Credential) { c.Secret = "clave\n" },
		"allow":                func(c *Credential) { c.Allow = []string{"GET /"} },
		"puerto 80":            func(c *Credential) { c.Port = 80 },
		"máquina":              func(c *Credential) { c.UpstreamMachine = "0123456789abcdef" },
		"disable sin upstream": func(c *Credential) { c.UpstreamTLS = UpstreamTLSDisable },
		"CA mala":              func(c *Credential) { c.CAPEM = "no es un PEM" },
	}
	for nombre, f := range malas {
		c := base()
		f(&c)
		if err := ValidarCredenciales([]Credential{c}); err == nil {
			t.Errorf("%s: debería rechazarse", nombre)
		}
	}
	c = base()
	c.Database, c.AnyDatabase = "", true
	if err := ValidarCredenciales([]Credential{c}); err != nil {
		t.Errorf("any-database: %v", err)
	}
}

// Sin credencial MySQL (una máquina con solo HTTP), ServeMySQL cierra sin
// mandar el saludo; inactivo, un ERR propio; lleno, 1040.
func TestMyInactivoSinCredencialYOcupado(t *testing.T) {
	p := New(Options{})
	a, b := net.Pipe()
	go p.ServeMySQL(context.Background(), b)
	_ = a.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _ := io.ReadAll(a); len(n) != 0 {
		t.Errorf("sin credencial MySQL mandó %q", n)
	}

	srv := nuevoServidorMy(t, "native")
	e := proxyMy(t, srv, nil)
	for i := 0; i < MaxMySQLConns; i++ {
		e.p.mySem <- struct{}{}
	}
	k := conectarMy(t, e.addr)
	if code, _, _ := esperarErr(t, k.leer()); code != 1040 {
		t.Errorf("ocupado: %d", code)
	}
	for i := 0; i < MaxMySQLConns; i++ {
		<-e.p.mySem
	}

	e2 := proxyMy(t, srv, nil, func(o *Options) { o.Enabled = func() bool { return false } })
	k2 := conectarMy(t, e2.addr)
	if code, _, _ := esperarErr(t, k2.leer()); code != 1045 {
		t.Errorf("inactivo: %d", code)
	}
	k2.c.Close()
	recs, _ := e2.registro(t)
	if len(recs) != 1 || recs[0].Reason != ReasonDisabled || !recs[0].Denied {
		t.Errorf("registro %+v", recs)
	}
}

// El invitado que no contesta al saludo se corta a pgPre.
func TestMyPlazoAntesDeAutenticar(t *testing.T) {
	srv := nuevoServidorMy(t, "native")
	e := proxyMy(t, srv, nil, func(p *Proxy) { p.pgPre = 100 * time.Millisecond })
	k := conectarMy(t, e.addr)
	k.saludo()
	inicio := time.Now()
	if _, err := io.ReadAll(k.c); err != nil || time.Since(inicio) > 5*time.Second {
		t.Fatalf("el proxy no cortó a tiempo: %v %v", err, time.Since(inicio))
	}
	recs, _ := e.registro(t)
	if len(recs) != 1 || recs[0].Reason != ReasonTimeout {
		t.Errorf("registro %+v", recs)
	}
}

// Los scrambles contra valores calculados a mano con la definición del
// protocolo, y el saludo del proxy se lee con el analizador de saludos.
func TestMyScramblesYSaludo(t *testing.T) {
	nonce := []byte("abcdefghijklmnopqrst")
	if got := scrambleNative(nonce, ""); len(got) != 20 {
		t.Errorf("native: %d bytes", len(got))
	}
	// Propiedad del protocolo: el servidor, que guarda SHA1(SHA1(clave)),
	// recupera SHA1(clave) del scramble y comprueba su hash.
	s := scrambleNative(nonce, "pw")
	s2 := sha1Sum(sha1Sum([]byte("pw")))
	x := sha1Sum(append(append([]byte(nil), nonce...), s2...))
	for i := range x {
		x[i] ^= s[i]
	}
	if !bytes.Equal(sha1Sum(x), s2) {
		t.Error("scrambleNative no cumple la comprobación del servidor")
	}
	s = scrambleSHA2(nonce, "pw")
	h2 := sha256Sum(sha256Sum([]byte("pw")))
	y := sha256Sum(append(append([]byte(nil), h2...), nonce...))
	for i := range y {
		y[i] ^= s[i]
	}
	if !bytes.Equal(sha256Sum(y), h2) {
		t.Error("scrambleSHA2 no cumple la comprobación del servidor")
	}
	n, _ := nonceMy()
	for _, c := range n {
		if c < 0x21 || c > 0x7e {
			t.Fatalf("nonce no imprimible: %q", n)
		}
	}
	sv, err := parsearSaludoMy(saludoMy(n))
	if err != nil || !bytes.Equal(sv.nonce, n) || sv.plugin != pluginNative || sv.caps != myOfrecidas {
		t.Fatalf("saludo: %+v %v", sv, err)
	}
}

func sha1Sum(b []byte) []byte {
	h := sha1.Sum(b)
	return h[:]
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
