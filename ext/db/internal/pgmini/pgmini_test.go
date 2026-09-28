package pgmini

import (
	"bufio"
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"strings"
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
	defer c.Close()
	r := bufio.NewReader(c)
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return
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
		c.Write(auth(10, "SCRAM-SHA-256\x00\x00"))
		p := leerP()
		i := strings.Index(string(p), "\x00")
		cf := string(p[i+1+4:]) // tras el mecanismo y la longitud
		bare := strings.TrimPrefix(cf, "n,,")
		cn := strings.TrimPrefix(strings.Split(bare, ",")[1], "r=")
		nonce := cn + "SERVERPART"
		sal := []byte("salsalsal")
		sf := "r=" + nonce + ",s=" + base64.StdEncoding.EncodeToString(sal) + ",i=4096"
		c.Write(auth(11, sf))
		fin := string(leerP())
		sinPrueba := "c=biws,r=" + nonce
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
