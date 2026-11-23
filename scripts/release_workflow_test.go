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

// B17: los jobs que compilan lo que se firma (build y vz) no restauran la
// caché de Go, que puede haber escrito cualquier otro workflow del repo; y
// ningún checkout deja el token del job en .git/config, donde lo lee
// cualquier paso posterior (el de release tiene contents: write).
func TestReleaseWorkflowSinCacheNiCredenciales(t *testing.T) {
	jobs := jobsDeWorkflow(t, "../.github/workflows/release.yml")
	for _, nombre := range []string{"build", "vz"} {
		j, ok := jobs[nombre]
		if !ok {
			t.Fatalf("release.yml sin job %s", nombre)
		}
		for _, p := range strings.Split(j, "\n      - ") {
			if !strings.Contains(p, "actions/setup-go@") {
				continue
			}
			if !regexp.MustCompile(`(?m)^\s+cache:\s*false\s*$`).MatchString(p) {
				t.Errorf("job %s: setup-go sin cache: false:\n%s", nombre, p)
			}
		}
	}
	checkouts := 0
	for nombre, j := range jobs {
		for _, p := range strings.Split(j, "\n      - ") {
			if !strings.Contains(p, "actions/checkout@") {
				continue
			}
			checkouts++
			if !regexp.MustCompile(`(?m)^\s+persist-credentials:\s*false\s*$`).MatchString(p) {
				t.Errorf("job %s: checkout sin persist-credentials: false:\n%s", nombre, p)
			}
		}
	}
	if checkouts == 0 {
		t.Fatal("release.yml sin ningún checkout")
	}
}

// F-02: release.yml no se fía de que la etiqueta saliera de release.sh. Desde
// 1.0 el job release pide el commit status del e2e de kling-hv (el mismo
// e2e-hv-estado.sh que release.sh) antes de firmar nada, con permiso de
// lectura de los status y el token del job.
func TestReleaseWorkflowExigeElE2EDeKlingHV(t *testing.T) {
	jobs := jobsDeWorkflow(t, "../.github/workflows/release.yml")
	rel, ok := jobs["release"]
	if !ok {
		t.Fatal("release.yml sin job release")
	}
	if !regexp.MustCompile(`(?m)^      statuses:\s*read\b`).MatchString(rel) {
		t.Error("job release sin permissions statuses: read")
	}
	puerta, firma := -1, -1
	for i, p := range strings.Split(rel, "\n      - ") {
		if puerta < 0 && strings.Contains(p, "scripts/e2e-hv-estado.sh") {
			puerta = i
			if !strings.Contains(p, "GH_TOKEN:") {
				t.Errorf("el paso del e2e de kling-hv sin GH_TOKEN:\n%s", p)
			}
		}
		if firma < 0 && strings.Contains(p, "name: firmar SHA256SUMS") {
			firma = i
		}
	}
	if puerta < 0 {
		t.Fatal("job release: ningún paso corre scripts/e2e-hv-estado.sh")
	}
	if firma < 0 || puerta > firma {
		t.Errorf("job release: el e2e de kling-hv (paso %d) no va antes de firmar (paso %d)", puerta, firma)
	}
}
