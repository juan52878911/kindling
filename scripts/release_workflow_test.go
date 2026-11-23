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

// F-03: el vmlinux y el min que se publican (los del job imagenes, que baja
// `kling try` en el primer uso) son los que arranca el e2e de Linux de la
// release: el job e2e depende de imagenes y le pasa el artefacto, y
// e2e-linux.yml, con images_artifact, usa esos en vez de compilar los suyos.
func TestReleaseWorkflowE2EArrancaLasImagenesPublicadas(t *testing.T) {
	jobs := jobsDeWorkflow(t, "../.github/workflows/release.yml")
	e2e, ok := jobs["e2e"]
	if !ok {
		t.Fatal("release.yml sin job e2e")
	}
	m := regexp.MustCompile(`(?m)^    needs:\s*\[([^\]]*)\]`).FindStringSubmatch(e2e)
	if m == nil || !strings.Contains(m[1], "imagenes") {
		t.Errorf("job e2e de release.yml no depende de imagenes:\n%s", e2e)
	}
	if !regexp.MustCompile(`(?m)^      images_artifact:\s*imagenes\s*$`).MatchString(e2e) {
		t.Errorf("job e2e de release.yml no pasa images_artifact: imagenes:\n%s", e2e)
	}

	b, err := os.ReadFile("../.github/workflows/e2e-linux.yml")
	if err != nil {
		t.Fatal(err)
	}
	cab, _, _ := strings.Cut(string(b), "\njobs:\n")
	_, call, ok := strings.Cut(cab, "  workflow_call:\n")
	if !ok || !strings.Contains(call, "      images_artifact:") {
		t.Fatal("e2e-linux.yml sin la entrada images_artifact de workflow_call")
	}
	lj := jobsDeWorkflow(t, "../.github/workflows/e2e-linux.yml")["e2e"]
	var baja, kernel, min string
	for _, p := range strings.Split(lj, "\n      - ") {
		switch {
		case strings.Contains(p, "actions/download-artifact@"):
			baja = p
		case strings.HasPrefix(p, "name: kernel\n"):
			kernel = p
		case strings.HasPrefix(p, "name: imagen min\n"):
			min = p
		}
	}
	if !strings.Contains(baja, "name: ${{ inputs.images_artifact }}") {
		t.Errorf("e2e-linux.yml no baja el artefacto de images_artifact:\n%s", baja)
	}
	for n, p := range map[string]string{"kernel": kernel, "imagen min": min} {
		if !strings.Contains(p, "inputs.images_artifact == ''") {
			t.Errorf("e2e-linux.yml: el paso %q no se salta con images_artifact:\n%s", n, p)
		}
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

// C-10: kling-unpack (rust/kling-unpack) se publica para las plataformas con
// daemon (linux amd64/arm64 estático y darwin arm64), compilado con su
// Cargo.lock, comprobado contra el camino de Go antes de subirlo y metido en
// el contrato de assets, así que entra en el SHA256SUMS que se firma.
func TestReleaseWorkflowPublicaKlingUnpack(t *testing.T) {
	jobs := jobsDeWorkflow(t, "../.github/workflows/release.yml")
	u, ok := jobs["unpack"]
	if !ok {
		t.Fatal("release.yml sin job unpack")
	}
	for _, s := range []string{"--locked", "x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl",
		"aarch64-apple-darwin", "KLING_UNPACK_TEST", "-run UnpackHelper", "persist-credentials: false"} {
		if !strings.Contains(u, s) {
			t.Errorf("job unpack sin %q", s)
		}
	}
	rel := jobs["release"]
	if !regexp.MustCompile(`(?m)^    needs: \[[^\]]*\bunpack\b`).MatchString(rel) {
		t.Error("job release no depende de unpack")
	}
	if !strings.Contains(rel, "pattern: unpack-*") {
		t.Error("job release no baja los artefactos unpack-*")
	}
	if !strings.Contains(rel, `"kling-unpack-linux-$arch"`) || !strings.Contains(rel, "kling-unpack-darwin-arm64") {
		t.Error("el contrato de assets no lleva kling-unpack")
	}
}
