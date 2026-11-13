package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Las banderas de una credencial: postgres lleva rol, puerto, base y CA; lo
// que no pega con el tipo se rechaza antes de leer la clave, que llega por -f.
func TestCredentialFlagsSpec(t *testing.T) {
	dir := t.TempDir()
	clave := filepath.Join(dir, "clave")
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(clave, []byte("pw-real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parse := func(args ...string) (*credFlags, error) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		cf := credentialFlags(fs)
		return cf, fs.Parse(append([]string{"-f", clave}, args...))
	}

	cf, _ := parse("-type", "postgres", "-domain", "db.example.com", "-env", "PGPASSWORD", "-user", "app",
		"-database", "appdb", "-port", "6432", "-ca-file", ca)
	s, err := cf.spec()
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != "postgres" || s.User != "app" || s.Database != "appdb" || s.Port != 6432 ||
		!strings.HasPrefix(s.CAPEM, "-----BEGIN") || s.Secret != "pw-real" {
		t.Fatalf("spec %+v", s)
	}
	if got := pgConexion(s); !strings.Contains(got, "host=db.example.com user=app dbname=appdb password=$PGPASSWORD") {
		t.Errorf("pgConexion: %q", got)
	}
	if got := pgDestino(s); got != "db.example.com:6432 over verified TLS" {
		t.Errorf("pgDestino: %q", got)
	}

	for nombre, args := range map[string][]string{
		"postgres sin rol":   {"-type", "postgres", "-domain", "db.example.com", "-env", "P"},
		"postgres con allow": {"-type", "postgres", "-domain", "db.example.com", "-env", "P", "-user", "a", "-allow-request", "GET /"},
		"http con rol":       {"-domain", "api.example.com", "-env", "K", "-user", "a"},
		"http con CA":        {"-domain", "api.example.com", "-env", "K", "-ca-file", ca},
		"postgres sin base":  {"-type", "postgres", "-domain", "db.example.com", "-env", "P", "-user", "a"},
		"base y any":         {"-type", "postgres", "-domain", "db.example.com", "-env", "P", "-user", "a", "-database", "d", "-any-database"},
		"http con any":       {"-domain", "api.example.com", "-env", "K", "-any-database"},
		"tipo raro":          {"-type", "mysql", "-domain", "db.example.com", "-env", "P"},
		"http con upstream":  {"-domain", "api.example.com", "-env", "K", "-upstream", "127.0.0.1:5432"},
		"http con nombre":    {"-domain", "api.example.com", "-env", "K", "-tls-server-name", "x.example.com"},
		"disable sin upstream": {"-type", "postgres", "-domain", "db.example.com", "-env", "P", "-user", "a",
			"-upstream-tls", "disable"},
		"tls raro": {"-type", "postgres", "-domain", "db.example.com", "-env", "P", "-user", "a",
			"-upstream", "127.0.0.1:5432", "-upstream-tls", "require"},
	} {
		cf, err := parse(args...)
		if err != nil {
			t.Fatalf("%s: %v", nombre, err)
		}
		if _, err := cf.spec(); err == nil {
			t.Errorf("%s: aceptada", nombre)
		}
	}
}

// -upstream, -upstream-tls y -tls-server-name: pasan a la credencial y, con
// disable hacia algo que no es el loopback, avisan por stderr de que las
// consultas viajan sin cifrar.
func TestCredentialFlagsUpstream(t *testing.T) {
	clave := filepath.Join(t.TempDir(), "clave")
	if err := os.WriteFile(clave, []byte("pw-real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var avisos bytes.Buffer
	antes := credAvisos
	credAvisos = &avisos
	t.Cleanup(func() { credAvisos = antes })
	spec := func(args ...string) string {
		t.Helper()
		avisos.Reset()
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		cf := credentialFlags(fs)
		base := []string{"-f", clave, "-type", "postgres", "-domain", "db.example.com", "-env", "PGPASSWORD", "-user", "app", "-database", "appdb"}
		if err := fs.Parse(append(base, args...)); err != nil {
			t.Fatal(err)
		}
		s, err := cf.spec()
		if err != nil {
			t.Fatal(err)
		}
		return s.Upstream + "|" + s.UpstreamTLS + "|" + s.TLSServerName + "|" + pgDestino(s)
	}

	if got := spec("-upstream", "127.0.0.1:5432", "-upstream-tls", "disable"); !strings.HasPrefix(got, "127.0.0.1:5432|disable||") ||
		!strings.Contains(got, "without TLS") {
		t.Errorf("loopback + disable: %q", got)
	}
	if avisos.Len() != 0 {
		t.Errorf("aviso con el loopback: %q", avisos.String())
	}
	spec("-upstream", "10.0.0.5:5432", "-upstream-tls", "disable")
	if !strings.Contains(avisos.String(), "traffic to 10.0.0.5:5432, including query data, is unencrypted") {
		t.Errorf("sin aviso para la LAN: %q", avisos.String())
	}
	if got := spec("-upstream", "db.lan:5432", "-tls-server-name", "pg.internal.example.com"); got !=
		"db.lan:5432||pg.internal.example.com|db.example.com (upstream db.lan:5432) over TLS verified as pg.internal.example.com" {
		t.Errorf("upstream + nombre TLS: %q", got)
	}
	if avisos.Len() != 0 {
		t.Errorf("aviso con TLS: %q", avisos.String())
	}
}

// -any-database: pasa a la credencial y avisa por stderr; sin él ni -database,
// el CLI lo rechaza.
func TestCredentialFlagsAnyDatabase(t *testing.T) {
	clave := filepath.Join(t.TempDir(), "clave")
	if err := os.WriteFile(clave, []byte("pw-real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var avisos bytes.Buffer
	antes := credAvisos
	credAvisos = &avisos
	t.Cleanup(func() { credAvisos = antes })
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cf := credentialFlags(fs)
	if err := fs.Parse([]string{"-f", clave, "-type", "postgres", "-domain", "db.example.com", "-env", "P", "-user", "app", "-any-database"}); err != nil {
		t.Fatal(err)
	}
	s, err := cf.spec()
	if err != nil || !s.AnyDatabase || s.Database != "" {
		t.Fatalf("spec %+v, %v", s, err)
	}
	if !strings.Contains(avisos.String(), "warning: -any-database") || !strings.Contains(avisos.String(), "CONNECT") {
		t.Errorf("sin aviso: %q", avisos.String())
	}
}

// -type mysql: mismas banderas que postgres; avisa de que la base no es una
// frontera, de que sin TLS fuera del loopback el servidor no se autentica, y
// describe la conexión con los términos de MySQL.
func TestCredentialFlagsMySQL(t *testing.T) {
	clave := filepath.Join(t.TempDir(), "clave")
	if err := os.WriteFile(clave, []byte("pw-real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var avisos bytes.Buffer
	antes := credAvisos
	credAvisos = &avisos
	t.Cleanup(func() { credAvisos = antes })
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cf := credentialFlags(fs)
	if err := fs.Parse([]string{"-f", clave, "-type", "mysql", "-domain", "db.example.com", "-env", "MYSQL_PWD",
		"-user", "app", "-database", "appdb", "-upstream", "10.0.0.5:3306", "-upstream-tls", "disable"}); err != nil {
		t.Fatal(err)
	}
	s, err := cf.spec()
	if err != nil || s.Type != "mysql" || s.User != "app" || s.Database != "appdb" || s.UpstreamTLS != "disable" || s.Secret != "pw-real" {
		t.Fatalf("spec %+v, %v", s, err)
	}
	if !strings.Contains(avisos.String(), "USE any database") || !strings.Contains(avisos.String(), "does not prove the server knows the password") {
		t.Errorf("avisos: %q", avisos.String())
	}
	if got := pgConexion(s); !strings.Contains(got, "database=appdb") || !strings.Contains(got, "ssl-mode") {
		t.Errorf("conexión: %q", got)
	}
	s.Upstream, s.UpstreamTLS = "", ""
	if got := pgDestino(s); !strings.Contains(got, "db.example.com:3306") {
		t.Errorf("destino: %q", got)
	}

	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	cf = credentialFlags(fs)
	_ = fs.Parse([]string{"-f", clave, "-type", "mysql", "-domain", "db.example.com", "-env", "MYSQL_PWD", "-user", "app"})
	if _, err := cf.spec(); err == nil || !strings.Contains(err.Error(), "-type mysql needs -database") {
		t.Errorf("sin -database: %v", err)
	}
}

// -type sigv4: el access key id y la región van en banderas, la secret key en
// -f; lo que es de otro tipo (o lo que la re-firma no usa) se rechaza.
func TestCredentialFlagsSigV4(t *testing.T) {
	dir := t.TempDir()
	clave := filepath.Join(dir, "sk")
	if err := os.WriteFile(clave, []byte("secreto-real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parse := func(args ...string) (*credFlags, error) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		cf := credentialFlags(fs)
		return cf, fs.Parse(append([]string{"-f", clave}, args...))
	}
	cf, _ := parse("-type", "sigv4", "-domain", "s3.eu-west-1.amazonaws.com", "-env", "AWS_ACCESS_KEY_ID",
		"-access-key-id", "AKIAEXAMPLE1234", "-region", "eu-west-1", "-allow-request", "GET /bucket/**")
	s, err := cf.spec()
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != "sigv4" || s.AccessKeyID != "AKIAEXAMPLE1234" || s.Region != "eu-west-1" || s.Service != "" ||
		s.Secret != "secreto-real" || len(s.Allow) != 1 {
		t.Fatalf("spec %+v", s)
	}
	if got := svConexion(s); !strings.Contains(got, "http://s3.eu-west-1.amazonaws.com") ||
		!strings.Contains(got, "path-style") || !strings.Contains(got, "$AWS_ACCESS_KEY_ID") ||
		strings.Contains(got, "AKIAEXAMPLE1234") {
		t.Errorf("svConexion: %q", got)
	}

	base := []string{"-type", "sigv4", "-domain", "s3.example.com", "-env", "AWS_ACCESS_KEY_ID"}
	for nombre, args := range map[string][]string{
		"sin access key":  append(append([]string(nil), base...), "-region", "auto"),
		"sin region":      append(append([]string(nil), base...), "-access-key-id", "AKIAEXAMPLE1234"),
		"con rol":         append(append([]string(nil), base...), "-access-key-id", "AKIAEXAMPLE1234", "-region", "auto", "-user", "a"),
		"con cabecera":    append(append([]string(nil), base...), "-access-key-id", "AKIAEXAMPLE1234", "-region", "auto", "-header", "X-A"),
		"con body":        append(append([]string(nil), base...), "-access-key-id", "AKIAEXAMPLE1234", "-region", "auto", "-body"),
		"http con region": {"-domain", "api.example.com", "-env", "K", "-region", "auto"},
		"pg con access":   {"-type", "postgres", "-domain", "db.example.com", "-env", "P", "-user", "a", "-database", "d", "-access-key-id", "AKIAEXAMPLE1234"},
	} {
		cf, err := parse(args...)
		if err != nil {
			t.Fatalf("%s: parse: %v", nombre, err)
		}
		if _, err := cf.spec(); err == nil {
			t.Errorf("%s: spec aceptado", nombre)
		}
	}
}
