package main

import (
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
	if got := pgDestino(s); got != "db.example.com:6432" {
		t.Errorf("pgDestino: %q", got)
	}

	for nombre, args := range map[string][]string{
		"postgres sin rol":   {"-type", "postgres", "-domain", "db.example.com", "-env", "P"},
		"postgres con allow": {"-type", "postgres", "-domain", "db.example.com", "-env", "P", "-user", "a", "-allow-request", "GET /"},
		"http con rol":       {"-domain", "api.example.com", "-env", "K", "-user", "a"},
		"http con CA":        {"-domain", "api.example.com", "-env", "K", "-ca-file", ca},
		"tipo raro":          {"-type", "mysql", "-domain", "db.example.com", "-env", "P"},
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
