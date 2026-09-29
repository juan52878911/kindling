package main

// Los scripts que editan pg_hba.conf, ejecutados de verdad con /bin/sh contra
// un fichero temporal. Un `su` falso en el PATH hace de Postgres: contesta a
// SHOW hba_file con la ruta del fichero y apunta los pg_reload_conf.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runHBA ejecuta script con sh -s. hba es el contenido inicial de pg_hba.conf;
// devuelve el final y cuántas recargas hubo.
func runHBA(t *testing.T, script, hba string) (string, int, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "pg_hba.conf")
	if err := os.WriteFile(f, []byte(hba), 0o600); err != nil {
		t.Fatal(err)
	}
	reloads := filepath.Join(dir, "reloads")
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// su -s /bin/sh postgres -c "<orden>": la orden es $5.
	su := "#!/bin/sh\ncase \"$5\" in\n" +
		"  *'SHOW hba_file'*) echo '" + f + "' ;;\n" +
		"  *pg_reload_conf*) echo x >> '" + reloads + "'; echo t ;;\n" +
		"  *) echo \"unexpected: $5\" >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "su"), []byte(su), 0o755); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("/bin/sh", "-s")
	c.Stdin = strings.NewReader(script)
	c.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Logf("sh: %s", out)
	}
	b, rerr := os.ReadFile(f)
	if rerr != nil {
		t.Fatal(rerr)
	}
	n := 0
	if r, err := os.ReadFile(reloads); err == nil {
		n = strings.Count(string(r), "x")
	}
	return string(b), n, err
}

const hbaBase = "local all postgres peer\nhost  all app 0.0.0.0/0 scram-sha-256\n"

func TestHBAScriptSoloLaBaseDeLaCopia(t *testing.T) {
	got, _, err := runHBA(t, hbaScript("appdb", "agent", true), hbaBase)
	if err != nil {
		t.Fatal(err)
	}
	want := hbaBase + `host "appdb" "agent" 0.0.0.0/0 scram-sha-256 # kling-db` + "\n"
	if got != want {
		t.Fatalf("pg_hba.conf:\n%s\nwant:\n%s", got, want)
	}
	// Idempotente: añadir otra vez no duplica.
	if got2, _, err := runHBA(t, hbaScript("appdb", "agent", true), got); err != nil || got2 != want {
		t.Fatalf("second add: %v\n%s", err, got2)
	}
	// Quitar se lleva la línea nueva y la de versiones anteriores (host all).
	old := got + `host all "agent" 0.0.0.0/0 scram-sha-256 # kling-db` + "\n"
	got, _, err = runHBA(t, hbaScript("appdb", "agent", false), old)
	if err != nil || got != hbaBase {
		t.Fatalf("remove: %v\n%s", err, got)
	}
	if strings.Contains(hbaScript("appdb", "agent", true), "/var/lib/postgresql") {
		t.Fatal("hardcoded pg_hba.conf path")
	}
}

func TestHBAScriptRutaRara(t *testing.T) {
	// Una ruta con caracteres fuera de la lista no se toca.
	s := strings.Replace(hbaScript("appdb", "agent", true), `F=$(q 'SHOW hba_file')`, `F='/tmp/a b;c'`, 1)
	if _, _, err := runHBA(t, s, hbaBase); err == nil {
		t.Fatal("accepted an odd pg_hba path")
	}
}

func TestPurgeHBAScript(t *testing.T) {
	in := hbaBase +
		`host "appdb" "agent" 0.0.0.0/0 scram-sha-256 # kling-db` + "\n" +
		`host all "viejo" 0.0.0.0/0 scram-sha-256 # kling-db` + "\n" +
		"local all kling_db_ro peer map=kling_db_ask\n"
	got, reloads, err := runHBA(t, purgeHBAScript, in)
	if err != nil {
		t.Fatal(err)
	}
	if want := hbaBase + "local all kling_db_ro peer map=kling_db_ask\n"; got != want {
		t.Fatalf("pg_hba.conf:\n%s\nwant:\n%s", got, want)
	}
	if reloads != 1 {
		t.Fatalf("%d reloads, want 1", reloads)
	}
	// Sin nada que quitar: no toca el fichero ni recarga.
	got, reloads, err = runHBA(t, purgeHBAScript, hbaBase)
	if err != nil || got != hbaBase || reloads != 0 {
		t.Fatalf("clean file: err=%v reloads=%d\n%s", err, reloads, got)
	}
}
