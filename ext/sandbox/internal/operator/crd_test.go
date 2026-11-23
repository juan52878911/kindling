package operator

// Los dos crd.yaml (el de `kubectl apply` y el del chart de Helm) son copia a
// mano uno del otro, y los tipos de Go son la tercera copia de lo mismo: estos
// tests fallan si se separan.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const (
	crdPlano = "../../deploy/crd.yaml"
	crdHelm  = "../../../../deploy/helm/kindling-sandbox/crds/crd.yaml"
	rbacHelm = "../../../../deploy/helm/kindling-sandbox/templates/rbac.yaml"
	rbacPlan = "../../deploy/rbac.yaml"
)

func leer(t *testing.T, ruta string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(ruta))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCRDHelmEsCopiaDelPlano(t *testing.T) {
	a, b := leer(t, crdPlano), leer(t, crdHelm)
	if a != b {
		t.Fatalf("%s y %s difieren (%d y %d bytes): copia uno sobre el otro", crdPlano, crdHelm, len(a), len(b))
	}
}

func TestCRDDeclaraLosDosRecursos(t *testing.T) {
	crd := leer(t, crdPlano)
	for _, q := range []string{"name: sandboxes.sandbox.kindling.dev", "name: sandboxpools.sandbox.kindling.dev", "kind: SandboxPool", "plural: " + PoolResource} {
		if !strings.Contains(crd, q) {
			t.Errorf("falta %q en el CRD", q)
		}
	}
}

// propiedadesJSON son los nombres JSON de los campos de un struct.
func propiedadesJSON(v any) []string {
	var out []string
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		if n, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); n != "" && n != "-" {
			out = append(out, n)
		}
	}
	return out
}

func TestCRDTieneTodosLosCamposDeLosTipos(t *testing.T) {
	crd := leer(t, crdPlano)
	casos := map[string][]string{
		"SandboxSpec":   propiedadesJSON(SandboxSpec{}),
		"VolumeSpec":    propiedadesJSON(VolumeSpec{}),
		"SandboxStatus": propiedadesJSON(SandboxStatus{}),
		"PoolSpec":      propiedadesJSON(PoolSpec{}),
		"PoolStatus":    propiedadesJSON(PoolStatus{}),
	}
	for tipo, campos := range casos {
		for _, c := range campos {
			if ok, _ := regexp.MatchString(`(?m)^\s+`+regexp.QuoteMeta(c)+`:\s*$`, crd); !ok {
				t.Errorf("%s.%s no está como propiedad en el CRD", tipo, c)
			}
		}
	}
	if !strings.Contains(crd, "enum: [none, internet, allowlist]") {
		t.Error("egress del CRD no admite allowlist")
	}
}

func TestRBACDaPermisoSobreSandboxPools(t *testing.T) {
	for _, f := range []string{rbacPlan, rbacHelm} {
		r := leer(t, f)
		for _, q := range []string{`resources: ["sandboxpools"]`, `resources: ["sandboxpools/status"]`} {
			if !strings.Contains(r, q) {
				t.Errorf("%s: falta %s", f, q)
			}
		}
	}
}
