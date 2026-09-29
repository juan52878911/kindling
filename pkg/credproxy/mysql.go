package credproxy

// Proxy de credenciales para MySQL y MariaDB: el modelo de postgres.go (la
// clave real no entra en la microVM; el invitado tiene un marcador) sobre el
// protocolo cliente/servidor de MySQL 4.1+.
//
// EL TRAMO DEL INVITADO va en claro, como en Postgres: el invitado conecta al
// puerto del dominio con credencial, que su resolver desvía al proxy. En
// MySQL habla primero el servidor, así que el proxy manda SU saludo (versión
// propia, id de conexión 0, un nonce nuevo, sin CLIENT_SSL: un cliente que
// exige TLS falla en su lado). El invitado contesta con usuario y la prueba
// del marcador: mysql_native_password o caching_sha2_password (la ruta
// rápida, la del scramble). Cualquier otro plugin recibe un AuthSwitchRequest
// a mysql_native_password con el mismo nonce. El proxy calcula lo que daría
// el marcador de CADA credencial MySQL con ese nonce y lo compara en tiempo
// constante con lo recibido; así elige la credencial. Después exige que el
// usuario sea el de la credencial y, si fija base, esa o ninguna.
//
// EL TRAMO DEL SERVIDOR sale como el de Postgres (dialPublico, o Upstream
// fijado por el operador con la barrera de upstream.go) y por defecto con TLS
// verificado (TLS 1.2+, contra el nombre de la credencial o TLSServerName): si
// el saludo del servidor no ofrece CLIENT_SSL es un fallo, no "entonces en
// claro". El saludo se lee con su longitud exacta y sin buffer, y el cliente
// empieza el TLS nada más mandar el SSLRequest: lo que un intermediario meta
// detrás del saludo acaba dentro del handshake de TLS, que falla. La
// autenticación con la clave real:
//   - caching_sha2_password: el scramble; si el servidor pide la
//     autenticación completa (0x04), la clave en claro DENTRO del TLS
//     verificado (lo que hace cualquier cliente). Sin TLS se rechaza: la
//     alternativa, cifrar con la clave pública RSA que manda el propio
//     servidor, no autentica al servidor y este proxy no la implementa.
//   - mysql_native_password: el scramble SHA-1 (la clave no cruza).
//   - mysql_clear_password (tras un AuthSwitch): solo dentro del TLS.
//   - Nada más (sha256_password, ed25519, GSSAPI, PAM...): se rechaza.
//
// UpstreamTLS "disable" (solo con Upstream) quita el TLS: se admiten
// mysql_native_password y la ruta rápida de caching_sha2_password, que no
// mandan la clave; la autenticación completa y la clave en claro se rechazan.
// OJO, más débil que en Postgres: en MySQL el servidor NO prueba que conoce
// la clave (no hay equivalente a la firma de SCRAM). Un impostor en la
// dirección fijada se quedaría con un scramble (que permite atacar la clave
// por diccionario, inútil contra una clave aleatoria larga) y con las
// consultas del invitado. Por eso la CLI lo advierte fuera del loopback.
//
// CAPACIDADES: tras autenticar se empalman bytes, así que invitado y servidor
// tienen que hablar el mismo dialecto. El proxy ofrece al invitado un juego
// fijo y conservador (sin compresión, sin LOCAL INFILE, sin atributos de
// conexión ni de consulta, sin metadatos opcionales) y exige que el servidor
// tenga las que cambian el formato de lo que viaja después (DEPRECATE_EOF,
// SESSION_TRACK, MULTI_RESULTS...) si el invitado las eligió; si no, error
// propio antes de mandar la clave.
//
// QUÉ VE EL INVITADO de la autenticación: un OK solo cuando el servidor dio el
// suyo (se le reenvía ese mismo OK, ya autenticado). Un error del servidor
// antes de eso NO se reenvía: el invitado recibe uno propio (1045 o 2003, con
// el código del servidor como mucho) y el host lo anota sin texto. Después,
// bytes tal cual en los dos sentidos: aquí NO se sustituye nada.
//
// KILL QUERY NO SE MAPEA en esta versión. En MySQL cancelar es abrir OTRA
// conexión y mandar `KILL QUERY <id>`, con el id que dio el saludo. El id del
// saludo es del proxy (0: el saludo sale antes de saber a qué servidor se va)
// y mapearlo exigiría reescribir SQL en el flujo, que este proxy no mira. Un
// cliente que cancela así (Connector/J con setQueryTimeout, por ejemplo)
// recibe "Unknown thread id: 0" y la consulta sigue hasta su final o su
// max_execution_time. El id real sale de SELECT CONNECTION_ID(): un KILL
// QUERY con él, por el proxy y como el mismo usuario, funciona.
//
// QUÉ NO RESUELVE: como en Postgres, el invitado usa el usuario con todos sus
// permisos. Y además, en MySQL la base NO es una frontera: con la credencial
// fijada a una base, el invitado puede hacer USE otra o leer otra.tabla si el
// usuario tiene GRANT allí. Lo que acota es el GRANT del usuario (solo en su
// base, sin FILE, SUPER, PROCESS ni GRANT OPTION). La versión que ve el
// invitado en el saludo es la del proxy; SELECT VERSION() da la real.
//
// LÍMITES y REGISTRO: MaxMySQLConns conexiones a la vez por máquina, los
// plazos de Postgres (pgPre, pgAuth), paquetes de antes de autenticar
// acotados a myMaxPaquete; una línea de auditoría por conexión (kind "mysql")
// con dominio, usuario, base, método con el servidor, motivo, bytes y
// duración. Nunca la clave, el marcador ni nada del SQL.
//
// A QUÉ PUERTO: todo el TCP de la IP del proxy que no es 53, 80 ni 443 llega
// a ServeDB. Con solo credenciales MySQL, todo es MySQL; con solo Postgres,
// todo Postgres. Con las dos, MySQL es el puerto 3306 (ValidarCredenciales lo
// exige así) y lo demás Postgres: en macOS el puerto se ve tal cual; en Linux
// el DNAT del 3306 lleva a un listener propio (internal/net).

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
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
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// KindMySQL es el tipo de una credencial de MySQL/MariaDB y el kind de sus
	// registros de auditoría. kling-vz lo anuncia en credential_kinds.
	KindMySQL = "mysql"
	// MaxMySQLConns: conexiones a la vez al proxy de MySQL de una máquina.
	MaxMySQLConns = 32
	// MySQLDefaultPort es el puerto del servidor si la credencial no dice
	// otro, y el que distingue MySQL de Postgres si la máquina tiene de los
	// dos (ver la cabecera).
	MySQLDefaultPort = 3306

	// myMaxPaquete acota cada paquete antes de autenticar (los dos tramos).
	myMaxPaquete = 16 << 10
	// myMaxUsuario y myMaxBase: MariaDB admite usuarios de hasta 80
	// caracteres (MySQL 8, 32) y bases de 64.
	myMaxUsuario = 80
	myMaxBase    = 64
	myMaxAuth    = 255

	// myVersion es la versión del saludo del proxy: una 8.0 para que los
	// clientes elijan el dialecto moderno; SELECT VERSION() da la real.
	myVersion = "8.0.40-kindling-credproxy"
	// myCharset es utf8mb4_general_ci (45), que conocen MySQL y MariaDB.
	myCharset = 45
	// myAutocommit es SERVER_STATUS_AUTOCOMMIT.
	myAutocommit = 0x0002
)

// Capacidades del protocolo (CLIENT_*).
const (
	myLongPassword    uint32 = 1 << 0
	myFoundRows       uint32 = 1 << 1
	myLongFlag        uint32 = 1 << 2
	myConnectWithDB   uint32 = 1 << 3
	myCompress        uint32 = 1 << 5
	myLocalFiles      uint32 = 1 << 7
	myIgnoreSpace     uint32 = 1 << 8
	myProtocol41      uint32 = 1 << 9
	myInteractive     uint32 = 1 << 10
	mySSL             uint32 = 1 << 11
	myIgnoreSigpipe   uint32 = 1 << 12
	myTransactions    uint32 = 1 << 13
	mySecureConn      uint32 = 1 << 15
	myMultiStatements uint32 = 1 << 16
	myMultiResults    uint32 = 1 << 17
	myPSMultiResults  uint32 = 1 << 18
	myPluginAuth      uint32 = 1 << 19
	myConnectAttrs    uint32 = 1 << 20
	myAuthLenenc      uint32 = 1 << 21
	mySessionTrack    uint32 = 1 << 23
	myDeprecateEOF    uint32 = 1 << 24
)

// myOfrecidas es lo que el proxy ofrece al invitado. Fuera a propósito:
// SSL (este tramo no lleva TLS), COMPRESS/ZSTD (el empalme no descomprime),
// LOCAL_FILES (que el servidor pueda pedir ficheros al invitado),
// CONNECT_ATTRS, QUERY_ATTRIBUTES y OPTIONAL_RESULTSET_METADATA.
const myOfrecidas = myLongPassword | myFoundRows | myLongFlag | myConnectWithDB | myIgnoreSpace |
	myProtocol41 | myInteractive | myIgnoreSigpipe | myTransactions | mySecureConn |
	myMultiStatements | myMultiResults | myPSMultiResults | myPluginAuth | myAuthLenenc |
	mySessionTrack | myDeprecateEOF

// myDeFormato son las capacidades que cambian lo que viaja después de
// autenticar (o cómo lo interpreta el servidor): si el invitado eligió una, el
// servidor tiene que tenerla, porque los bytes se empalman tal cual.
const myDeFormato = myFoundRows | myLongFlag | myIgnoreSpace | myInteractive | myTransactions |
	myMultiStatements | myMultiResults | myPSMultiResults | mySessionTrack | myDeprecateEOF

// Plugins de autenticación.
const (
	pluginNative  = "mysql_native_password"
	pluginSHA2    = "caching_sha2_password"
	pluginLimpio  = "mysql_clear_password"
	sha2RapidoOK  = 3 // AuthMoreData: la ruta rápida ha valido
	sha2Completa  = 4 // AuthMoreData: el servidor pide la autenticación completa
	myMaxCambios  = 1 // AuthSwitchRequest admitidos por conexión y tramo
	myErrCodigo   = 0xff
	myOKCodigo    = 0x00
	myCambio      = 0xfe
	myMasDatos    = 0x01
	myIDConexion  = 0
	myMaxPaqNorma = 1 << 24 // max_packet por defecto si el invitado manda 0
)

// Métodos de autenticación con el servidor (Record.Auth).
const (
	AuthMySQLNative     = pluginNative
	AuthCachingSHA2Fast = "caching_sha2_password-fast"
	AuthCachingSHA2     = pluginSHA2
	AuthMySQLClear      = pluginLimpio
)

// ReasonCapabilities: el servidor no tiene una capacidad que el invitado
// eligió (o le faltan las básicas): no se le manda la clave.
const ReasonCapabilities = "capabilities"

// validarMySQL: ver ValidarTipo.
func validarMySQL(c *Credential) error {
	d := c.Domain
	if len(c.Allow) > 0 {
		return fmt.Errorf("credential for %s: -allow-request is only for HTTP credentials", d)
	}
	if c.UpstreamMachine != "" || c.UpstreamOwner != "" {
		return fmt.Errorf("credential for %s: upstream machine (kling db attach) is only for postgres credentials in this version", d)
	}
	if c.Port == 0 {
		c.Port = MySQLDefaultPort
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("credential for %s: port %d out of range", d, c.Port)
	}
	switch c.Port {
	case 53, 80, 443:
		// El DNAT de esos puertos lleva al DNS o al proxy HTTP, nunca aquí.
		return fmt.Errorf("credential for %s: port %d is taken by the DNS or the HTTP proxy; a mysql credential cannot use it", d, c.Port)
	}
	if c.User == "" {
		return fmt.Errorf("credential for %s: a mysql credential needs -user", d)
	}
	if err := validarNombreMy(c.User, myMaxUsuario); err != nil {
		return fmt.Errorf("credential for %s: user: %w", d, err)
	}
	switch {
	case c.Database != "" && c.AnyDatabase:
		return fmt.Errorf("credential for %s: -database and -any-database are mutually exclusive", d)
	case c.Database != "":
		if err := validarNombreMy(c.Database, myMaxBase); err != nil {
			return fmt.Errorf("credential for %s: database: %w", d, err)
		}
	case !c.AnyDatabase:
		return fmt.Errorf("credential for %s: a mysql credential needs -database (or -any-database to let the guest pick any database)", d)
	}
	// ASCII imprimible: la clave viaja como cadena C en la autenticación
	// completa y en mysql_clear_password.
	for i := 0; i < len(c.Secret); i++ {
		if c.Secret[i] < 0x20 || c.Secret[i] > 0x7e {
			return fmt.Errorf("credential for %s: a mysql password must be printable ASCII", d)
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

// validarNombreMy: un usuario o una base de MySQL sin nada que pueda romper el
// protocolo (van como cadenas C) o un terminal.
func validarNombreMy(s string, max int) error {
	if s == "" || len(s) > max {
		return fmt.Errorf("must be 1-%d bytes", max)
	}
	if !utf8.ValidString(s) {
		return errors.New("must be UTF-8")
	}
	for _, r := range s {
		if r == 0 || unicode.IsControl(r) {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

// validarPuertosDB: con credenciales MySQL y Postgres en la misma máquina, el
// proxy las distingue por el puerto al que conecta el invitado, y el único que
// ve en Linux es el 3306 (ver la cabecera). Así que entonces las MySQL van al
// 3306 y ninguna Postgres.
func validarPuertosDB(creds []Credential) error {
	var hayPG, hayMy bool
	for _, c := range creds {
		hayPG = hayPG || c.Kind == KindPostgres
		hayMy = hayMy || c.Kind == KindMySQL
	}
	if !hayPG || !hayMy {
		return nil
	}
	for _, c := range creds {
		switch {
		case c.Kind == KindMySQL && c.Port != MySQLDefaultPort:
			return fmt.Errorf("credential for %s: with postgres credentials on the same machine, a mysql credential must use port %d (got %d)", c.Domain, MySQLDefaultPort, c.Port)
		case c.Kind == KindPostgres && c.Port == MySQLDefaultPort:
			return fmt.Errorf("credential for %s: with mysql credentials on the same machine, port %d is mysql's; a postgres credential cannot use it", c.Domain, MySQLDefaultPort)
		}
	}
	return nil
}

// MySQLActivo dice si el proxy de MySQL tiene algo que hacer: proxy activo y
// al menos una credencial MySQL.
func (p *Proxy) MySQLActivo() bool {
	if p.enabled != nil && !p.enabled() {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.my) > 0
}

// DBActivo dice si alguno de los proxies de bases de datos (Postgres o MySQL)
// tiene algo que hacer. kling-vz lo usa para no aceptar conexiones que
// acabarían rechazadas.
func (p *Proxy) DBActivo() bool {
	if p.enabled != nil && !p.enabled() {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.pg) > 0 || len(p.my) > 0
}

// ServeDB atiende UNA conexión de un invitado a la IP del proxy en un puerto
// de base de datos y la cierra al terminar. port es el puerto al que conectó
// el invitado si quien sirve lo sabe (0 si no: el listener genérico de Linux).
// Decide el protocolo como dice la cabecera: MySQL si solo hay credenciales
// MySQL, o si hay de los dos tipos y el puerto es el 3306; si no, Postgres.
func (p *Proxy) ServeDB(ctx context.Context, conn net.Conn, port int) {
	p.mu.RLock()
	hayPG, hayMy := len(p.pg) > 0, len(p.my) > 0
	p.mu.RUnlock()
	if hayMy && (!hayPG || port == MySQLDefaultPort) {
		p.ServeMySQL(ctx, conn)
		return
	}
	p.ServePG(ctx, conn)
}

// ServeMySQL atiende UNA conexión de un invitado al proxy de MySQL y la cierra
// al terminar. Cancelar ctx corta la conexión en el acto (en los dos tramos).
func (p *Proxy) ServeMySQL(ctx context.Context, conn net.Conn) {
	inicio := time.Now()
	guest := &connContada{Conn: conn}
	s := &sesionMy{sesionPG: sesionPG{p: p, guest: guest, rec: Record{Kind: KindMySQL}, inicio: inicio}}
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
		s.errorMy(0, 1045, "28000", "credentials need egress allowlist")
		return
	}
	// Sin credenciales MySQL se cierra sin mandar ni leer nada.
	if !p.MySQLActivo() {
		s.rec.Reason, s.rec.Denied = ReasonNoCredential, true
		return
	}
	select {
	case p.mySem <- struct{}{}:
		defer func() { <-p.mySem }()
	default:
		s.rec.Reason = ReasonBusy
		s.errorMy(0, 1040, "08004", "too many connections to the credential proxy")
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(pgKeepAlive)
	}
	s.servir()
}

// sesionMy es el estado de una conexión: el de Postgres (cierre, conexión al
// servidor, registro) más el protocolo de MySQL.
type sesionMy struct {
	sesionPG
}

// errorMy manda al invitado un paquete ERR propio con número de secuencia
// seq. El texto es siempre nuestro: nada que venga del servidor.
func (s *sesionMy) errorMy(seq byte, code uint16, sqlstate, msg string) {
	_ = s.guest.SetWriteDeadline(time.Now().Add(pgPreAuth))
	_, _ = s.guest.Write(paqueteMy(seq, errMy(code, sqlstate, "kindling credential proxy: "+msg)))
}

// respuestaMy es lo que el invitado manda tras el saludo (HandshakeResponse41).
type respuestaMy struct {
	caps       uint32
	maxPaquete uint32
	charset    byte
	user, db   string
	auth       []byte
	plugin     string
}

func (s *sesionMy) servir() {
	p, guest := s.p, s.guest
	_ = guest.SetDeadline(s.inicio.Add(p.pgPre))

	// 1. El saludo del proxy y la respuesta del invitado.
	nonce, err := nonceMy()
	if err != nil {
		s.rec.Reason = ReasonUpstreamError
		return
	}
	if _, err := guest.Write(paqueteMy(0, saludoMy(nonce))); err != nil {
		return
	}
	seq, b, err := leerPaqueteMy(guest, myMaxPaquete)
	if err != nil {
		s.motivoLectura(err)
		return
	}
	g, err := parsearRespuestaMy(b)
	if err != nil {
		s.rec.Reason = ReasonBadStartup
		s.errorMy(seq+1, 1043, "08S01", err.Error())
		return
	}

	// 2. La prueba del marcador. Un plugin que no es ninguno de los dos se
	// cambia a mysql_native_password con el mismo nonce.
	plugin := g.plugin
	if g.caps&myPluginAuth == 0 {
		plugin = pluginNative
	}
	auth := g.auth
	if plugin != pluginNative && plugin != pluginSHA2 {
		plugin = pluginNative
		sw := append([]byte{myCambio}, pluginNative...)
		sw = append(append(append(sw, 0), nonce...), 0)
		if _, err := guest.Write(paqueteMy(seq+1, sw)); err != nil {
			return
		}
		if seq, auth, err = leerPaqueteMy(guest, myMaxPaquete); err != nil {
			s.motivoLectura(err)
			return
		}
	}
	cred, hay, ok := p.elegirMy(plugin, nonce, auth)
	if !ok {
		s.rec.Reason, s.rec.Denied = ReasonBadPlaceholder, true
		if !hay {
			s.rec.Reason = ReasonNoCredential
		}
		s.errorMy(seq+1, 1045, "28000", "access denied (the password must be the placeholder of a mysql credential)")
		return
	}
	s.rec.Host, s.rec.Creds, s.rec.Upstream = cred.Domain, []string{cred.Env}, cred.upstreamAuditado()
	if g.user != cred.User {
		s.rec.Reason, s.rec.Denied = ReasonUserMismatch, true
		s.errorMy(seq+1, 1045, "28000", "the user does not match the credential")
		return
	}
	s.rec.User, s.rec.AnyDatabase = cred.User, cred.AnyDatabase
	db := g.db
	if db == "" {
		db = cred.Database
	}
	if !cred.AnyDatabase && db != cred.Database {
		s.rec.Reason, s.rec.Denied = ReasonDatabaseMismatch, true
		s.errorMy(seq+1, 1044, "42000", "the database does not match the credential")
		return
	}
	if db != "" && validarNombreMy(db, myMaxBase) != nil {
		s.rec.Reason = ReasonBadStartup
		s.errorMy(seq+1, 1043, "08S01", "invalid database name")
		return
	}
	s.rec.Database = db
	if contieneInsensible(db, PlaceholderPrefix) {
		s.rec.Database = ":cred"
	}

	// 3. El servidor: saludo, TLS verificado y la autenticación con la clave.
	fin := s.inicio.Add(p.pgAuth)
	_ = guest.SetDeadline(fin)
	ctx, cancel := context.WithDeadline(s.ctx, fin)
	defer cancel()
	up, metodo, okUp, err := s.conectarMy(ctx, cred, g, db)
	if err != nil {
		var es *errServidorMy
		switch {
		case errors.As(err, &es):
			s.rec.Reason = ReasonUpstreamAuth
			p.logf("credential proxy mysql %s (%s): the server refused the connection (error %d)", cred.Domain, cred.destinoPG(), es.code)
			if es.code == 1045 || strings.HasPrefix(es.sqlstate, "28") {
				s.errorMy(seq+1, 1045, "28000", "the database server refused the credential"+codigoMy(es.code))
			} else {
				s.errorMy(seq+1, 2003, "HY000", "the database server refused the connection"+codigoMy(es.code))
			}
			return
		case errors.Is(err, errCapacidadesMy):
			s.rec.Reason = ReasonCapabilities
		case errors.Is(err, errTLSUpstream):
			s.rec.Reason = ReasonUpstreamTLS
		case errors.Is(err, errAutenticacionMy):
			s.rec.Reason = ReasonUpstreamAuth
		default:
			s.rec.Reason = ReasonUpstreamError
		}
		p.logf("credential proxy mysql %s (%s): %v", cred.Domain, cred.destinoPG(), err)
		if s.rec.Reason == ReasonUpstreamAuth {
			s.errorMy(seq+1, 1045, "28000", "the database server asked for an authentication the proxy does not do (see the host log)")
			return
		}
		s.errorMy(seq+1, 2003, "HY000", "could not connect to the database server")
		return
	}
	s.rec.Auth = metodo
	if metodo == AuthMySQLClear || metodo == AuthCachingSHA2 {
		cred.claro.Do(func() {
			p.logf("credential proxy mysql %s (%s): the server took the password in cleartext inside TLS (%s)", cred.Domain, cred.destinoPG(), metodo)
		})
	}

	// 4. Autenticado: el OK del servidor al invitado (tras el "ruta rápida
	// vale" si habló caching_sha2) y a pasar bytes.
	_ = guest.SetDeadline(time.Time{})
	_ = up.SetDeadline(time.Time{})
	if plugin == pluginSHA2 {
		seq++
		if _, err := guest.Write(paqueteMy(seq, []byte{myMasDatos, sha2RapidoOK})); err != nil {
			return
		}
	}
	if _, err := guest.Write(paqueteMy(seq+1, okUp)); err != nil {
		return
	}
	s.empalmar(up)
}

// empalmar pasa bytes en los dos sentidos hasta que uno se cierra, y entonces
// cierra los dos.
func (s *sesionMy) empalmar(up net.Conn) {
	hecho := make(chan struct{})
	go func() {
		defer close(hecho)
		defer s.cerrar()
		_, _ = io.Copy(up, s.guest)
	}()
	func() {
		defer s.cerrar()
		_, _ = io.Copy(s.guest, up)
	}()
	<-hecho
}

// elegirMy busca la credencial MySQL cuyo marcador da auth con ese nonce y
// ese plugin. Calcula y compara con TODAS en tiempo constante: ni el tiempo
// dice cuál casó ni cuántas hay. hay es si la máquina tiene alguna MySQL.
func (p *Proxy) elegirMy(plugin string, nonce, auth []byte) (cred credPG, hay, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	elegida := -1
	for i, c := range p.my {
		var esperado []byte
		if plugin == pluginSHA2 {
			esperado = scrambleSHA2(nonce, c.Placeholder)
		} else {
			esperado = scrambleNative(nonce, c.Placeholder)
		}
		if subtle.ConstantTimeCompare(auth, esperado) == 1 {
			elegida = i
		}
	}
	if elegida < 0 {
		return credPG{}, len(p.my) > 0, false
	}
	return p.my[elegida], true, true
}

var (
	// errCapacidadesMy: el servidor no habla lo que el invitado eligió.
	errCapacidadesMy = errors.New("capabilities")
	// errAutenticacionMy: el servidor pidió algo que el proxy no hace.
	errAutenticacionMy = errors.New("authentication")
)

// errServidorMy es un ERR del servidor antes de autenticar: solo su código.
type errServidorMy struct {
	code     uint16
	sqlstate string
}

func (e *errServidorMy) Error() string { return fmt.Sprintf("server error %d", e.code) }

// saludoServidorMy es lo que se usa del saludo del servidor.
type saludoServidorMy struct {
	caps   uint32
	nonce  []byte
	plugin string
}

// conectarMy abre la conexión al servidor de cred, lee su saludo, negocia TLS
// (salvo UpstreamTLS "disable") y se autentica con la clave real como el
// invitado g sobre la base db. Devuelve la conexión, el método y el OK del
// servidor.
func (s *sesionMy) conectarMy(ctx context.Context, cred credPG, g respuestaMy, db string) (net.Conn, string, []byte, error) {
	p := s.p
	var c net.Conn
	var err error
	if cred.Upstream != "" {
		c, err = p.dialUp(ctx, cred.Upstream)
	} else {
		c, err = p.dialPG(ctx, "tcp", net.JoinHostPort(cred.Domain, strconv.Itoa(cred.Port)))
	}
	if err != nil {
		return nil, "", nil, err
	}
	if !s.ponerUp(c) {
		return nil, "", nil, errors.New("session closed")
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	parar := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer parar()

	// El saludo, con su longitud exacta y sin buffer (ver la cabecera).
	seq, b, err := leerPaqueteMy(c, myMaxPaquete)
	if err != nil {
		return nil, "", nil, err
	}
	sv, err := parsearSaludoMy(b)
	if err != nil {
		return nil, "", nil, err
	}
	sinTLS := cred.UpstreamTLS == UpstreamTLSDisable
	caps, err := capsUpstream(g.caps, sv.caps, db != "", !sinTLS)
	if err != nil {
		return nil, "", nil, err
	}
	charset := g.charset
	maxPaq := g.maxPaquete
	if maxPaq == 0 {
		maxPaq = myMaxPaqNorma
	}
	up := c
	if !sinTLS {
		seq++
		if _, err := c.Write(paqueteMy(seq, peticionSSLMy(caps, maxPaq, charset))); err != nil {
			return nil, "", nil, err
		}
		tc := tls.Client(c, cred.tls)
		if err := tc.HandshakeContext(ctx); err != nil {
			return nil, "", nil, fmt.Errorf("%w: %v", errTLSUpstream, err)
		}
		up = tc
		s.mu.Lock()
		s.up = tc
		s.mu.Unlock()
	}
	metodo, ok, err := autenticarMy(up, seq, caps, maxPaq, charset, cred, sv, db, sinTLS)
	if err != nil {
		return nil, "", nil, err
	}
	return up, metodo, ok, nil
}

// capsUpstream calcula las capacidades con que el proxy se presenta al
// servidor: las del invitado que el proxy ofreció, sin las del tramo del
// invitado (LENENC, atributos), más las que el proxy necesita. Falla si el
// servidor no tiene las básicas o alguna de formato que el invitado eligió.
func capsUpstream(invitado, servidor uint32, conDB, conTLS bool) (uint32, error) {
	basicas := myProtocol41 | mySecureConn | myPluginAuth
	if servidor&basicas != basicas {
		return 0, fmt.Errorf("%w: the server does not speak protocol 4.1 with plugin authentication (capabilities %#x)", errCapacidadesMy, servidor)
	}
	if conTLS && servidor&mySSL == 0 {
		return 0, fmt.Errorf("%w: the server does not offer TLS", errTLSUpstream)
	}
	faltan := invitado & myOfrecidas & myDeFormato &^ servidor
	if faltan != 0 {
		return 0, fmt.Errorf("%w: the server lacks capabilities the guest chose (%#x)", errCapacidadesMy, faltan)
	}
	caps := invitado&myOfrecidas&myDeFormato | basicas | myLongPassword
	if conDB {
		caps |= myConnectWithDB
	}
	if conTLS {
		caps |= mySSL
	}
	return caps, nil
}

// autenticarMy lleva la autenticación con el servidor hasta su OK. seq es el
// último número de secuencia usado en el tramo. sinTLS admite solo lo que no
// manda la clave (ver la cabecera).
func autenticarMy(up net.Conn, seq byte, caps, maxPaq uint32, charset byte, cred credPG, sv saludoServidorMy, db string, sinTLS bool) (string, []byte, error) {
	plugin := pluginNative
	if sv.plugin == pluginSHA2 {
		plugin = pluginSHA2
	}
	metodo := AuthMySQLNative
	var auth []byte
	if plugin == pluginSHA2 {
		auth, metodo = scrambleSHA2(sv.nonce, cred.Secret), AuthCachingSHA2Fast
	} else {
		auth = scrambleNative(sv.nonce, cred.Secret)
	}
	b := binary.LittleEndian.AppendUint32(nil, caps)
	b = binary.LittleEndian.AppendUint32(b, maxPaq)
	b = append(b, charset)
	b = append(b, make([]byte, 23)...)
	b = append(append(b, cred.User...), 0)
	b = append(append(b, byte(len(auth))), auth...)
	if caps&myConnectWithDB != 0 {
		b = append(append(b, db...), 0)
	}
	b = append(append(b, plugin...), 0)
	seq++
	if _, err := up.Write(paqueteMy(seq, b)); err != nil {
		return "", nil, err
	}
	cambios := 0
	for {
		var pk []byte
		var err error
		seq, pk, err = leerPaqueteMy(up, myMaxPaquete)
		if err != nil {
			return "", nil, err
		}
		if len(pk) == 0 {
			return "", nil, errors.New("empty packet during authentication")
		}
		enviar := func(datos []byte) error {
			seq++
			_, err := up.Write(paqueteMy(seq, datos))
			return err
		}
		switch pk[0] {
		case myOKCodigo:
			return metodo, pk, nil
		case myErrCodigo:
			return "", nil, parsearErrMy(pk)
		case myCambio: // AuthSwitchRequest
			cambios++
			if cambios > myMaxCambios || len(pk) < 2 {
				return "", nil, fmt.Errorf("%w: unexpected authentication switch", errAutenticacionMy)
			}
			nombre, datos, ok := cadenaMy(pk[1:])
			if !ok {
				return "", nil, fmt.Errorf("%w: malformed authentication switch", errAutenticacionMy)
			}
			datos = sinNulFinal(datos)
			plugin = nombre
			switch nombre {
			case pluginNative:
				if len(datos) < 20 {
					return "", nil, fmt.Errorf("%w: short nonce in the authentication switch", errAutenticacionMy)
				}
				metodo = AuthMySQLNative
				err = enviar(scrambleNative(datos[:20], cred.Secret))
			case pluginSHA2:
				if len(datos) < 20 {
					return "", nil, fmt.Errorf("%w: short nonce in the authentication switch", errAutenticacionMy)
				}
				metodo = AuthCachingSHA2Fast
				err = enviar(scrambleSHA2(datos[:20], cred.Secret))
			case pluginLimpio:
				if sinTLS {
					return "", nil, fmt.Errorf("%w: the server asked for the password in cleartext and the upstream has no TLS", errAutenticacionMy)
				}
				metodo = AuthMySQLClear
				err = enviar(append([]byte(cred.Secret), 0))
			default:
				return "", nil, fmt.Errorf("%w: the server asked for the %q plugin, which the proxy does not support", errAutenticacionMy, recortar(nombre, 64))
			}
			if err != nil {
				return "", nil, err
			}
		case myMasDatos: // AuthMoreData de caching_sha2_password
			if plugin != pluginSHA2 || len(pk) != 2 {
				return "", nil, fmt.Errorf("%w: unexpected extra authentication data", errAutenticacionMy)
			}
			switch pk[1] {
			case sha2RapidoOK:
				// Viene el OK detrás.
			case sha2Completa:
				if sinTLS {
					return "", nil, fmt.Errorf("%w: the server asked for caching_sha2_password full authentication, which needs TLS (the proxy does not send the password without it; connect once over TLS to warm the server cache, or use mysql_native_password)", errAutenticacionMy)
				}
				metodo = AuthCachingSHA2
				if err := enviar(append([]byte(cred.Secret), 0)); err != nil {
					return "", nil, err
				}
			default:
				return "", nil, fmt.Errorf("%w: unexpected caching_sha2_password state %d", errAutenticacionMy, pk[1])
			}
		default:
			return "", nil, fmt.Errorf("%w: unexpected packet %#x during authentication", errAutenticacionMy, pk[0])
		}
	}
}

// ── paquetes ────────────────────────────────────────────────────────────────

// leerPaqueteMy lee un paquete (3 bytes de longitud, 1 de secuencia, datos).
// Uno de más de max bytes es un error: antes de autenticar no hace falta más,
// y así tampoco llegan paquetes partidos (0xffffff).
func leerPaqueteMy(r io.Reader, max int) (byte, []byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	if n > max {
		return 0, nil, fmt.Errorf("packet of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	return h[3], b, nil
}

func paqueteMy(seq byte, datos []byte) []byte {
	n := len(datos)
	b := make([]byte, 4, 4+n)
	b[0], b[1], b[2], b[3] = byte(n), byte(n>>8), byte(n>>16), seq
	return append(b, datos...)
}

// errMy es un paquete ERR (protocolo 4.1).
func errMy(code uint16, sqlstate, msg string) []byte {
	b := []byte{myErrCodigo}
	b = binary.LittleEndian.AppendUint16(b, code)
	b = append(b, '#')
	b = append(b, sqlstate...)
	return append(b, msg...)
}

// parsearErrMy saca de un ERR del servidor el código y el SQLSTATE (solo si
// tiene la forma de uno). El texto no sale de aquí.
func parsearErrMy(pk []byte) *errServidorMy {
	e := &errServidorMy{}
	if len(pk) >= 3 {
		e.code = binary.LittleEndian.Uint16(pk[1:3])
	}
	if len(pk) >= 9 && pk[3] == '#' {
		st := pk[4:9]
		valido := true
		for _, c := range st {
			if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
				valido = false
			}
		}
		if valido {
			e.sqlstate = string(st)
		}
	}
	return e
}

func codigoMy(code uint16) string {
	if code == 0 {
		return ""
	}
	return " (upstream error " + strconv.Itoa(int(code)) + ")"
}

// nonceMy son 20 bytes aleatorios en ASCII imprimible: hay clientes antiguos
// que leen la segunda parte del nonce del saludo como cadena C.
func nonceMy() ([]byte, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	for i := range b {
		b[i] = '!' + b[i]%94
	}
	return b, nil
}

// saludoMy es el saludo del proxy (HandshakeV10) con ese nonce.
func saludoMy(nonce []byte) []byte {
	b := []byte{10}
	b = append(append(b, myVersion...), 0)
	b = binary.LittleEndian.AppendUint32(b, myIDConexion)
	b = append(b, nonce[:8]...)
	b = append(b, 0)
	b = binary.LittleEndian.AppendUint16(b, uint16(myOfrecidas&0xffff))
	b = append(b, myCharset)
	b = binary.LittleEndian.AppendUint16(b, myAutocommit)
	b = binary.LittleEndian.AppendUint16(b, uint16(myOfrecidas>>16))
	b = append(b, byte(len(nonce)+1))
	b = append(b, make([]byte, 10)...)
	b = append(append(b, nonce[8:]...), 0)
	return append(append(b, pluginNative...), 0)
}

// peticionSSLMy es el SSLRequest: la cabecera de la respuesta, sin más.
func peticionSSLMy(caps, maxPaq uint32, charset byte) []byte {
	b := binary.LittleEndian.AppendUint32(nil, caps)
	b = binary.LittleEndian.AppendUint32(b, maxPaq)
	b = append(b, charset)
	return append(b, make([]byte, 23)...)
}

// parsearRespuestaMy lee la respuesta del invitado al saludo.
func parsearRespuestaMy(b []byte) (respuestaMy, error) {
	var r respuestaMy
	if len(b) < 32 {
		return r, errors.New("malformed handshake response")
	}
	caps := binary.LittleEndian.Uint32(b)
	if caps&mySSL != 0 {
		return r, errors.New("TLS is not available between the guest and the proxy (use ssl-mode DISABLED or PREFERRED; the proxy uses TLS towards the server)")
	}
	if caps&myProtocol41 == 0 {
		return r, errors.New("protocol 4.1 is required")
	}
	r.caps = caps & myOfrecidas
	r.maxPaquete = binary.LittleEndian.Uint32(b[4:])
	r.charset = b[8]
	resto := b[32:]
	user, resto, ok := cadenaMy(resto)
	if !ok {
		return r, errors.New("malformed handshake response")
	}
	if err := validarNombreMy(user, myMaxUsuario); err != nil {
		return r, fmt.Errorf("invalid user name: %v", err)
	}
	r.user = user
	switch {
	case caps&myAuthLenenc != 0:
		n, k, ok := lenencMy(resto)
		if !ok || n > myMaxAuth || uint64(len(resto)-k) < n {
			return r, errors.New("malformed auth response")
		}
		r.auth, resto = resto[k:k+int(n)], resto[k+int(n):]
	case caps&mySecureConn != 0:
		if len(resto) < 1 || len(resto)-1 < int(resto[0]) {
			return r, errors.New("malformed auth response")
		}
		n := int(resto[0])
		r.auth, resto = resto[1:1+n], resto[1+n:]
	default:
		return r, errors.New("secure connection authentication is required")
	}
	if caps&myConnectWithDB != 0 && len(resto) > 0 {
		db, rr, ok := cadenaMy(resto)
		if !ok {
			return r, errors.New("malformed database name")
		}
		r.db, resto = db, rr
	}
	if caps&myPluginAuth != 0 && len(resto) > 0 {
		pl, _, ok := cadenaMy(resto)
		if !ok {
			return r, errors.New("malformed auth plugin name")
		}
		r.plugin = pl
	}
	// Los atributos de conexión (si el cliente los manda sin que se le
	// ofrecieran) se ignoran: no se reenvían al servidor.
	return r, nil
}

// parsearSaludoMy lee el saludo del servidor. Un ERR en su lugar ("too many
// connections", "host blocked") es un errServidorMy con su código.
func parsearSaludoMy(b []byte) (saludoServidorMy, error) {
	var sv saludoServidorMy
	if len(b) == 0 {
		return sv, errors.New("empty server greeting")
	}
	if b[0] == myErrCodigo {
		return sv, parsearErrMy(b)
	}
	if b[0] != 10 {
		return sv, fmt.Errorf("unsupported server protocol %d", b[0])
	}
	_, resto, ok := cadenaMy(b[1:])
	if !ok || len(resto) < 4+8+1+2 {
		return sv, errors.New("malformed server greeting")
	}
	resto = resto[4:] // id de conexión
	parte1 := resto[:8]
	resto = resto[9:]
	sv.caps = uint32(binary.LittleEndian.Uint16(resto))
	resto = resto[2:]
	if len(resto) < 1+2+2+1+10 {
		return sv, errors.New("server greeting without capabilities")
	}
	sv.caps |= uint32(binary.LittleEndian.Uint16(resto[3:])) << 16
	largo := int(resto[5])
	resto = resto[16:]
	n := largo - 8
	if n < 13 {
		n = 13
	}
	if sv.caps&mySecureConn == 0 || len(resto) < n {
		return sv, errors.New("server greeting without a 20-byte nonce")
	}
	nonce := append(append([]byte(nil), parte1...), sinNulFinal(resto[:n])...)
	if len(nonce) < 20 {
		return sv, errors.New("server greeting without a 20-byte nonce")
	}
	sv.nonce = nonce[:20]
	resto = resto[n:]
	if sv.caps&myPluginAuth != 0 {
		// Hay servidores que no terminan el nombre con NUL: vale el resto.
		if i := indexByte(resto, 0); i >= 0 {
			resto = resto[:i]
		}
		sv.plugin = string(resto)
	}
	return sv, nil
}

// cadenaMy parte una cadena C.
func cadenaMy(b []byte) (string, []byte, bool) {
	if i := indexByte(b, 0); i >= 0 {
		return string(b[:i]), b[i+1:], true
	}
	return "", nil, false
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func sinNulFinal(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == 0 {
		return b[:len(b)-1]
	}
	return b
}

// lenencMy lee un entero de longitud codificada: valor y bytes que ocupa.
func lenencMy(b []byte) (uint64, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	switch c := b[0]; {
	case c < 0xfb:
		return uint64(c), 1, true
	case c == 0xfc && len(b) >= 3:
		return uint64(binary.LittleEndian.Uint16(b[1:])), 3, true
	case c == 0xfd && len(b) >= 4:
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16, 4, true
	case c == 0xfe && len(b) >= 9:
		return binary.LittleEndian.Uint64(b[1:]), 9, true
	}
	return 0, 0, false
}

// ── scrambles ───────────────────────────────────────────────────────────────

// scrambleNative es la respuesta de mysql_native_password:
// SHA1(clave) XOR SHA1(nonce || SHA1(SHA1(clave))).
func scrambleNative(nonce []byte, clave string) []byte {
	s1 := sha1.Sum([]byte(clave))
	s2 := sha1.Sum(s1[:])
	h := sha1.New()
	h.Write(nonce)
	h.Write(s2[:])
	out := h.Sum(nil)
	for i := range out {
		out[i] ^= s1[i]
	}
	return out
}

// scrambleSHA2 es la de caching_sha2_password (ruta rápida):
// SHA256(clave) XOR SHA256(SHA256(SHA256(clave)) || nonce).
func scrambleSHA2(nonce []byte, clave string) []byte {
	s1 := sha256.Sum256([]byte(clave))
	s2 := sha256.Sum256(s1[:])
	h := sha256.New()
	h.Write(s2[:])
	h.Write(nonce)
	out := h.Sum(nil)
	for i := range out {
		out[i] ^= s1[i]
	}
	return out
}
