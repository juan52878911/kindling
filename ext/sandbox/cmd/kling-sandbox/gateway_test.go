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
	"net/http"
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

	// Los campos 5 y 6 son los máximos de mem_mib y vcpus por sandbox.
	t.Setenv("KLING_SANDBOX_ADMINS", "")
	t.Setenv("KLING_SANDBOX_TENANTS", "ana:tok1:10:5:2048:2,bob:tok2::::8")
	ts, err = leerTenants()
	if err != nil {
		t.Fatal(err)
	}
	if ts[0].MaxMemMiB != 2048 || ts[0].MaxVCPUs != 2 || ts[1].MaxMemMiB != 0 || ts[1].MaxVCPUs != 8 {
		t.Fatalf("limits: %+v", ts)
	}
	// El 7.º es la suma de mem_mib viva del tenant.
	t.Setenv("KLING_SANDBOX_TENANTS", "ana:tok1:10:5:2048:2:8192")
	if ts, err = leerTenants(); err != nil || ts[0].MaxMemTotalMiB != 8192 {
		t.Fatalf("total limit: %+v %v", ts, err)
	}
	// Del 8.º al 10.º: procesos de fondo, sesiones y lecturas con follow.
	t.Setenv("KLING_SANDBOX_TENANTS", "ana:tok1:10:5:2048:2:8192:7:3:9")
	if ts, err = leerTenants(); err != nil || ts[0].MaxProcesos != 7 || ts[0].MaxSesiones != 3 || ts[0].MaxSeguir != 9 {
		t.Fatalf("process limits: %+v %v", ts, err)
	}
	t.Setenv("KLING_SANDBOX_TENANTS", "ana:tok1:::::::x")
	if _, err := leerTenants(); err == nil {
		t.Fatal("expected an error for a bad max_processes")
	}
	t.Setenv("KLING_SANDBOX_TENANTS", "ana:tok1:10:5:mucho")
	if _, err := leerTenants(); err == nil {
		t.Fatal("expected an error for a bad max_mem_mib")
	}

	// Un admin que no es ningún tenant es una errata, no un permiso que se ignora.
	t.Setenv("KLING_SANDBOX_ADMINS", "opss")
	if _, err := leerTenants(); err == nil {
		t.Fatal("expected an error for an unknown admin")
	}
}

// El servidor del gateway acota las cabeceras (plazo y tamaño) y lo que vive
// una conexión keep-alive callada.
func TestServidorHTTPConPlazosYTopes(t *testing.T) {
	s := servidorHTTP(http.NotFoundHandler())
	if s.ReadHeaderTimeout <= 0 || s.IdleTimeout <= 0 || s.MaxHeaderBytes <= 0 || s.MaxHeaderBytes > 1<<20 {
		t.Fatalf("server without limits: header %s, idle %s, max header %d", s.ReadHeaderTimeout, s.IdleTimeout, s.MaxHeaderBytes)
	}
	if s.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout %s would cut shells and long execs", s.WriteTimeout)
	}
}
