package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/registry"
)

// planFor decide el runtime completo a partir del tipo de paquete: apk, canal
// de preinstalación y la familia de base que pickBase buscará en el daemon.
// Si npm y pypi divergieran aquí, un servidor de PyPI acabaría en una imagen
// sin python — el fallo se vería en el import como "no abrió el puerto".
func TestPlanFor(t *testing.T) {
	pkg := func(reg, id, ver string) registry.Package {
		return registry.Package{RegistryType: reg, Identifier: id, Version: ver}
	}

	npm, err := planFor(pkg("npm", "@scope/server", "1.2.3"))
	if err != nil {
		t.Fatalf("npm: %v", err)
	}
	if strings.Join(npm.apk, " ") != "nodejs npm" {
		t.Errorf("npm.apk = %v", npm.apk)
	}
	if len(npm.npm) != 1 || npm.npm[0] != "@scope/server@1.2.3" || len(npm.pip) != 0 {
		t.Errorf("npm debe preinstalar por npm y solo por npm: npm=%v pip=%v", npm.npm, npm.pip)
	}
	if npm.family != "node" {
		t.Errorf("npm.family = %q, want node", npm.family)
	}

	py, err := planFor(pkg("pypi", "Semgrep_MCP", "1.0.0"))
	if err != nil {
		t.Fatalf("pypi: %v", err)
	}
	if strings.Join(py.apk, " ") != "python3 py3-pip" {
		t.Errorf("pypi.apk = %v", py.apk)
	}
	// El especificador de pip es == y el nombre va normalizado (PEP 503): con
	// el nombre crudo, `pip install` y el registro hablarían de dos paquetes.
	if len(py.pip) != 1 || py.pip[0] != "semgrep-mcp==1.0.0" || len(py.npm) != 0 {
		t.Errorf("pypi debe preinstalar por pip y normalizado: pip=%v npm=%v", py.pip, py.npm)
	}
	if py.family != "python" {
		t.Errorf("pypi.family = %q, want python", py.family)
	}

	// Sin versión no debe quedar un separador colgando.
	sv, _ := planFor(pkg("pypi", "semgrep", ""))
	if sv.pip[0] != "semgrep" {
		t.Errorf("sin versión: pip=%v", sv.pip)
	}

	// Un tipo que Stdio() no debería dejar pasar falla aquí con nombre, no
	// construye una imagen a medias.
	if _, err := planFor(pkg("oci", "img", "")); err == nil {
		t.Error("oci debería rechazarse")
	}
}

// Los nombres de familia que pickBase busca en el daemon ("node", "python")
// tienen que ser los MISMOS que 70-build-minimal-image.sh acepta como preset.
// Los dos extremos están en lenguajes distintos y renombrar en uno no rompe el
// otro en compilación: se rompería como un `kling add` que empaqueta sobre
// `min` sin avisar y una capa 5 veces más gorda. Este test ata los extremos,
// igual que TestInitScriptsReadLayerParam ata kling.layer.
func TestFamiliasDeRuntimeCasanConElScriptDeBase(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "70-build-minimal-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for family, pkgs := range map[string]string{
		"node":   "nodejs npm",
		"python": "python3 py3-pip",
	} {
		re := regexp.MustCompile(regexp.QuoteMeta(family+`)`) + `\s+PKGS="` + regexp.QuoteMeta(pkgs) + `"`)
		if !re.MatchString(s) {
			t.Errorf("70-build-minimal-image.sh no tiene el preset %q -> %q que planFor promete", family, pkgs)
		}
	}
}
