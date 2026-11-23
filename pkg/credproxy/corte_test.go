package credproxy

import (
	"encoding/pem"
	"errors"
	"io"
	"testing"
	"time"
)

// Una sesión de base de datos ya autenticada sigue usando la clave real
// mientras viva. Por eso rotar o quitar la credencial tiene que cortarla
// también cuando el destino es un dominio (no solo hacia otra máquina), y en
// MySQL igual que en Postgres. Repetir el mismo juego no corta nada.
func TestPGDominioRotarOQuitarCorta(t *testing.T) {
	srv := nuevoServidorPG(t, "scram")
	e := proxyPG(t, srv, nil)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.cert.Certificate[0]}))
	base := Credential{Env: "PGPASSWORD", Domain: pgDominio, Placeholder: pgMarca, Secret: pgClave,
		Kind: KindPostgres, User: pgUser, Database: pgDB, CAPEM: ca}

	k := entrar(t, e.addr)
	if _, err := e.p.SetCredentials([]Credential{base}); err != nil {
		t.Fatal(err)
	}
	if cortada(k) {
		t.Fatal("repetir la misma credencial no debe cortar la sesión")
	}
	rotada := base
	rotada.Secret = pgClave + "-nueva"
	if _, err := e.p.SetCredentials([]Credential{rotada}); err != nil {
		t.Fatal(err)
	}
	if !cortada(k) {
		t.Fatal("rotar la clave debe cortar la sesión que entró con la anterior")
	}

	if _, err := e.p.SetCredentials([]Credential{base}); err != nil {
		t.Fatal(err)
	}
	k = entrar(t, e.addr)
	if _, err := e.p.SetCredentials(nil); err != nil {
		t.Fatal(err)
	}
	if !cortada(k) {
		t.Fatal("quitar la credencial debe cortar la sesión")
	}
}

// Invalidar("") corta lo que va a otras máquinas, no las sesiones hacia un
// dominio: el agente que cambió de dueño no tiene que ver con ellas.
func TestPGDominioNoLoCortaInvalidarAgente(t *testing.T) {
	e := proxyPG(t, nuevoServidorPG(t, "scram"), nil)
	k := entrar(t, e.addr)
	if n := e.p.Invalidar(""); n != 0 {
		t.Fatalf("Invalidar(\"\") cortó %d sesiones hacia un dominio", n)
	}
	if cortada(k) {
		t.Fatal("la sesión hacia el dominio se cortó")
	}
}

// cortadaMy dice si la conexión MySQL del invitado se cerró en poco tiempo.
func cortadaMy(k *clienteMy) bool {
	_ = k.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var b [1]byte
	_, err := k.c.Read(b[:])
	return errors.Is(err, io.EOF) || (err != nil && !isTimeout(err))
}

func TestMyRotarOQuitarCorta(t *testing.T) {
	srv := nuevoServidorMy(t, "native")
	e := proxyMy(t, srv, nil)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.cert.Certificate[0]}))
	base := Credential{Env: "MYSQL_PWD", Domain: myDominio, Placeholder: myMarca, Secret: myClave,
		Kind: KindMySQL, User: myUser, Database: myDB, CAPEM: ca}
	entrarMy := func() *clienteMy {
		k := conectarMy(t, e.addr)
		if p := k.login(pluginNative, myMarca); p[0] != 0 {
			t.Fatalf("esperaba OK, llegó %q", p)
		}
		return k
	}

	k := entrarMy()
	if _, err := e.p.SetCredentials([]Credential{base}); err != nil {
		t.Fatal(err)
	}
	if cortadaMy(k) {
		t.Fatal("repetir la misma credencial no debe cortar la sesión")
	}
	rotada := base
	rotada.Secret = myClave + "-nueva"
	if _, err := e.p.SetCredentials([]Credential{rotada}); err != nil {
		t.Fatal(err)
	}
	if !cortadaMy(k) {
		t.Fatal("rotar la clave debe cortar la sesión que entró con la anterior")
	}

	if _, err := e.p.SetCredentials([]Credential{base}); err != nil {
		t.Fatal(err)
	}
	k = entrarMy()
	if _, err := e.p.SetCredentials(nil); err != nil {
		t.Fatal(err)
	}
	if !cortadaMy(k) {
		t.Fatal("quitar la credencial debe cortar la sesión")
	}
}
