package doctor

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPlanTLS(t *testing.T) {
	mk := func(host, q string) *pgURL {
		p, err := parseURL("postgres://app@" + host + "/appdb" + q)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	remoto := func(q string) *pgURL { return mk("db.example", q) }
	local := func(q string) *pgURL { return mk("127.0.0.1", q) }
	// disable: solo loopback, salvo -insecure.
	if _, _, err := planTLS(remoto("?sslmode=disable"), Target{}); err == nil {
		t.Fatal("disable contra un remoto sin -insecure")
	}
	if m, _, err := planTLS(remoto("?sslmode=disable"), Target{Insecure: true}); err != nil || m != "" {
		t.Fatalf("disable -insecure: %q %v", m, err)
	}
	if m, _, err := planTLS(local("?sslmode=disable"), Target{}); err != nil || m != "" {
		t.Fatalf("disable en loopback: %q %v", m, err)
	}
	if _, _, err := planTLS(local("?sslmode=disable"), Target{TLSServerName: "x"}); err == nil {
		t.Fatal("disable con -tls-server-name")
	}
	// require no autentica: en remoto exige -insecure.
	if _, _, err := planTLS(remoto("?sslmode=require"), Target{}); err == nil {
		t.Fatal("require contra un remoto sin -insecure")
	}
	// Por defecto, verify-full.
	if m, _, err := planTLS(remoto(""), Target{}); err != nil || m != "verify-full" {
		t.Fatalf("defecto: %q %v", m, err)
	}
	if m, _, err := planTLS(remoto("?sslmode=verify-ca"), Target{}); err != nil || m != "verify-ca" {
		t.Fatalf("verify-ca: %q %v", m, err)
	}
	// Un fichero de CA que no es PEM o no existe falla.
	malo := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(malo, []byte("no soy un certificado"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := planTLS(remoto(""), Target{CAFile: malo}); err == nil {
		t.Fatal("aceptó un CA file sin certificados")
	}
	if _, _, err := planTLS(remoto(""), Target{CAFile: malo + ".no"}); err == nil {
		t.Fatal("aceptó un CA file inexistente")
	}
}

// servidorTLS es un Postgres falso con TLS, sin autenticación, que contesta
// error a toda consulta (el doctor lo convierte en hallazgos, no en fallo).
func servidorTLS(t *testing.T) (addr, caPEM string) {
	t.Helper()
	ca, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &ca.PublicKey, ca)
	caCert, _ := x509.ParseCertificate(caDER)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "db.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"db.test"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, caCert, &k.PublicKey, ca)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("sin red loopback:", err)
	}
	t.Cleanup(func() { ln.Close() })
	cfg := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: k}}, MinVersion: tls.VersionTLS12}
	msg := func(tp byte, b []byte) []byte {
		o := binary.BigEndian.AppendUint32([]byte{tp}, uint32(4+len(b)))
		return append(o, b...)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var h [8]byte
				if _, err := io.ReadFull(c, h[:]); err != nil || binary.BigEndian.Uint32(h[4:]) != 80877103 {
					return // sin SSLRequest (sslmode=disable): se cuelga la conexión
				}
				c.Write([]byte("S"))
				tc := tls.Server(c, cfg)
				if tc.Handshake() != nil {
					return
				}
				r := bufio.NewReader(tc)
				var l [4]byte
				io.ReadFull(r, l[:])
				io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint32(l[:]))-4)
				tc.Write(msg('R', binary.BigEndian.AppendUint32(nil, 0)))
				tc.Write(msg('Z', []byte("I")))
				for {
					var mh [5]byte
					if _, err := io.ReadFull(r, mh[:]); err != nil {
						return
					}
					io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint32(mh[1:]))-4)
					tc.Write(msg('E', []byte("SERROR\x00C42501\x00Mno\x00\x00")))
					tc.Write(msg('Z', []byte("I")))
				}
			}()
		}
	}()
	return ln.Addr().String(), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

func TestRunURLTLS(t *testing.T) {
	addr, caPEM := servidorTLS(t)
	_, port, _ := net.SplitHostPort(addr)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte(caPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := "postgres://app@127.0.0.1:" + port + "/appdb"

	// Sin la CA del servidor, verify-full (el defecto) falla.
	if err := runURL(t.Context(), Target{URL: raw}, &report{}); err == nil {
		t.Fatal("verify-full aceptó un certificado de una CA desconocida")
	}
	// Con la CA pero sin el nombre del certificado (127.0.0.1 no está en él).
	if err := runURL(t.Context(), Target{URL: raw, CAFile: ca}, &report{}); err == nil {
		t.Fatal("verify-full aceptó un nombre que no es el del certificado")
	}
	// CA + nombre: bien, y sin hallazgo de "sin TLS".
	r := &report{}
	if err := runURL(t.Context(), Target{URL: raw, CAFile: ca, TLSServerName: "db.test"}, r); err != nil {
		t.Fatal(err)
	}
	for _, f := range r.findings {
		if f.Rule == "DB041" {
			t.Fatalf("hallazgo DB041 con TLS verificado: %+v", f)
		}
	}
	// sslrootcert en la URL hace lo mismo que -ca-file.
	if err := runURL(t.Context(), Target{URL: raw + "?sslrootcert=" + ca, TLSServerName: "db.test"}, &report{}); err != nil {
		t.Fatal(err)
	}
	// verify-ca no mira el nombre.
	if err := runURL(t.Context(), Target{URL: raw + "?sslmode=verify-ca", CAFile: ca}, &report{}); err != nil {
		t.Fatal(err)
	}
	// Contra un servidor que solo habla TLS, disable no entra.
	if err := runURL(t.Context(), Target{URL: raw + "?sslmode=disable"}, &report{}); err == nil {
		t.Fatal("disable contra un servidor TLS funcionó")
	}
}
