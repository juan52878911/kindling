package scripts

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// jobsDeWorkflow parte un workflow en sus jobs (las claves con dos espacios
// bajo jobs:), sin un parser de YAML: el núcleo no admite dependencias.
func jobsDeWorkflow(t *testing.T, ruta string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatal(err)
	}
	_, jobs, ok := strings.Cut(string(b), "\njobs:\n")
	if !ok {
		t.Fatalf("%s sin jobs:", ruta)
	}
	cabecera := regexp.MustCompile(`(?m)^  ([A-Za-z0-9_-]+):\s*$`)
	idx := cabecera.FindAllStringSubmatchIndex(jobs, -1)
	out := map[string]string{}
	for i, m := range idx {
		fin := len(jobs)
		if i+1 < len(idx) {
			fin = idx[i+1][0]
		}
		out[jobs[m[2]:m[3]]] = jobs[m[0]:fin]
	}
	return out
}

// El job release solo baja los artefactos de los jobs que producen assets
// (con pattern): sin él, con merge-multiple, cualquier otro artefacto del
// run con el nombre de un asset lo pisaría antes de calcular SHA256SUMS.
// Y el job del tap comprueba la firma del SHA256SUMS antes de fiarse de él.
func TestReleaseWorkflowArtefactosYFirmaDelTap(t *testing.T) {
	jobs := jobsDeWorkflow(t, "../.github/workflows/release.yml")
	rel, ok := jobs["release"]
	if !ok {
		t.Fatal("release.yml sin job release")
	}
	pasos := strings.Split(rel, "\n      - ")
	descargas := 0
	for _, p := range pasos {
		if !strings.Contains(p, "actions/download-artifact@") {
			continue
		}
		descargas++
		if !regexp.MustCompile(`(?m)^\s+pattern:\s*\S`).MatchString(p) {
			t.Errorf("job release: download-artifact sin pattern:\n%s", p)
		}
	}
	if descargas == 0 {
		t.Fatal("job release: no baja ningún artefacto")
	}
	tap, ok := jobs["tap"]
	if !ok {
		t.Fatal("release.yml sin job tap")
	}
	if !strings.Contains(tap, "firmar verify -tag \"$VERSION\"") {
		t.Error("job tap: no comprueba la firma de SHA256SUMS (firmar verify -tag)")
	}
}
