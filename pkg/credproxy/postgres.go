package credproxy

// Proxy de credenciales para PostgreSQL: el mismo modelo que el HTTP (la clave
// real no entra en la microVM; el invitado tiene un marcador) sobre el
// protocolo de Postgres v3.
//
// EL TRAMO DEL INVITADO va en claro: el invitado conecta al puerto de Postgres
// del dominio con credencial, que su resolver contesta con la IP del proxy, y
// ahí no hay TLS (a un SSLRequest o GSSENCRequest se contesta 'N', como un
// servidor sin TLS; un ClientHello directo se cierra). Ese tramo no sale de la
// máquina: es el veth de su netns (Linux) o la pila de gVisor de su kling-vz
// (macOS). El proxy pide la contraseña en claro (AuthenticationCleartext
// Password), la compara en tiempo constante con los marcadores de las
// credenciales Postgres y así elige la credencial; después exige que el rol
// sea el de la credencial y, si la credencial fija base de datos, esa.
//
// EL TRAMO DEL SERVIDOR sale por el dialer de solo IPs públicas (dialPublico)
// o, si el operador fijó Upstream, a esa dirección (upstream.go: loopback y
// privadas sí; metadatos y la red de kindling nunca). Por defecto con TLS
// verificado contra el nombre de la credencial o TLSServerName (TLS 1.2+,
// raíces del sistema más la CA de la credencial si la trae). Un servidor que
// contesta 'N' al SSLRequest es un fallo, no un "entonces en claro": sin TLS
// solo se sale si la credencial lo pide (UpstreamTLS "disable", solo con
// Upstream), y entonces sin SSLRequest y solo con SCRAM-SHA-256. La
// respuesta al SSLRequest se lee byte a byte, sin buffer: lo que un
// intermediario inyecte detrás de la 'S' no puede colarse como si viniera
// dentro del TLS (CVE-2021-23214). La autenticación es SCRAM-SHA-256 (con
// -PLUS si el servidor lo ofrece; ver scram.go) o contraseña en claro, que va
// dentro de ese TLS verificado. MD5, GSS, SSPI y cualquier otro mecanismo SASL
// se rechazan.
//
// QUÉ VE EL INVITADO de la autenticación: AuthenticationOk solo cuando el
// servidor ha dado la suya (y, con SCRAM, tras comprobar su firma). Un error
// del servidor antes de eso NO se reenvía: el invitado recibe uno propio
// (28P01 o 08006, con el SQLSTATE del servidor como mucho) y el host lo anota
// sin texto. Tras la autenticación el flujo pasa tal cual en los dos sentidos
// —aquí NO se sustituye nada, ni el marcador ni la clave— salvo BackendKeyData,
// cuya clave de cancelación se cambia por una falsa: un CancelRequest del
// invitado con la falsa se traduce a la real en una conexión nueva (con el
// mismo TLS), y uno con una clave desconocida se cierra sin más.
//
// QUÉ NO RESUELVE: el invitado usa el rol con todos sus permisos (el proxy no
// mira el SQL). Lo que acota el daño es el rol: de solo lectura, con GRANT a lo
// justo, sin CREATEROLE. Tampoco evita que el invitado cambie la contraseña del
// rol con ALTER ROLE si el rol puede: la clave del proxy dejaría de valer.
//
// LÍMITES: MaxPGConns conexiones a la vez por máquina, arranque acotado a
// pgMaxStartup bytes, pgPreAuth para que el invitado mande arranque y
// contraseña y pgAuthTotal para toda la autenticación. Tras ella no hay plazo
// de inactividad (una conexión de un pool puede estar horas callada); hay
// keepalive TCP.
//
// REGISTRO: una línea por conexión (kind "postgres"): dominio, rol, base de
// datos, método de autenticación con el servidor, motivo si no llegó, bytes y
// duración. Nunca la clave, el marcador ni nada del SQL.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// KindPostgres es el tipo de una credencial de Postgres y el kind de sus
	// registros de auditoría.
	KindPostgres = "postgres"
	// MaxPGConns: conexiones a la vez al proxy de Postgres de una máquina.
	MaxPGConns = 32
	// MaxCAPEM: tamaño de la CA de una credencial.
	MaxCAPEM = 64 << 10
	// PGDefaultPort es el puerto del servidor si la credencial no dice otro.
	PGDefaultPort = 5432

	pgPreAuth   = 10 * time.Second
	pgAuthTotal = 15 * time.Second
	pgKeepAlive = 30 * time.Second

	pgMaxStartup  = 10000
	pgMaxPassword = 1024
	// pgMaxAuthMsg acota cada mensaje del servidor antes de la autenticación,
	// y pgMaxPreReady los de entre AuthenticationOk y ReadyForQuery (se leen
	// enteros para cambiar BackendKeyData).
	pgMaxAuthMsg  = 16 << 10
	pgMaxPreReady = 1 << 20
	pgMaxNombre   = 63

	pgSSLRequest    = 80877103
	pgGSSENCRequest = 80877104
	pgCancelRequest = 80877102
	pgProto30       = 3 << 16
)

// Motivos propios del proxy de Postgres (Record.Reason).
const (
	ReasonBadStartup       = "bad_startup"       // arranque o contraseña mal formados, o protocolo no soportado
	ReasonTimeout          = "timeout"           // el invitado no terminó el arranque a tiempo
	ReasonBadPlaceholder   = "bad_placeholder"   // la contraseña no es el marcador de ninguna credencial
	ReasonUserMismatch     = "user_mismatch"     // el rol no es el de la credencial
	ReasonDatabaseMismatch = "database_mismatch" // la base de datos no es la de la credencial
	ReasonReplication      = "replication"       // conexión de replicación: no se admite
	ReasonUpstreamTLS      = "upstream_tls"      // el servidor no habla TLS o su certificado no vale
	ReasonUpstreamAuth     = "upstream_auth"     // el servidor rechazó la credencial o pidió un método no admitido
	ReasonUnknownCancel    = "unknown_cancel"    // CancelRequest con una clave que el proxy no dio
)

// Métodos de autenticación con el servidor (Record.Auth).
const (
	AuthSCRAMPlus = "scram-sha-256-plus"
	AuthSCRAM     = "scram-sha-256"
	AuthPassword  = "password"
	AuthTrust     = "trust"
)

// credPG es una credencial de Postgres con su configuración TLS ya hecha.
type credPG struct {
	Credential
	tls *tls.Config
	// claro hace que el aviso de contraseña en claro salga una vez por
	// credencial (por juego de credenciales: una rotación lo repone).
	claro *sync.Once
}

func compilarPG(c Credential) (credPG, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if c.CAPEM != "" && !pool.AppendCertsFromPEM([]byte(c.CAPEM)) {
		return credPG{}, fmt.Errorf("credential for %s: the CA is not a PEM certificate", c.Domain)
	}
	nombre := c.Domain
	if c.TLSServerName != "" {
		nombre = c.TLSServerName
	}
	return credPG{Credential: c, tls: &tls.Config{
		ServerName: nombre,
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
	}, claro: new(sync.Once)}, nil
}

// validarPostgres: ver ValidarTipo.
func validarPostgres(c *Credential) error {
	d := c.Domain
	if len(c.Allow) > 0 {
		return fmt.Errorf("credential for %s: -allow-request is only for HTTP credentials", d)
	}
	if c.Port == 0 {
		c.Port = PGDefaultPort
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("credential for %s: port %d out of range", d, c.Port)
	}
	if c.User == "" {
		return fmt.Errorf("credential for %s: a postgres credential needs -user", d)
	}
	if err := validarNombrePG(c.User); err != nil {
		return fmt.Errorf("credential for %s: user: %w", d, err)
	}
	switch {
	case c.Database != "" && c.AnyDatabase:
		return fmt.Errorf("credential for %s: -database and -any-database are mutually exclusive", d)
	case c.Database != "":
		if err := validarNombrePG(c.Database); err != nil {
			return fmt.Errorf("credential for %s: database: %w", d, err)
		}
	case !c.AnyDatabase:
		return fmt.Errorf("credential for %s: a postgres credential needs -database (or -any-database to allow every database the role can connect to)", d)
	}
	// ASCII imprimible: SCRAM pide SASLprep, que para esto es la identidad
	// (ver scram.go), y la contraseña viaja como cadena C.
	for i := 0; i < len(c.Secret); i++ {
		if c.Secret[i] < 0x20 || c.Secret[i] > 0x7e {
			return fmt.Errorf("credential for %s: a postgres password must be printable ASCII", d)
		}
	}
	if len(c.CAPEM) > MaxCAPEM {
		return fmt.Errorf("credential for %s: the CA is larger than %d bytes", d, MaxCAPEM)
	}
	if c.CAPEM != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(c.CAPEM)) {
		return fmt.Errorf("credential for %s: the CA is not a PEM certificate", d)
	}
	return validarUpstream(c)
}

// validarNombrePG: un identificador de Postgres (rol o base de datos) sin
// nada que pueda romper el protocolo o un terminal.
func validarNombrePG(s string) error {
	if s == "" || len(s) > pgMaxNombre {
		return fmt.Errorf("must be 1-%d bytes", pgMaxNombre)
	}
	if !utf8.ValidString(s) {
		return errors.New("must be UTF-8")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

// PGActivo dice si el proxy de Postgres tiene algo que hacer: proxy activo
// (Options.Enabled) y al menos una credencial Postgres. kling-vz lo usa para
// no aceptar conexiones que acabarían rechazadas.
func (p *Proxy) PGActivo() bool {
	if p.enabled != nil && !p.enabled() {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.pg) > 0
}

// claveCancel identifica una sesión para CancelRequest: pid y clave, tal y
// como las ve el invitado.
type claveCancel struct{ pid, clave uint32 }

// destinoCancel es a dónde y con qué clave real va una cancelación.
type destinoCancel struct {
	cred       credPG
	pid, clave uint32
}

// connContada cuenta lo que se lee de y se escribe a la conexión del invitado.
type connContada struct {
	net.Conn
	leidos, escritos atomic.Int64
}

func (c *connContada) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.leidos.Add(int64(n))
	return n, err
}

func (c *connContada) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.escritos.Add(int64(n))
	return n, err
}

// ServePG atiende UNA conexión de un invitado al proxy de Postgres y la cierra
// al terminar. Cancelar ctx corta la conexión en el acto (en los dos tramos):
// es como el servidor que la sirve se lleva sus conexiones al cerrar.
func (p *Proxy) ServePG(ctx context.Context, conn net.Conn) {
	inicio := time.Now()
	guest := &connContada{Conn: conn}
	s := &sesionPG{p: p, guest: guest, rec: Record{Kind: KindPostgres}, inicio: inicio}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.ctx = ctx
	parar := context.AfterFunc(ctx, s.cerrar)
	defer parar()
	defer s.cerrar()
	defer func() {
		if p.aud == nil {
			return
		}
		s.rec.TS = inicio
		s.rec.ReqBytes = guest.leidos.Load()
		s.rec.RespBytes = guest.escritos.Load()
		s.rec.MS = time.Since(inicio).Milliseconds()
		p.aud.Record(s.rec)
	}()

	if p.enabled != nil && !p.enabled() {
		s.rec.Reason, s.rec.Denied = ReasonDisabled, true
		s.fatal("28000", "credentials need egress allowlist")
		return
	}
	// Sin ninguna credencial de Postgres (una máquina que solo tiene las de
	// HTTP también recibe aquí lo que mande a cualquier puerto del host) se
	// cierra sin leer un byte: el analizador del protocolo no queda expuesto a
	// quien no tiene nada que pedirle.
	if !p.PGActivo() {
		s.rec.Reason, s.rec.Denied = ReasonNoCredential, true
		return
	}
	select {
	case p.pgSem <- struct{}{}:
		defer func() { <-p.pgSem }()
	default:
		s.rec.Reason = ReasonBusy
		s.fatal("53300", "too many connections to the credential proxy")
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(pgKeepAlive)
	}
	s.servir()
}

// sesionPG es el estado de una conexión.
type sesionPG struct {
	p      *Proxy
	ctx    context.Context
	guest  *connContada
	inicio time.Time
	rec    Record

	mu      sync.Mutex
	up      net.Conn
	cerrada bool
}

func (s *sesionPG) cerrar() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cerrada = true
	_ = s.guest.Close()
	if s.up != nil {
		_ = s.up.Close()
	}
}

// ponerUp registra la conexión al servidor para que cerrar() la corte; false
// si la sesión ya se cerró (y entonces la cierra).
func (s *sesionPG) ponerUp(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cerrada {
		_ = c.Close()
		return false
	}
	s.up = c
	return true
}

// fatal manda al invitado un ErrorResponse propio. El texto es siempre
// nuestro: nada que venga del servidor.
func (s *sesionPG) fatal(code, msg string) {
	_ = s.guest.SetWriteDeadline(time.Now().Add(pgPreAuth))
	_, _ = s.guest.Write(errorResponsePG(code, "kindling credential proxy: "+msg))
}

// motivoLectura pone el motivo de un fallo leyendo del invitado.
func (s *sesionPG) motivoLectura(err error) {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		s.rec.Reason = ReasonTimeout
		return
	}
	s.rec.Reason = ReasonBadStartup
}

func (s *sesionPG) servir() {
	p, guest := s.p, s.guest
	_ = guest.SetDeadline(s.inicio.Add(p.pgPre))

	// 1. Arranque: negociaciones de cifrado ('N'), cancelación o StartupMessage.
	var code uint32
	var cuerpo []byte
	for negs := 0; ; negs++ {
		var err error
		code, cuerpo, err = leerArranque(guest)
		if err != nil {
			s.motivoLectura(err)
			return
		}
		if code != pgSSLRequest && code != pgGSSENCRequest {
			break
		}
		if len(cuerpo) != 0 || negs >= 2 {
			s.rec.Reason = ReasonBadStartup
			return
		}
		if _, err := guest.Write([]byte{'N'}); err != nil {
			return
		}
	}
	if code == pgCancelRequest {
		s.cancelar(cuerpo)
		return
	}
	if code>>16 != 3 {
		s.rec.Reason = ReasonBadStartup
		s.fatal("08P01", "unsupported frontend protocol "+strconv.Itoa(int(code>>16))+"."+strconv.Itoa(int(code&0xffff)))
		return
	}
	params, desconocidas, err := parsearParams(cuerpo)
	if err != nil {
		s.rec.Reason = ReasonBadStartup
		s.fatal("08P01", err.Error())
		return
	}
	user, db := params.get("user"), params.get("database")
	if db == "" {
		db = user
	}
	if v, ok := params.buscar("replication"); ok {
		switch strings.ToLower(v) {
		case "false", "off", "no", "0":
		default:
			s.rec.Reason, s.rec.Denied = ReasonReplication, true
			s.fatal("28000", "replication connections are not allowed")
			return
		}
	}
	// El servidor habla 3.0 (el proxy no le pide más): una versión menor más
	// nueva o las opciones _pq_. se rechazan con NegotiateProtocolVersion,
	// que el cliente acepta y sigue.
	if code&0xffff != 0 || len(desconocidas) > 0 {
		if _, err := guest.Write(negociarVersionPG(desconocidas)); err != nil {
			return
		}
	}

	// 2. La contraseña: el marcador.
	if _, err := guest.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 3}); err != nil {
		return
	}
	tipo, msg, err := leerMensaje(guest, pgMaxPassword+1)
	if err != nil {
		s.motivoLectura(err)
		return
	}
	if tipo != 'p' || len(msg) == 0 || msg[len(msg)-1] != 0 {
		s.rec.Reason = ReasonBadStartup
		s.fatal("08P01", "expected a password message")
		return
	}
	cred, hay, ok := p.elegirPG(msg[:len(msg)-1])
	if !ok {
		s.rec.Reason, s.rec.Denied = ReasonBadPlaceholder, true
		if !hay {
			s.rec.Reason = ReasonNoCredential
		}
		s.fatal("28P01", "password authentication failed (the password must be the placeholder of a postgres credential)")
		return
	}
	s.rec.Host, s.rec.Creds, s.rec.Upstream = cred.Domain, []string{cred.Env}, cred.Upstream
	if user != cred.User {
		s.rec.Reason, s.rec.Denied = ReasonUserMismatch, true
		s.fatal("28000", "the user does not match the credential")
		return
	}
	s.rec.User = cred.User
	if !cred.AnyDatabase && db != cred.Database {
		s.rec.Reason, s.rec.Denied = ReasonDatabaseMismatch, true
		s.fatal("28000", "the database does not match the credential")
		return
	}
	if validarNombrePG(db) != nil {
		s.rec.Reason = ReasonBadStartup
		s.fatal("08P01", "invalid database name")
		return
	}
	s.rec.Database = db
	if contieneInsensible(db, PlaceholderPrefix) {
		s.rec.Database = ":cred"
	}

	// 3. El servidor: TLS verificado y la autenticación con la clave real.
	fin := s.inicio.Add(p.pgAuth)
	_ = guest.SetDeadline(fin)
	ctx, cancel := context.WithDeadline(s.ctx, fin)
	defer cancel()
	up, err := p.abrirPG(ctx, cred)
	if err != nil {
		s.rec.Reason = ReasonUpstreamTLS
		if !errors.Is(err, errTLSUpstream) {
			s.rec.Reason = ReasonUpstreamError
		}
		p.logf("credential proxy postgres %s (%s): %v", cred.Domain, cred.destinoPG(), err)
		s.fatal("08006", "could not connect to the database server")
		return
	}
	if !s.ponerUp(up) {
		return
	}
	_ = up.SetDeadline(fin)
	br := bufio.NewReader(up)
	if _, err := up.Write(arranqueUpstream(cred.User, db, params)); err != nil {
		s.rec.Reason = ReasonUpstreamError
		s.fatal("08006", "could not connect to the database server")
		return
	}
	metodo, codigo, err := autenticarPG(br, up, cred, cred.UpstreamTLS == UpstreamTLSDisable)
	if err != nil {
		s.rec.Reason = ReasonUpstreamAuth
		if codigo != "" {
			p.logf("credential proxy postgres %s (%s): the server refused the authentication (SQLSTATE %s)", cred.Domain, cred.destinoPG(), codigo)
		} else {
			p.logf("credential proxy postgres %s (%s): %v", cred.Domain, cred.destinoPG(), err)
		}
		if strings.HasPrefix(codigo, "28") || codigo == "" {
			s.fatal("28P01", "the database server refused the credential"+sqlstateDe(codigo))
		} else {
			s.fatal("08006", "the database server refused the connection"+sqlstateDe(codigo))
		}
		return
	}
	s.rec.Auth = metodo
	if metodo == AuthPassword {
		cred.claro.Do(func() {
			p.logf("credential proxy postgres %s (%s): server asked for the password in cleartext inside TLS; prefer SCRAM", cred.Domain, cred.destinoPG())
		})
	}

	// 4. Autenticado: AuthenticationOk al invitado y a pasar bytes.
	_ = guest.SetDeadline(time.Time{})
	_ = up.SetDeadline(time.Time{})
	if _, err := guest.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 0}); err != nil {
		return
	}
	s.relevo(br, up, cred)
}

// relevo pasa bytes en los dos sentidos hasta que uno se cierra, y entonces
// cierra los dos. Del servidor al invitado se miran los mensajes hasta el
// primer ReadyForQuery para cambiar BackendKeyData; después, bytes tal cual.
func (s *sesionPG) relevo(br *bufio.Reader, up net.Conn, cred credPG) {
	var registradas []claveCancel
	defer func() {
		s.p.cancelMu.Lock()
		for _, k := range registradas {
			delete(s.p.cancelaciones, k)
		}
		s.p.cancelMu.Unlock()
	}()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer s.cerrar()
		_, _ = io.Copy(up, s.guest)
	}()
	func() {
		defer s.cerrar()
		for {
			tipo, msg, err := leerMensaje(br, pgMaxPreReady)
			if err != nil {
				return
			}
			if tipo == 'K' {
				if len(msg) != 8 {
					return
				}
				k, ok := s.p.registrarCancel(cred, binary.BigEndian.Uint32(msg[:4]), binary.BigEndian.Uint32(msg[4:]))
				if !ok {
					return
				}
				registradas = append(registradas, k)
				binary.BigEndian.PutUint32(msg[4:], k.clave)
			}
			if _, err := s.guest.Write(mensajePG(tipo, msg)); err != nil {
				return
			}
			if tipo == 'Z' {
				break
			}
		}
		_, _ = io.Copy(s.guest, br)
	}()
	wg.Wait()
}

// elegirPG busca la credencial Postgres cuyo marcador es pass. Compara con
// TODAS en tiempo constante: ni el tiempo dice cuál casó ni cuántas hay. hay
// es si la máquina tiene alguna credencial Postgres.
func (p *Proxy) elegirPG(pass []byte) (cred credPG, hay, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	elegida := -1
	for i, c := range p.pg {
		if subtle.ConstantTimeCompare(pass, []byte(c.Placeholder)) == 1 {
			elegida = i
		}
	}
	if elegida < 0 {
		return credPG{}, len(p.pg) > 0, false
	}
	return p.pg[elegida], true, true
}

// registrarCancel da una clave de cancelación falsa para (pid, clave) real.
func (p *Proxy) registrarCancel(cred credPG, pid, clave uint32) (claveCancel, bool) {
	p.cancelMu.Lock()
	defer p.cancelMu.Unlock()
	for range 8 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return claveCancel{}, false
		}
		k := claveCancel{pid: pid, clave: binary.BigEndian.Uint32(b[:])}
		if _, usada := p.cancelaciones[k]; !usada {
			p.cancelaciones[k] = destinoCancel{cred: cred, pid: pid, clave: clave}
			return k, true
		}
	}
	return claveCancel{}, false
}

// cancelar atiende un CancelRequest: con una clave que el proxy dio, abre una
// conexión nueva (al mismo destino y con el mismo modo TLS) y manda la real; con otra, cierra
// sin decir nada, como hace el propio Postgres.
func (s *sesionPG) cancelar(cuerpo []byte) {
	s.rec.Method = "cancel"
	if len(cuerpo) != 8 {
		s.rec.Reason = ReasonBadStartup
		return
	}
	k := claveCancel{pid: binary.BigEndian.Uint32(cuerpo[:4]), clave: binary.BigEndian.Uint32(cuerpo[4:])}
	s.p.cancelMu.Lock()
	d, ok := s.p.cancelaciones[k]
	s.p.cancelMu.Unlock()
	if !ok {
		s.rec.Reason, s.rec.Denied = ReasonUnknownCancel, true
		return
	}
	s.rec.Host, s.rec.Creds, s.rec.User = d.cred.Domain, []string{d.cred.Env}, d.cred.User
	s.rec.Upstream = d.cred.Upstream
	ctx, cancel := context.WithTimeout(s.ctx, s.p.pgAuth)
	defer cancel()
	up, err := s.p.abrirPG(ctx, d.cred)
	if err != nil {
		s.rec.Reason = ReasonUpstreamTLS
		if !errors.Is(err, errTLSUpstream) {
			s.rec.Reason = ReasonUpstreamError
		}
		s.p.logf("credential proxy postgres %s (%s): cancel: %v", d.cred.Domain, d.cred.destinoPG(), err)
		return
	}
	if !s.ponerUp(up) {
		return
	}
	var m [16]byte
	binary.BigEndian.PutUint32(m[0:], 16)
	binary.BigEndian.PutUint32(m[4:], pgCancelRequest)
	binary.BigEndian.PutUint32(m[8:], d.pid)
	binary.BigEndian.PutUint32(m[12:], d.clave)
	_ = up.SetDeadline(time.Now().Add(pgPreAuth))
	if _, err := up.Write(m[:]); err != nil {
		s.rec.Reason = ReasonUpstreamError
		return
	}
	// El servidor cierra sin contestar; esperarlo un momento hace que la
	// petición llegue entera antes de cortar el TLS.
	_, _ = up.Read(m[:1])
}

// errTLSUpstream marca los fallos de la negociación TLS con el servidor.
var errTLSUpstream = errors.New("TLS")

// abrirPG conecta con el servidor de cred y negocia TLS verificado. La
// respuesta al SSLRequest se lee de un byte y sin buffer (ver la cabecera).
// Con Upstream marca esa dirección con dialUp (upstream.go); si además
// UpstreamTLS es "disable", devuelve la conexión sin TLS y sin SSLRequest
// (autenticarPG solo admitirá entonces SCRAM-SHA-256).
func (p *Proxy) abrirPG(ctx context.Context, cred credPG) (net.Conn, error) {
	var c net.Conn
	var err error
	if cred.Upstream != "" {
		c, err = p.dialUp(ctx, cred.Upstream)
	} else {
		c, err = p.dialPG(ctx, "tcp", net.JoinHostPort(cred.Domain, strconv.Itoa(cred.Port)))
	}
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if cred.UpstreamTLS == UpstreamTLSDisable {
		return c, nil
	}
	parar := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer parar()
	if _, err := c.Write([]byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}); err != nil {
		c.Close()
		return nil, err
	}
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		c.Close()
		return nil, err
	}
	if b[0] != 'S' {
		c.Close()
		return nil, fmt.Errorf("%w: the server does not offer TLS (answered %q to SSLRequest)", errTLSUpstream, b[0])
	}
	tc := tls.Client(c, cred.tls)
	if err := tc.HandshakeContext(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("%w: %v", errTLSUpstream, err)
	}
	return tc, nil
}

// errSinTLSNecesitaSCRAM: sin TLS hacia el servidor solo vale SCRAM-SHA-256.
var errSinTLSNecesitaSCRAM = errors.New("upstream without TLS requires SCRAM-SHA-256")

// autenticarPG lleva la autenticación con el servidor hasta su
// AuthenticationOk. Devuelve el método usado; con un ErrorResponse del
// servidor, su SQLSTATE (nunca el texto). sinTLS (UpstreamTLS "disable")
// admite SOLO SASL SCRAM-SHA-256 (sin -PLUS): ni contraseña en claro, ni md5,
// ni un AuthenticationOk sin SCRAM (trust), que dejaría al servidor sin
// probar que conoce la clave cuando no hay certificado que lo identifique.
func autenticarPG(br *bufio.Reader, up net.Conn, cred credPG, sinTLS bool) (metodo, codigo string, err error) {
	var scram *scramCliente
	verificado := false
	for {
		tipo, msg, err := leerMensaje(br, pgMaxAuthMsg)
		if err != nil {
			return "", "", err
		}
		switch tipo {
		case 'E':
			c := sqlstate(msg)
			return "", c, errors.New("error response")
		case 'N':
			continue // NoticeResponse: no se reenvía
		case 'R':
		default:
			return "", "", fmt.Errorf("unexpected message %q during authentication", tipo)
		}
		if len(msg) < 4 {
			return "", "", errors.New("malformed authentication message")
		}
		sub, datos := binary.BigEndian.Uint32(msg[:4]), msg[4:]
		switch sub {
		case 0: // AuthenticationOk
			if scram != nil && !verificado {
				return "", "", errors.New("AuthenticationOk without a verified SCRAM server signature")
			}
			if sinTLS && scram == nil {
				return "", "", fmt.Errorf("%w (the server let us in without authenticating)", errSinTLSNecesitaSCRAM)
			}
			if metodo == "" {
				metodo = AuthTrust
			}
			return metodo, "", nil
		case 3: // AuthenticationCleartextPassword: dentro del TLS verificado
			if sinTLS {
				return "", "", fmt.Errorf("%w (the server asked for a cleartext password)", errSinTLSNecesitaSCRAM)
			}
			if metodo != "" {
				return "", "", errors.New("unexpected password request")
			}
			metodo = AuthPassword
			if _, err := up.Write(mensajePG('p', append([]byte(cred.Secret), 0))); err != nil {
				return "", "", err
			}
		case 10: // AuthenticationSASL
			if metodo != "" {
				return "", "", errors.New("unexpected SASL request")
			}
			mecs := cadenasPG(datos)
			var cb []byte
			if tc, ok := up.(*tls.Conn); ok && !sinTLS {
				if cs := tc.ConnectionState(); len(cs.PeerCertificates) > 0 {
					cb = tlsServerEndPoint(cs.PeerCertificates[0])
				}
			}
			var mec string
			switch {
			case cb != nil && contiene(mecs, scramSHA256Plus):
				mec, metodo = scramSHA256Plus, AuthSCRAMPlus
			case contiene(mecs, scramSHA256):
				mec, metodo = scramSHA256, AuthSCRAM
			case sinTLS:
				return "", "", fmt.Errorf("%w (the server offers %s)", errSinTLSNecesitaSCRAM, strings.Join(mecs, ", "))
			default:
				return "", "", fmt.Errorf("the server offers no supported SASL mechanism (%s)", strings.Join(mecs, ", "))
			}
			scram, err = nuevoScram(cred.Secret, cb, mec == scramSHA256Plus)
			if err != nil {
				return "", "", err
			}
			primero := scram.primero()
			b := append([]byte(mec), 0)
			b = binary.BigEndian.AppendUint32(b, uint32(len(primero)))
			b = append(b, primero...)
			if _, err := up.Write(mensajePG('p', b)); err != nil {
				return "", "", err
			}
		case 11: // AuthenticationSASLContinue
			if scram == nil || scram.firmaServidor != nil {
				return "", "", errors.New("unexpected SASLContinue")
			}
			final, err := scram.final(datos)
			if err != nil {
				return "", "", err
			}
			if _, err := up.Write(mensajePG('p', final)); err != nil {
				return "", "", err
			}
		case 12: // AuthenticationSASLFinal
			if scram == nil || verificado {
				return "", "", errors.New("unexpected SASLFinal")
			}
			if err := scram.verificar(datos); err != nil {
				return "", "", err
			}
			verificado = true
		case 5:
			if sinTLS {
				return "", "", fmt.Errorf("%w (the server asked for md5)", errSinTLSNecesitaSCRAM)
			}
			return "", "", errors.New("the server asked for md5, which the proxy does not support (use scram-sha-256)")
		default:
			return "", "", fmt.Errorf("the server asked for an unsupported authentication method (%d)", sub)
		}
	}
}

// ── mensajes ────────────────────────────────────────────────────────────────

// leerArranque lee un mensaje de arranque (sin byte de tipo): longitud y
// código, y devuelve el resto. Un 0x16 al principio es un ClientHello de TLS
// directo: no se habla TLS en este tramo.
func leerArranque(r io.Reader) (uint32, []byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if h[0] == 0x16 {
		return 0, nil, errors.New("TLS without SSLRequest")
	}
	n := binary.BigEndian.Uint32(h[:])
	if n < 8 || n > pgMaxStartup {
		return 0, nil, fmt.Errorf("startup packet of %d bytes", n)
	}
	b := make([]byte, n-4)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	return binary.BigEndian.Uint32(b[:4]), b[4:], nil
}

// leerMensaje lee un mensaje con tipo; cuerpos de más de max bytes son un error.
func leerMensaje(r io.Reader, max int) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n < 4 || int64(n)-4 > int64(max) {
		return 0, nil, fmt.Errorf("message %q of %d bytes", h[0], n)
	}
	b := make([]byte, n-4)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	return h[0], b, nil
}

func mensajePG(tipo byte, cuerpo []byte) []byte {
	b := make([]byte, 5, 5+len(cuerpo))
	b[0] = tipo
	binary.BigEndian.PutUint32(b[1:], uint32(4+len(cuerpo)))
	return append(b, cuerpo...)
}

// errorResponsePG es un ErrorResponse FATAL con código y mensaje.
func errorResponsePG(code, msg string) []byte {
	var b []byte
	for _, f := range [][2]string{{"S", "FATAL"}, {"V", "FATAL"}, {"C", code}, {"M", msg}} {
		b = append(b, f[0][0])
		b = append(b, f[1]...)
		b = append(b, 0)
	}
	return mensajePG('E', append(b, 0))
}

// negociarVersionPG es NegotiateProtocolVersion: 3.0 y las opciones _pq_. que
// no se reconocen.
func negociarVersionPG(desconocidas []string) []byte {
	b := binary.BigEndian.AppendUint32(nil, 0)
	b = binary.BigEndian.AppendUint32(b, uint32(len(desconocidas)))
	for _, d := range desconocidas {
		b = append(append(b, d...), 0)
	}
	return mensajePG('v', b)
}

// sqlstate saca el código (campo C) de un ErrorResponse, solo si tiene la
// forma de un SQLSTATE: nada más del mensaje sale de aquí.
func sqlstate(msg []byte) string {
	for len(msg) > 0 && msg[0] != 0 {
		campo := msg[0]
		fin := -1
		for i := 1; i < len(msg); i++ {
			if msg[i] == 0 {
				fin = i
				break
			}
		}
		if fin < 0 {
			return ""
		}
		if campo == 'C' {
			v := string(msg[1:fin])
			if len(v) != 5 {
				return ""
			}
			for i := 0; i < 5; i++ {
				if !(v[i] >= '0' && v[i] <= '9' || v[i] >= 'A' && v[i] <= 'Z') {
					return ""
				}
			}
			return v
		}
		msg = msg[fin+1:]
	}
	return ""
}

func sqlstateDe(c string) string {
	if c == "" {
		return ""
	}
	return " (upstream SQLSTATE " + c + ")"
}

// cadenasPG parte una lista de cadenas C terminada en una vacía.
func cadenasPG(b []byte) []string {
	var out []string
	for len(b) > 0 {
		i := 0
		for i < len(b) && b[i] != 0 {
			i++
		}
		if i == 0 || i == len(b) {
			break
		}
		out = append(out, string(b[:i]))
		b = b[i+1:]
	}
	return out
}

func contiene(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// paramsPG son los parámetros del StartupMessage, en orden.
type paramsPG [][2]string

func (ps paramsPG) buscar(k string) (string, bool) {
	for _, p := range ps {
		if p[0] == k {
			return p[1], true
		}
	}
	return "", false
}

func (ps paramsPG) get(k string) string { v, _ := ps.buscar(k); return v }

// parsearParams lee los pares clave/valor de un StartupMessage. Una clave
// repetida es un error (¿cuál valdría?). Las _pq_.* se apartan: el proxy no
// las entiende ni las pasa. user es obligatorio.
func parsearParams(b []byte) (paramsPG, []string, error) {
	var ps paramsPG
	var pq []string
	vistas := map[string]bool{}
	for {
		k, resto, ok := cadenaC(b)
		if !ok {
			return nil, nil, errors.New("malformed startup packet")
		}
		b = resto
		if k == "" {
			break
		}
		v, resto, ok := cadenaC(b)
		if !ok {
			return nil, nil, errors.New("malformed startup packet")
		}
		b = resto
		if vistas[k] {
			return nil, nil, fmt.Errorf("repeated startup parameter %q", k)
		}
		vistas[k] = true
		if strings.HasPrefix(k, "_pq_.") {
			pq = append(pq, k)
			continue
		}
		ps = append(ps, [2]string{k, v})
	}
	if len(b) != 0 {
		return nil, nil, errors.New("malformed startup packet")
	}
	if ps.get("user") == "" {
		return nil, nil, errors.New("no user in the startup packet")
	}
	return ps, pq, nil
}

func cadenaC(b []byte) (string, []byte, bool) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), b[i+1:], true
		}
	}
	return "", nil, false
}

// arranqueUpstream es el StartupMessage 3.0 hacia el servidor: rol y base de
// datos de la credencial, y el resto de parámetros del invitado (menos
// replication, que ya se comprobó que es falso).
func arranqueUpstream(user, db string, ps paramsPG) []byte {
	b := binary.BigEndian.AppendUint32(make([]byte, 4, 256), pgProto30)
	add := func(k, v string) { b = append(append(append(append(b, k...), 0), v...), 0) }
	add("user", user)
	add("database", db)
	for _, p := range ps {
		switch p[0] {
		case "user", "database", "replication":
			continue
		}
		add(p[0], p[1])
	}
	b = append(b, 0)
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	return b
}

// ── servidor ────────────────────────────────────────────────────────────────

// PGServer sirve el proxy de Postgres en un listener (Linux: el lado host del
// veth). Close deja de aceptar y corta las conexiones abiertas.
type PGServer struct {
	p      *Proxy
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	lns    []net.Listener
}

// NewPGServer crea el servidor de p.
func NewPGServer(p *Proxy) *PGServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &PGServer{p: p, ctx: ctx, cancel: cancel}
}

// Serve acepta en ln hasta Close. Devuelve nil tras Close.
func (s *PGServer) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.lns = append(s.lns, ln)
	s.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// EMFILE y compañía: esperar un poco en vez de girar en vacío.
			time.Sleep(20 * time.Millisecond)
			continue
		}
		// Add bajo el candado y solo si no se ha cerrado: Close cancela bajo
		// el mismo candado, así que su Wait nunca ve un Add a destiempo.
		s.mu.Lock()
		if s.ctx.Err() != nil {
			s.mu.Unlock()
			_ = c.Close()
			return nil
		}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.p.ServePG(s.ctx, c)
		}()
	}
}

// Close cierra los listeners y las conexiones, y espera a que terminen.
func (s *PGServer) Close() error {
	s.mu.Lock()
	s.cancel()
	for _, ln := range s.lns {
		_ = ln.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}
