package scripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// release.sh es la puerta de cada release: estos tests lo corren con
// --dry-run sobre un repo de usar y tirar, con un origin local y un gh falso
// que contesta las check runs y los commit status desde ficheros (y aplica el
// --jq con el jq del sistema, como haría gh). Comprueban que la puerta se
// cierra de verdad: sin el e2e de kling-hv, con un job obligatorio saltado o
// con entradas en [Unreleased], -n sale con error aunque enseñe las notas.

const changelogListo = `# Changelog

## [Unreleased]

## [1.0.0] - 2026-11-10

### Added

- Something.

[Unreleased]: https://example.invalid/compare/v1.0.0...HEAD
`

// Los jobs de ci.yml tal como los nombra GitHub en un push a main.
var jobsCI = []string{
	"make test cross (.)", "make test cross (ext/db)", "make test cross (ext/mcp)",
	"make test cross (ext/phone)", "make test cross (ext/sandbox)",
	"núcleo sin dependencias ni cgo", "kling-unpack (Rust)", "go.work",
	"clon real (xfs)", "clon real (btrfs)", "kling-vz and kling-hv (macOS)",
	"clon real (apfs)", "lint (govulncheck, staticcheck, shellcheck, apidiff)",
}

type checkRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

func runsVerdes(extra ...checkRun) []checkRun {
	var r []checkRun
	for _, n := range jobsCI {
		r = append(r, checkRun{n, "completed", "success"})
	}
	return append(r, extra...)
}

type escenario struct {
	runs      []checkRun
	statusHV  string // "" sin commit status e2e-mac/kling-hv
	changelog string
	env       []string
}

// correrRelease monta el repo, corre release.sh -n v1.0.0 y devuelve la
// salida combinada y si salió con 0.
func correrRelease(t *testing.T, e escenario) (string, bool) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("sin jq: el gh falso lo necesita para el --jq")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("sin git")
	}
	raiz := t.TempDir()
	repo := filepath.Join(raiz, "repo")
	origin := filepath.Join(raiz, "origin.git")
	falsos := filepath.Join(raiz, "bin")
	datos := filepath.Join(raiz, "datos")
	for _, d := range []string{filepath.Join(repo, "scripts"), falsos, datos} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"release.sh", "release-notes.sh"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		escribir(t, filepath.Join(repo, "scripts", f), string(b), 0o755)
	}
	escribir(t, filepath.Join(repo, "scripts", "install.sh"), "KLING_RELEASE_PUBKEYS=\"clave\"\n", 0o755)
	cl := e.changelog
	if cl == "" {
		cl = changelogListo
	}
	escribir(t, filepath.Join(repo, "CHANGELOG.md"), cl, 0o644)

	// go falso: `firmar check` no es lo que se prueba aquí.
	escribir(t, filepath.Join(falsos, "go"), "#!/bin/sh\nexit 0\n", 0o755)
	// gh falso: gh api URL [--jq FILTRO], con las respuestas en $DATOS.
	escribir(t, filepath.Join(falsos, "gh"), `#!/bin/sh
[ "$1" = api ] || exit 1
url=$2; shift 2
case "$url" in
  *check-runs*) f="$DATOS/check-runs.json" ;;
  */status)     f="$DATOS/status.json" ;;
  *) exit 1 ;;
esac
if [ "${1:-}" = --jq ]; then exec jq -r "$2" "$f"; fi
cat "$f"
`, 0o755)
	runs, _ := json.Marshal(map[string]any{"check_runs": e.runs})
	escribir(t, filepath.Join(datos, "check-runs.json"), string(runs), 0o644)
	var st []map[string]string
	if e.statusHV != "" {
		st = append(st, map[string]string{"context": "e2e-mac/kling-hv", "state": e.statusHV})
	}
	stj, _ := json.Marshal(map[string]any{"statuses": st})
	escribir(t, filepath.Join(datos, "status.json"), string(stj), 0o644)

	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(raiz, "init", "-q", "--bare", "-b", "main", origin)
	git(repo, "init", "-q", "-b", "main")
	git(repo, "add", ".")
	git(repo, "commit", "-q", "-m", "x")
	git(repo, "remote", "add", "origin", origin)
	git(repo, "push", "-q", "origin", "main")

	cmd := exec.Command("sh", "scripts/release.sh", "-n", "v1.0.0")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PATH="+falsos+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DATOS="+datos, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "KLING_E2E_HV=", "VERSION=")
	sha, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range e.env {
		cmd.Env = append(cmd.Env, strings.ReplaceAll(v, "@SHA@", strings.TrimSpace(string(sha))))
	}
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func escribir(t *testing.T, path, s string, modo os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), modo); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseDryRunPasaConTodoVerde(t *testing.T) {
	out, ok := correrRelease(t, escenario{
		// Lo que no es obligatorio puede saltarse (el e2e de Linux sin KVM).
		runs:     runsVerdes(checkRun{"microVMs (Firecracker, KVM)", "completed", "skipped"}),
		statusHV: "success",
	})
	if !ok {
		t.Fatalf("release.sh -n falló con todo en verde:\n%s", out)
	}
	if !strings.Contains(out, "Something.") {
		t.Errorf("las notas no salen en el dry-run:\n%s", out)
	}
}

// Sin el commit status del e2e de kling-hv no hay release 1.x, y la línea a
// mano (KLING_E2E_HV) ya no lo sustituye: era un prefijo del sha que nadie
// comprobaba que se hubiese corrido.
func TestReleaseSinE2EHVFallaAunqueHayaKLING_E2E_HV(t *testing.T) {
	for _, env := range [][]string{nil, {"KLING_E2E_HV=0000000"}} {
		e := escenario{runs: runsVerdes(), env: env}
		out, ok := correrRelease(t, e)
		if ok {
			t.Fatalf("release.sh -n %v sin status e2e-mac/kling-hv salió con 0:\n%s", env, out)
		}
		if !strings.Contains(out, "kling-hv e2e not run") {
			t.Errorf("%v: no dice qué falta:\n%s", env, out)
		}
	}
	// KLING_E2E_HV con el sha de verdad tampoco vale.
	out, ok := correrRelease(t, escenario{runs: runsVerdes(), env: []string{"KLING_E2E_HV=@SHA@"}})
	if ok {
		t.Fatalf("KLING_E2E_HV sigue abriendo la puerta:\n%s", out)
	}
	// Un status en failure tampoco.
	if out, ok := correrRelease(t, escenario{runs: runsVerdes(), statusHV: "failure"}); ok {
		t.Fatalf("status en failure aceptado:\n%s", out)
	}
}

func TestReleaseJobObligatorioSaltadoNoEsVerde(t *testing.T) {
	runs := runsVerdes()
	for i := range runs {
		if strings.HasPrefix(runs[i].Name, "lint") {
			runs[i].Conclusion = "skipped"
		}
	}
	out, ok := correrRelease(t, escenario{runs: runs, statusHV: "success"})
	if ok || !strings.Contains(out, "lint") {
		t.Fatalf("un lint saltado contó como verde (ok=%v):\n%s", ok, out)
	}
	// Y uno que ni aparece (se borró del workflow o no llegó a correr).
	out, ok = correrRelease(t, escenario{runs: runsVerdes()[1:], statusHV: "success"})
	if ok {
		t.Fatalf("sin \"make test cross (.)\" la puerta se abrió:\n%s", out)
	}
	runs = runsVerdes()
	out, ok = correrRelease(t, escenario{runs: runs[:len(runs)-1], statusHV: "success"})
	if ok || !strings.Contains(out, "lint") {
		t.Fatalf("sin lint la puerta se abrió (ok=%v):\n%s", ok, out)
	}
}

// Una etiqueta lleva todo lo de main: lo que siga en [Unreleased] saldría en
// el binario y no en sus notas.
func TestReleaseSeNiegaConUnreleasedNoVacio(t *testing.T) {
	cl := strings.Replace(changelogListo, "## [Unreleased]\n", "## [Unreleased]\n\n### Fixed\n\n- Pending.\n", 1)
	out, ok := correrRelease(t, escenario{runs: runsVerdes(), statusHV: "success", changelog: cl})
	if ok || !strings.Contains(out, "[Unreleased]") {
		t.Fatalf("[Unreleased] con entradas no paró la release (ok=%v):\n%s", ok, out)
	}
}

// La lista de jobs obligatorios de release.sh y los jobs de ci.yml no pueden
// separarse: un job nuevo que nadie añada a la lista podría saltarse sin que
// la puerta lo note, y uno que ya no existe la cerraría para siempre. Los
// nombres con ${{ matrix.K }} se expanden con la lista en línea `K: [a, b]`
// de su job.
func TestReleaseObligatoriosSonLosJobsDeCI(t *testing.T) {
	ci, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := os.ReadFile("release.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`CI_OBLIGATORIOS='([^']*)'`).FindSubmatch(rel)
	if m == nil {
		t.Fatal("release.sh sin CI_OBLIGATORIOS")
	}
	obl := map[string]bool{}
	for _, n := range strings.Split(string(m[1]), "\n") {
		obl[n] = true
	}

	i := strings.Index(string(ci), "\njobs:\n")
	if i < 0 {
		t.Fatal("ci.yml sin jobs:")
	}
	nombreRe := regexp.MustCompile(`(?m)^    name: (.+)$`)
	matrizRe := regexp.MustCompile(`\$\{\{ matrix\.(\w+) \}\}`)
	var deCI []string
	// Un bloque por job: las claves de dos espacios bajo jobs:.
	for _, bloque := range regexp.MustCompile(`(?m)^  [\w-]+:$`).Split(string(ci[i:]), -1)[1:] {
		nm := nombreRe.FindStringSubmatch(bloque)
		if nm == nil {
			t.Fatalf("job sin name: en ci.yml:\n%.200s", bloque)
		}
		nombre := nm[1]
		k := matrizRe.FindStringSubmatch(nombre)
		if k == nil {
			deCI = append(deCI, nombre)
			continue
		}
		vals := regexp.MustCompile(`(?m)^        ` + k[1] + `: \[(.*)\]$`).FindStringSubmatch(bloque)
		if vals == nil {
			t.Fatalf("%q: sin la lista en línea de matrix.%s", nombre, k[1])
		}
		for _, v := range strings.Split(vals[1], ",") {
			deCI = append(deCI, strings.Replace(nombre, k[0], strings.TrimSpace(v), 1))
		}
	}
	for _, n := range deCI {
		if !obl[n] {
			t.Errorf("el job %q de ci.yml no está en CI_OBLIGATORIOS de release.sh", n)
		}
		delete(obl, n)
	}
	for n := range obl {
		t.Errorf("CI_OBLIGATORIOS tiene %q, que ci.yml ya no corre", n)
	}
	// Y el gh falso de estos tests contesta con los mismos.
	if strings.Join(deCI, "|") != strings.Join(jobsCI, "|") {
		t.Errorf("jobsCI del test se ha separado de ci.yml:\n ci.yml %q\n test   %q", deCI, jobsCI)
	}
}
