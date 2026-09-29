package templates

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestListIncluyeLasBasicas(t *testing.T) {
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, i := range list {
		got[i.Name] = i.Summary
	}
	for _, n := range []string{"empty", "crm-demo"} {
		if got[n] == "" {
			t.Errorf("template %s missing or without summary: %v", n, got)
		}
	}
}

func TestMaterialize(t *testing.T) {
	list, _ := List()
	for _, info := range list {
		d, err := Materialize(info.Name)
		if err != nil {
			t.Fatalf("%s: %v", info.Name, err)
		}
		st, err := os.Stat(d.Root)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s: root %v %v", info.Name, st, err)
		}
		filepath.Walk(d.Root, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm()&0o077 != 0 {
				t.Errorf("%s is accessible to others: %v", p, fi.Mode())
			}
			return nil
		})
		if ms, _ := filepath.Glob(filepath.Join(d.Migrations, "*.sql")); len(ms) == 0 {
			t.Errorf("%s: no migrations", info.Name)
		}
		if b, err := os.ReadFile(d.Seed); err != nil || len(b) == 0 {
			t.Errorf("%s: seed %v", info.Name, err)
		}
		if _, err := os.Stat(filepath.Join(d.Root, "README.md")); err == nil {
			t.Errorf("%s: the README is not input for the build", info.Name)
		}
		d.Cleanup()
		if _, err := os.Stat(d.Root); err == nil {
			t.Errorf("%s: cleanup left the directory", info.Name)
		}
	}
	for _, bad := range []string{"", "nope", "../empty", "empty/", "/etc", "Empty", "empty/migrations"} {
		if _, err := Materialize(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

// El seed de crm-demo tiene que ser determinista y sin datos reales: nada de
// random(), now() ni fechas relativas al momento de construirlo.
func TestCRMDemoDeterministaYSintetico(t *testing.T) {
	d, err := Materialize("crm-demo")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Cleanup()
	b, _ := os.ReadFile(d.Seed)
	seed := string(b)
	for _, bad := range []string{"random()", "now()", "current_date", "current_timestamp", "clock_timestamp", "gen_random_uuid", "COPY ", "\\copy"} {
		if strings.Contains(strings.ToLower(seed), strings.ToLower(bad)) {
			t.Errorf("the seed uses %s", bad)
		}
	}
	if !strings.Contains(seed, "generate_series") {
		t.Error("the seed does not use generate_series")
	}
	// Todos los correos son del dominio reservado, todos los teléfonos de la
	// gama ficticia.
	for _, m := range regexp.MustCompile(`@([a-z0-9.]+)'`).FindAllStringSubmatch(seed, -1) {
		if m[1] != "example.com" {
			t.Errorf("e-mail domain %s", m[1])
		}
	}
	if !strings.Contains(seed, "+1-555-01") || regexp.MustCompile(`'\+\d[\d-]*\d`).FindAllString(strings.ReplaceAll(seed, "+1-555-01", ""), -1) != nil {
		t.Error("phone numbers outside the fictional 555-01xx range")
	}
	// Las tablas que promete el README.
	mig, _ := os.ReadFile(filepath.Join(d.Migrations, "001_schema.sql"))
	for _, tbl := range []string{"customers", "contacts", "deals", "activities", "products", "invoices"} {
		if !strings.Contains(string(mig), "CREATE TABLE "+tbl+" ") {
			t.Errorf("missing table %s", tbl)
		}
	}
	for _, st := range []string{"lead", "qualified", "proposal", "negotiation", "won", "lost"} {
		if !strings.Contains(string(mig), "'"+st+"'") {
			t.Errorf("missing deal stage %s", st)
		}
	}
}
