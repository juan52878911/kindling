package main

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/imagen"
)

func TestParseAudit(t *testing.T) {
	out := "U /usr/local/bin/postgres\nU /usr/lib/libssl.so.3\nF 100 /usr/local/bin/postgres\nF 5 /etc/with space/x\nbasura\nF x /bad\nF 7 relative\n"
	files, used := parseAudit([]byte(out))
	if len(files) != 2 || files[1].Path != "/etc/with space/x" || files[1].Size != 5 {
		t.Fatalf("files %+v", files)
	}
	if !used["/usr/local/bin/postgres"] || !used["/usr/lib/libssl.so.3"] || len(used) != 2 {
		t.Fatalf("used %v", used)
	}
}

func TestInformeAuditoria(t *testing.T) {
	const MiB = 1 << 20
	files := []auditFile{
		{"/usr/local/bin/postgres", 10 * MiB},
		{"/usr/lib/libLLVM.so.21.1", 100 * MiB},
		{"/usr/local/lib/postgresql/bitcode/a.bc", 20 * MiB},
		{"/usr/local/lib/postgresql/plpgsql.so", 1 * MiB},
		{"/usr/share/zoneinfo/Zulu", 1 * MiB},
	}
	used := map[string]bool{"/usr/local/bin/postgres": true}
	rules, err := imagen.CargarReglasSlim()
	if err != nil {
		t.Fatal(err)
	}
	got := informeAuditoria("pg", files, used, &rules)
	for _, want := range []string{
		"audit of pg: 5 files",
		"never seen mapped or open: 4 files",
		"100M  /usr/lib/libLLVM.so.21.1",
		"/usr/lib/libLLVM.so.21.1",
		"already removes",
		"candidates, not proof",
		"Nothing was removed or saved",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// LLVM (100) y bitcode (20) los cubre -slim; plpgsql y zoneinfo, no.
	if !strings.Contains(got, "removes 120M of the unused") {
		t.Errorf("slim coverage wrong:\n%s", got)
	}
	// Sin reglas no hay línea de cobertura.
	if strings.Contains(informeAuditoria("pg", files, used, nil), "already removes") {
		t.Error("coverage line without rules")
	}
}

func TestDirDe(t *testing.T) {
	if got := dirDe("/usr/local/lib/postgresql/bitcode/a.bc", 3); got != "/usr/local/lib" {
		t.Error(got)
	}
	if got := dirDe("/etc/passwd", 3); got != "/etc" {
		t.Error(got)
	}
}
