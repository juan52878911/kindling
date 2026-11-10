package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Los tokens van en cada petición: en claro, solo por loopback. Fuera de
// loopback hace falta TLS o pedirlo explícitamente con -insecure-listen.
func TestEscucharExigeTLSFueraDeLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0"} {
		ln, err := escuchar(addr, "", "", false)
		if err == nil {
			ln.Close()
			t.Errorf("%s without a certificate: accepted", addr)
		} else if !strings.Contains(err.Error(), "-insecure-listen") {
			t.Errorf("%s: error does not say how to opt in: %v", addr, err)
		}
	}
	for _, c := range []struct {
		addr     string
		insecure bool
	}{{"127.0.0.1:0", false}, {"localhost:0", false}, {"0.0.0.0:0", true}} {
		ln, err := escuchar(c.addr, "", "", c.insecure)
		if err != nil {
			t.Errorf("%s (insecure=%v): %v", c.addr, c.insecure, err)
			continue
		}
		ln.Close()
	}
	// Solo uno de los dos ficheros es una errata.
	if ln, err := escuchar("127.0.0.1:0", "cert.pem", "", false); err == nil {
		ln.Close()
		t.Error("-tls-cert without -tls-key: accepted")
	}
}

func TestEscucharConTLS(t *testing.T) {
	cert, key := certPrueba(t)
	ln, err := escuchar("0.0.0.0:0", cert, key, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.Write([]byte("hola"))
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", net.JoinHostPort("127.0.0.1", port),
		&tls.Config{InsecureSkipVerify: true}) // el certificado es de prueba
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	defer c.Close()
	b := make([]byte, 4)
	if _, err := c.Read(b); err != nil || string(b) != "hola" {
		t.Fatalf("read %q %v", b, err)
	}
}

// certPrueba escribe un par autofirmado para 127.0.0.1 y devuelve sus rutas.
func certPrueba(t *testing.T) (string, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gateway"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cp, kp
}

func TestLeerTenantsConAdmins(t *testing.T) {
	t.Setenv("KLING_SANDBOX_TOKEN", "")
	t.Setenv("KLING_SANDBOX_TENANTS", "ana:tok1:10,ops:tok2")
	t.Setenv("KLING_SANDBOX_ADMINS", " ops ")
	ts, err := leerTenants()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].Admin || !ts[1].Admin {
		t.Fatalf("tenants: %+v", ts)
	}

	// Un admin que no es ningún tenant es una errata, no un permiso que se ignora.
	t.Setenv("KLING_SANDBOX_ADMINS", "opss")
	if _, err := leerTenants(); err == nil {
		t.Fatal("expected an error for an unknown admin")
	}
}
