// Package pgmini es un cliente PostgreSQL mínimo, solo con la biblioteca
// estándar, para el banco de pruebas: StartupMessage, autenticación (trust,
// contraseña en claro, SCRAM-SHA-256 o SCRAM-SHA-256-PLUS), consulta simple y
// lectura de DataRow. TLS (SSLRequest + TLS 1.2+) es opcional: el banco habla
// con un invitado o un contenedor del mismo host sin él; el doctor lo usa
// contra bases remotas.
// No es un cliente de producción: sin consultas extendidas, sin COPY, sin
// tipos; todo valor vuelve como texto.
package pgmini

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// maxMsg acota el cuerpo de un mensaje del servidor: uno hostil no nos hace
// reservar gigabytes.
const maxMsg = 16 << 20

// Config es lo que hace falta para conectar.
type Config struct {
	Addr     string // host:puerto
	User     string
	Password string
	Database string
	// Timeout acota conectar+autenticar y cada Query (por defecto 30 s). Si el
	// contexto trae un plazo más corto, manda el del contexto.
	Timeout time.Duration
	// NoCleartext rechaza la autenticación con contraseña en claro: sin TLS,
	// esa contraseña viajaría legible por la red. Solo SCRAM (o trust).
	NoCleartext bool
	// TLSMode activa TLS: "" (sin TLS), "require" (cifra sin verificar al
	// servidor: no frena a un intermediario), "verify-ca" (cadena válida, sin
	// comprobar el nombre) o "verify-full" (cadena y nombre). Si el servidor
	// no acepta TLS, Dial falla: nunca se degrada en silencio.
	TLSMode string
	// TLSServerName es el nombre que se verifica y va en el SNI (por defecto,
	// el host de Addr).
	TLSServerName string
	// RootCAs son las raíces de confianza (nil: las del sistema).
	RootCAs *x509.CertPool
}

// Modos de TLS de Config.TLSMode.
const (
	TLSRequire    = "require"
	TLSVerifyCA   = "verify-ca"
	TLSVerifyFull = "verify-full"
)

// sslRequestCode es el código del SSLRequest (80877103 = 1234,5679).
const sslRequestCode = 80877103

// Conn es una conexión. No es segura para uso concurrente.
type Conn struct {
	nc      net.Conn
	r       *bufio.Reader
	timeout time.Duration
	stop    func() bool
	// cb son los datos tls-server-end-point del certificado del servidor
	// (nil sin TLS) para SCRAM-SHA-256-PLUS.
	cb []byte
}

// Error es un ErrorResponse del servidor.
type Error struct{ Severity, Code, Message string }

func (e *Error) Error() string {
	return fmt.Sprintf("postgres: %s %s: %s", e.Severity, e.Code, e.Message)
}

// Dial conecta, se autentica y devuelve la conexión lista para consultas.
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	nc, err := d.DialContext(dctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	c := &Conn{nc: nc, r: bufio.NewReader(nc), timeout: cfg.Timeout}
	// Cancelar el contexto cierra el socket y desbloquea cualquier lectura.
	c.stop = context.AfterFunc(ctx, func() { nc.Close() })
	c.plazo(ctx)
	if cfg.TLSMode != "" {
		if err := c.iniciarTLS(dctx, cfg); err != nil {
			c.Close()
			return nil, err
		}
		c.plazo(ctx)
	}
	if err := c.arranque(cfg); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// tlsConfig arma la configuración del cliente según el modo. TLS 1.2 como
// mínimo. require no verifica nada; verify-ca comprueba la cadena pero no el
// nombre; verify-full, las dos cosas.
func tlsConfig(cfg Config) (*tls.Config, error) {
	name := cfg.TLSServerName
	if name == "" {
		h, _, err := net.SplitHostPort(cfg.Addr)
		if err != nil {
			return nil, err
		}
		name = h
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: name, RootCAs: cfg.RootCAs}
	switch cfg.TLSMode {
	case TLSVerifyFull:
	case TLSRequire:
		tc.InsecureSkipVerify = true // pedido explícito: cifrar sin autenticar al servidor
	case TLSVerifyCA:
		// El nombre no se comprueba, la cadena sí: se verifica a mano porque
		// InsecureSkipVerify desactiva también la verificación de la cadena.
		tc.InsecureSkipVerify = true
		tc.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("postgres: the server sent no certificate")
			}
			inter := x509.NewCertPool()
			for _, ic := range cs.PeerCertificates[1:] {
				inter.AddCert(ic)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: cfg.RootCAs, Intermediates: inter})
			return err
		}
	default:
		return nil, fmt.Errorf("postgres: unknown TLS mode %q", cfg.TLSMode)
	}
	return tc, nil
}

// iniciarTLS negocia TLS con un SSLRequest: 'S' sigue con el handshake; 'N'
// (el servidor no habla TLS) o cualquier otra cosa es un error, no una
// degradación a texto claro.
func (c *Conn) iniciarTLS(ctx context.Context, cfg Config) error {
	tc, err := tlsConfig(cfg)
	if err != nil {
		return err
	}
	req := binary.BigEndian.AppendUint32(nil, 8)
	req = binary.BigEndian.AppendUint32(req, sslRequestCode)
	if _, err := c.nc.Write(req); err != nil {
		return err
	}
	// Se lee directo del socket, sin bufio: nada de lo que el servidor mande
	// después de la 'S' puede quedarse como texto claro en un búfer.
	var b [1]byte
	if _, err := io.ReadFull(c.nc, b[:]); err != nil {
		return err
	}
	switch b[0] {
	case 'S':
	case 'N':
		return errors.New("postgres: the server does not accept TLS")
	default:
		return errors.New("postgres: unexpected reply to SSLRequest")
	}
	if c.r.Buffered() > 0 {
		return errors.New("postgres: data after the SSLRequest reply")
	}
	tconn := tls.Client(c.nc, tc)
	if err := tconn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("postgres: TLS handshake: %w", err)
	}
	if peer := tconn.ConnectionState().PeerCertificates; len(peer) > 0 {
		c.cb = tlsServerEndPoint(peer[0])
	}
	c.nc = tconn
	c.r = bufio.NewReader(tconn)
	return nil
}

// Close cierra la conexión (con Terminate, si se puede).
func (c *Conn) Close() error {
	c.stop()
	_ = c.nc.SetWriteDeadline(time.Now().Add(time.Second))
	_ = c.escribir('X', nil)
	return c.nc.Close()
}

func (c *Conn) plazo(ctx context.Context) {
	d := time.Now().Add(c.timeout)
	if cd, ok := ctx.Deadline(); ok && cd.Before(d) {
		d = cd
	}
	_ = c.nc.SetDeadline(d)
}

func (c *Conn) escribir(tipo byte, cuerpo []byte) error {
	buf := make([]byte, 0, 5+len(cuerpo))
	if tipo != 0 { // el StartupMessage no lleva tipo
		buf = append(buf, tipo)
	}
	buf = binary.BigEndian.AppendUint32(buf, uint32(4+len(cuerpo)))
	buf = append(buf, cuerpo...)
	_, err := c.nc.Write(buf)
	return err
}

func (c *Conn) leer() (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(h[1:])) - 4
	if n < 0 || n > maxMsg {
		return 0, nil, fmt.Errorf("postgres: message of %d bytes", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return 0, nil, err
	}
	return h[0], body, nil
}

func (c *Conn) arranque(cfg Config) error {
	var b []byte
	b = binary.BigEndian.AppendUint32(b, 196608) // protocolo 3.0
	for _, kv := range [][2]string{{"user", cfg.User}, {"database", cfg.Database}, {"client_encoding", "UTF8"}} {
		if kv[1] == "" {
			continue
		}
		b = append(append(b, kv[0]...), 0)
		b = append(append(b, kv[1]...), 0)
	}
	b = append(b, 0)
	if err := c.escribir(0, b); err != nil {
		return err
	}
	var sc *scram
	for {
		t, body, err := c.leer()
		if err != nil {
			return err
		}
		switch t {
		case 'E':
			return parseError(body)
		case 'R':
			if len(body) < 4 {
				return errors.New("postgres: short Authentication message")
			}
			switch binary.BigEndian.Uint32(body) {
			case 0: // AuthenticationOk
			case 3: // contraseña en claro
				if cfg.NoCleartext {
					return errors.New("postgres: the server asks for a cleartext password; refused")
				}
				if err := c.escribir('p', append([]byte(cfg.Password), 0)); err != nil {
					return err
				}
			case 10: // SASL: elegir SCRAM-SHA-256
				// Con TLS y -PLUS ofrecido, se usa -PLUS (channel binding).
				mec := "SCRAM-SHA-256"
				plus := c.cb != nil && ofrece(body[4:], "SCRAM-SHA-256-PLUS")
				if plus {
					mec = "SCRAM-SHA-256-PLUS"
				} else if !ofrece(body[4:], "SCRAM-SHA-256") {
					return errors.New("postgres: the server offers no SCRAM-SHA-256")
				}
				if sc, err = nuevoScram(cfg.Password); err != nil {
					return err
				}
				sc.conBinding(c.cb, plus)
				ini := sc.primero()
				m := append([]byte(mec+"\x00"), binary.BigEndian.AppendUint32(nil, uint32(len(ini)))...)
				if err := c.escribir('p', append(m, ini...)); err != nil {
					return err
				}
			case 11: // SASLContinue
				if sc == nil {
					return errors.New("postgres: SASLContinue without SASL")
				}
				fin, err := sc.final(body[4:])
				if err != nil {
					return err
				}
				if err := c.escribir('p', fin); err != nil {
					return err
				}
			case 12: // SASLFinal
				if sc == nil {
					return errors.New("postgres: SASLFinal without SASL")
				}
				if err := sc.verificar(body[4:]); err != nil {
					return err
				}
			default:
				return fmt.Errorf("postgres: unsupported authentication method %d", binary.BigEndian.Uint32(body))
			}
		case 'Z':
			return nil
		}
		// 'S' (ParameterStatus), 'K' (BackendKeyData), 'N' (Notice): se ignoran.
	}
}

// ofrece dice si la lista de mecanismos SASL (cadenas terminadas en 0) trae m.
func ofrece(lista []byte, m string) bool {
	for _, s := range strings.Split(string(lista), "\x00") {
		if s == m {
			return true
		}
	}
	return false
}

func parseError(body []byte) error {
	e := &Error{}
	for len(body) > 1 {
		campo := body[0]
		i := 1
		for i < len(body) && body[i] != 0 {
			i++
		}
		v := string(body[1:i])
		switch campo {
		case 'S':
			e.Severity = v
		case 'C':
			e.Code = v
		case 'M':
			e.Message = v
		}
		if i+1 > len(body) {
			break
		}
		body = body[i+1:]
	}
	return e
}

// Query ejecuta sql con el protocolo de consulta simple (admite varias
// sentencias) y devuelve las filas de TODOS los resultados, en texto; NULL
// vuelve como cadena vacía. Si el servidor contesta un error, se sigue leyendo
// hasta ReadyForQuery para dejar la conexión usable, y se devuelve el error.
func (c *Conn) Query(ctx context.Context, sql string) ([][]string, error) {
	c.plazo(ctx)
	if err := c.escribir('Q', append([]byte(sql), 0)); err != nil {
		return nil, err
	}
	var rows [][]string
	var qerr error
	for {
		t, body, err := c.leer()
		if err != nil {
			return nil, err
		}
		switch t {
		case 'D':
			row, err := parseDataRow(body)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		case 'E':
			if qerr == nil {
				qerr = parseError(body)
			}
		case 'Z':
			return rows, qerr
		}
	}
}

func parseDataRow(b []byte) ([]string, error) {
	if len(b) < 2 {
		return nil, errors.New("postgres: short DataRow")
	}
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	row := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if len(b) < 4 {
			return nil, errors.New("postgres: truncated DataRow")
		}
		l := int32(binary.BigEndian.Uint32(b))
		b = b[4:]
		if l < 0 {
			row = append(row, "")
			continue
		}
		if int(l) > len(b) {
			return nil, errors.New("postgres: truncated DataRow value")
		}
		row = append(row, string(b[:l]))
		b = b[l:]
	}
	return row, nil
}
