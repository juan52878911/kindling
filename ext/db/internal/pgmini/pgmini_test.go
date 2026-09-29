package pgmini

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// Vector de la RFC 7677 §3: usuario "user", contraseña "pencil".
func TestScramVectorRFC7677(t *testing.T) {
	s := &scram{clave: "pencil", usuario: "user", nonce: "rOprNGfwEbeRWgbNEkqO"}
	if got := string(s.primero()); got != "n,,n=user,r=rOprNGfwEbeRWgbNEkqO" {
		t.Fatalf("client-first = %q", got)
	}
	sf := "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	fin, err := s.final([]byte(sf))
	if err != nil {
		t.Fatal(err)
	}
	want := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if string(fin) != want {
		t.Fatalf("client-final = %q", fin)
	}
	if err := s.verificar([]byte("v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=")); err != nil {
		t.Fatal(err)
	}
	if err := s.verificar([]byte("v=AAAA")); err == nil {
		t.Fatal("a wrong server signature must fail")
	}
}

func TestScramRechazaServidorMalo(t *testing.T) {
	mk := func() *scram { return &scram{clave: "x", nonce: "abc"} }
	for name, sf := range map[string]string{
		"nonce que no extiende": "r=zzz,s=c2Fs,i=4096",
		"sal vacía":             "r=abcdef,s=,i=4096",
		"pocas iteraciones":     "r=abcdef,s=c2Fs,i=10",
		"demasiadas":            "r=abcdef,s=c2Fs,i=99999999",
		"malformado":            "basura",
	} {
		s := mk()
		s.primero()
		if _, err := s.final([]byte(sf)); err == nil {
			t.Errorf("%s: se aceptó", name)
		}
	}
}

// falso es un servidor mínimo: una conexión, un guion.
type falso struct {
	ln   net.Listener
	auth string // "trust" | "clear" | "scram"
	pass string
	// respuesta a una consulta: filas de una columna
	filas []string
	err   bool
	// TLS: tlsCfg != nil acepta el SSLRequest; nil contesta 'N'. plus ofrece
	// SCRAM-SHA-256-PLUS. mec y gs2 son lo que el cliente eligió.
	tlsCfg *tls.Config
	plus   bool
	mu     sync.Mutex
	mec    string
	gs2    string
}

func (f *falso) eleccion() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mec, f.gs2
}

func nuevoFalso(t *testing.T, auth, pass string) *falso {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("sin red loopback:", err)
	}
	f := &falso{ln: ln, auth: auth, pass: pass}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.atender(c)
		}
	}()
	return f
}

func msg(t byte, body []byte) []byte {
	b := []byte{t}
	b = binary.BigEndian.AppendUint32(b, uint32(4+len(body)))
	return append(b, body...)
}

func auth(code uint32, extra string) []byte {
	return msg('R', append(binary.BigEndian.AppendUint32(nil, code), extra...))
}

func (f *falso) atender(c net.Conn) {
	defer func() { c.Close() }()
	r := bufio.NewReader(c)
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return
	}
	if binary.BigEndian.Uint32(l[:]) == 8 { // SSLRequest
		var code [4]byte
		if _, err := io.ReadFull(r, code[:]); err != nil || binary.BigEndian.Uint32(code[:]) != sslRequestCode {
			return
		}
		if f.tlsCfg == nil {
			c.Write([]byte("N"))
		} else {
			c.Write([]byte("S"))
			tc := tls.Server(c, f.tlsCfg)
			if tc.Handshake() != nil {
				return
			}
			c = tc
			r = bufio.NewReader(tc)
		}
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return
		}
	}
	io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint32(l[:]))-4) // StartupMessage
	leerP := func() []byte {
		var h [5]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil
		}
		b := make([]byte, binary.BigEndian.Uint32(h[1:])-4)
		io.ReadFull(r, b)
		return b
	}
	mal := func() {
		c.Write(msg('E', []byte("SFATAL\x00C28P01\x00Mpassword authentication failed\x00\x00")))
	}
	switch f.auth {
	case "clear":
		c.Write(auth(3, ""))
		if p := leerP(); string(p) != f.pass+"\x00" {
			mal()
			return
		}
	case "scram":
		mecs := "SCRAM-SHA-256\x00"
		if f.plus {
			mecs = "SCRAM-SHA-256-PLUS\x00" + mecs
		}
		c.Write(auth(10, mecs+"\x00"))
		p := leerP()
		i := strings.Index(string(p), "\x00")
		cf := string(p[i+1+4:]) // tras el mecanismo y la longitud
		partes := strings.SplitN(cf, ",", 3)
		gs2 := partes[0] + "," + partes[1] + ","
		bare := partes[2]
		f.mu.Lock()
		f.mec, f.gs2 = string(p[:i]), gs2
		f.mu.Unlock()
		cbind := gs2
		if strings.HasPrefix(gs2, "p=") {
			cbind += string(tlsServerEndPoint(f.tlsCfg.Certificates[0].Leaf))
		}
		cn := strings.TrimPrefix(strings.Split(bare, ",")[1], "r=")
		nonce := cn + "SERVERPART"
		sal := []byte("salsalsal")
		sf := "r=" + nonce + ",s=" + base64.StdEncoding.EncodeToString(sal) + ",i=4096"
		c.Write(auth(11, sf))
		fin := string(leerP())
		sinPrueba := "c=" + base64.StdEncoding.EncodeToString([]byte(cbind)) + ",r=" + nonce
		salted, _ := pbkdf2.Key(sha256.New, f.pass, sal, 4096, 32)
		ck := hmacSHA256(salted, "Client Key")
		sk := sha256.Sum256(ck)
		authMsg := bare + "," + sf + "," + sinPrueba
		sig := hmacSHA256(sk[:], authMsg)
		esperada := make([]byte, 32)
		for j := range ck {
			esperada[j] = ck[j] ^ sig[j]
		}
		if fin != sinPrueba+",p="+base64.StdEncoding.EncodeToString(esperada) {
			mal()
			return
		}
		srv := hmacSHA256(hmacSHA256(salted, "Server Key"), authMsg)
		c.Write(auth(12, "v="+base64.StdEncoding.EncodeToString(srv)))
	}
	c.Write(auth(0, ""))
	c.Write(msg('S', []byte("server_version\x0016.0\x00")))
	c.Write(msg('Z', []byte("I")))
	for {
		q := leerP()
		if q == nil {
			return
		}
		if f.err {
			c.Write(msg('E', []byte("SERROR\x00C42P01\x00Mrelation does not exist\x00\x00")))
			c.Write(msg('Z', []byte("I")))
			continue
		}
		c.Write(msg('T', []byte{0, 0})) // RowDescription (se ignora)
		for _, v := range f.filas {
			row := binary.BigEndian.AppendUint16(nil, 1)
			if v == "NULL" {
				row = binary.BigEndian.AppendUint32(row, 0xFFFFFFFF)
			} else {
				row = binary.BigEndian.AppendUint32(row, uint32(len(v)))
				row = append(row, v...)
			}
			c.Write(msg('D', row))
		}
		c.Write(msg('C', []byte("SELECT 1\x00")))
		c.Write(msg('Z', []byte("I")))
	}
}

func dial(t *testing.T, f *falso, pass string) (*Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return Dial(ctx, Config{Addr: f.ln.Addr().String(), User: "u", Password: pass, Database: "d", Timeout: 5 * time.Second})
}

func TestQueryContraServidorFalso(t *testing.T) {
	for _, auth := range []string{"trust", "clear", "scram"} {
		t.Run(auth, func(t *testing.T) {
			f := nuevoFalso(t, auth, "s3cret")
			f.filas = []string{"42", "NULL"}
			c, err := dial(t, f, "s3cret")
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			rows, err := c.Query(context.Background(), "SELECT count(*) FROM t")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 || rows[0][0] != "42" || rows[1][0] != "" {
				t.Fatalf("rows = %v", rows)
			}
			// La conexión sigue usable tras la consulta.
			if _, err := c.Query(context.Background(), "SELECT 1"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContrasenaMala(t *testing.T) {
	for _, a := range []string{"clear", "scram"} {
		f := nuevoFalso(t, a, "buena")
		if _, err := dial(t, f, "mala"); err == nil {
			t.Fatalf("%s: la contraseña mala se aceptó", a)
		}
	}
}

func TestNoCleartextNoEnviaLaClave(t *testing.T) {
	f := nuevoFalso(t, "clear", "s3cret")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := Config{Addr: f.ln.Addr().String(), User: "u", Password: "s3cret", Database: "d", Timeout: 5 * time.Second, NoCleartext: true}
	if c, err := Dial(ctx, cfg); err == nil {
		c.Close()
		t.Fatal("NoCleartext aceptó la contraseña en claro")
	}
}

func TestErrorDeConsultaDejaLaConexionUsable(t *testing.T) {
	f := nuevoFalso(t, "trust", "")
	f.err = true
	c, err := dial(t, f, "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Query(context.Background(), "SELECT * FROM nada")
	pe, ok := err.(*Error)
	if !ok || pe.Code != "42P01" {
		t.Fatalf("err = %v", err)
	}
	f.err = false
	f.filas = []string{"1"}
	if rows, err := c.Query(context.Background(), "SELECT 1"); err != nil || len(rows) != 1 {
		t.Fatalf("tras el error: %v %v", rows, err)
	}
}

func TestDataRowTruncada(t *testing.T) {
	for _, b := range [][]byte{nil, {0, 1}, {0, 1, 0, 0, 0, 9, 'a'}} {
		if _, err := parseDataRow(b); err == nil {
			t.Errorf("%v: se aceptó", b)
		}
	}
}

func TestMensajeGigante(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	go func() {
		c2.Write([]byte{'D', 0x7f, 0xff, 0xff, 0xff})
		c2.Close()
	}()
	c := &Conn{nc: c1, r: bufio.NewReader(c1)}
	if _, _, err := c.leer(); err == nil {
		t.Fatal("un mensaje de 2 GiB se aceptó")
	}
}

func TestCancelarDesbloquea(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go func() { // acepta y calla
		c, _ := ln.Accept()
		if c != nil {
			time.Sleep(5 * time.Second)
			c.Close()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if _, err := Dial(ctx, Config{Addr: ln.Addr().String(), User: "u", Timeout: 10 * time.Second}); err == nil {
		t.Fatal("debía fallar")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancelar no desbloqueó")
	}
}

// ── TLS ──────────────────────────────────────────────────────────────────────

// pki genera una CA y un certificado de servidor para db.test y 127.0.0.1.
func pki(t *testing.T, ca *ecdsa.PrivateKey, caCert *x509.Certificate) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	nueva := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	if ca == nil {
		ca = nueva()
		tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
		der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &ca.PublicKey, ca)
		if err != nil {
			t.Fatal(err)
		}
		caCert, _ = x509.ParseCertificate(der)
	}
	k := nueva()
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "db.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"db.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &k.PublicKey, ca)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k, Leaf: leaf}, pool
}

func falsoTLS(t *testing.T, auth, pass string, plus bool) (*falso, *x509.CertPool) {
	cert, pool := pki(t, nil, nil)
	f := nuevoFalso(t, auth, pass)
	f.tlsCfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	f.plus = plus
	return f, pool
}

func dialTLS(t *testing.T, f *falso, cfg Config) (*Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cfg.Addr, cfg.User, cfg.Database, cfg.Timeout = f.ln.Addr().String(), "u", "d", 5*time.Second
	return Dial(ctx, cfg)
}

func TestTLSVerifyFullConScramPlus(t *testing.T) {
	f, pool := falsoTLS(t, "scram", "s3cret", true)
	f.filas = []string{"7"}
	c, err := dialTLS(t, f, Config{Password: "s3cret", TLSMode: TLSVerifyFull, TLSServerName: "db.test", RootCAs: pool})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if mec, gs2 := f.eleccion(); mec != "SCRAM-SHA-256-PLUS" || gs2 != "p=tls-server-end-point,," {
		t.Fatalf("mecanismo = %q gs2 = %q", mec, gs2)
	}
	if rows, err := c.Query(context.Background(), "SELECT 7"); err != nil || len(rows) != 1 || rows[0][0] != "7" {
		t.Fatalf("%v %v", rows, err)
	}
}

func TestTLSScramSinPlusUsaY(t *testing.T) {
	f, pool := falsoTLS(t, "scram", "s3cret", false)
	c, err := dialTLS(t, f, Config{Password: "s3cret", TLSMode: TLSVerifyFull, TLSServerName: "db.test", RootCAs: pool})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if mec, gs2 := f.eleccion(); mec != "SCRAM-SHA-256" || gs2 != "y,," {
		t.Fatalf("mecanismo = %q gs2 = %q", mec, gs2)
	}
}

func TestTLSVerifyFullPorIPDeLaDireccion(t *testing.T) {
	// Sin TLSServerName se verifica el host de Addr (127.0.0.1, en el SAN).
	f, pool := falsoTLS(t, "trust", "", false)
	c, err := dialTLS(t, f, Config{TLSMode: TLSVerifyFull, RootCAs: pool})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestTLSVerifyFullRechaza(t *testing.T) {
	f, pool := falsoTLS(t, "trust", "", false)
	// Nombre que no está en el certificado.
	if c, err := dialTLS(t, f, Config{TLSMode: TLSVerifyFull, TLSServerName: "otro.test", RootCAs: pool}); err == nil {
		c.Close()
		t.Fatal("verify-full aceptó un nombre que no es el del certificado")
	}
	// CA que no firmó el certificado.
	_, otra := pki(t, nil, nil)
	if c, err := dialTLS(t, f, Config{TLSMode: TLSVerifyFull, TLSServerName: "db.test", RootCAs: otra}); err == nil {
		c.Close()
		t.Fatal("verify-full aceptó una CA ajena")
	}
}

func TestTLSVerifyCAyRequire(t *testing.T) {
	f, pool := falsoTLS(t, "trust", "", false)
	// verify-ca: nombre distinto vale, cadena mala no.
	c, err := dialTLS(t, f, Config{TLSMode: TLSVerifyCA, TLSServerName: "otro.test", RootCAs: pool})
	if err != nil {
		t.Fatalf("verify-ca con otro nombre: %v", err)
	}
	c.Close()
	_, otra := pki(t, nil, nil)
	if c, err := dialTLS(t, f, Config{TLSMode: TLSVerifyCA, RootCAs: otra}); err == nil {
		c.Close()
		t.Fatal("verify-ca aceptó una CA ajena")
	}
	// require no verifica nada.
	c, err = dialTLS(t, f, Config{TLSMode: TLSRequire})
	if err != nil {
		t.Fatalf("require: %v", err)
	}
	c.Close()
}

func TestTLSServidorSinTLSNoDegrada(t *testing.T) {
	f := nuevoFalso(t, "trust", "") // contesta 'N'
	if c, err := dialTLS(t, f, Config{TLSMode: TLSVerifyFull}); err == nil {
		c.Close()
		t.Fatal("se degradó a texto claro")
	}
}

func TestTLSModoDesconocido(t *testing.T) {
	f, _ := falsoTLS(t, "trust", "", false)
	if c, err := dialTLS(t, f, Config{TLSMode: "prefer"}); err == nil {
		c.Close()
		t.Fatal("aceptó un modo desconocido")
	}
}
