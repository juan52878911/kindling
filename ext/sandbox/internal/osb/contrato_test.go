package osb

import (
	"bufio"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// operacion es una operación de una spec de OSB copiada en testdata/.
type operacion struct{ spec, metodo, ruta string }

var reParam = regexp.MustCompile(`\{[^}]+\}`)

// operacionesDe lee las operaciones de una spec OpenAPI sin dependencias: las
// rutas van a dos espacios bajo `paths:` y los métodos a cuatro.
func operacionesDe(t *testing.T, fichero, spec string) []operacion {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", fichero))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []operacion
	enPaths, ruta := false, ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case l == "paths:":
			enPaths = true
		case enPaths && l != "" && l[0] != ' ' && l[0] != '#':
			enPaths = false
		case enPaths && strings.HasPrefix(l, "  /") && strings.HasSuffix(l, ":") && !strings.HasPrefix(l, "   "):
			ruta = strings.TrimSuffix(strings.TrimSpace(l), ":")
		case enPaths && ruta != "":
			for _, m := range []string{"get", "post", "put", "patch", "delete"} {
				if l == "    "+m+":" {
					out = append(out, operacion{spec, strings.ToUpper(m), ruta})
				}
			}
		}
	}
	return out
}

func todasLasOperaciones(t *testing.T) []operacion {
	ops := append(operacionesDe(t, "sandbox-lifecycle.yml", specLifecycle), operacionesDe(t, "execd-api.yaml", specExecd)...)
	if len(ops) != 90 { // 30 + 60 en 1.2.1: si cambia, la spec copiada no es la esperada
		t.Fatalf("operaciones leídas de las specs: %d, esperaba 90", len(ops))
	}
	return ops
}

func declarada(o operacion) *Declarada {
	for i, d := range NoImplementadas {
		if d.Spec == o.spec && d.Metodo == o.metodo && d.Ruta == o.ruta {
			return &NoImplementadas[i]
		}
	}
	return nil
}

// TestContrato recorre cada operación de las dos specs de OSB 1.2.1: o la sirve
// el shim (ni 404 de ruta, ni 405, ni 501) o está en NoImplementadas y
// contesta 501 con su motivo.
func TestContrato(t *testing.T) {
	b := nuevoBanco(t)
	hechas, no := 0, 0
	for _, o := range todasLasOperaciones(t) {
		ruta := reParam.ReplaceAllStringFunc(o.ruta, func(p string) string {
			switch p {
			case "{sandboxId}":
				return idOSB
			case "{port}":
				return "44772"
			case "{uid}", "{gid}":
				return "1000"
			}
			return "x"
		})
		cuerpo := "{}"
		if strings.Contains(o.ruta, "networkpolicy") && (o.metodo == "PATCH" || o.metodo == "DELETE") || o.ruta == "/files/mv" {
			cuerpo = "[]"
		}
		var st int
		var m map[string]any
		if o.spec == specLifecycle {
			st, m, _ = b.pedir(t, o.metodo, "/v1"+ruta, cuerpo)
		} else {
			cab := map[string]string{"X-EXECD-ACCESS-TOKEN": b.s.tokenExecd(idGW)}
			st, m, _ = b.pedirCon(t, o.metodo, "/sandboxes/"+idOSB+"/port/44772"+ruta, cab, cuerpo)
		}
		nombre := o.spec + " " + o.metodo + " " + o.ruta
		if st == http.StatusMethodNotAllowed || (st == 404 && m["code"] == codigoRuta) {
			t.Errorf("%s: sin ruta (%d %v)", nombre, st, m)
			continue
		}
		if d := declarada(o); d != nil {
			no++
			if st != 501 || m["message"] != d.Motivo {
				t.Errorf("%s: declarada 501 y contesta %d %v", nombre, st, m)
			}
			continue
		}
		hechas++
		if st == 501 {
			t.Errorf("%s: 501 sin declarar (%v)", nombre, m)
		}
	}
	t.Logf("OSB %s: %d operaciones implementadas, %d declaradas 501", VersionOSB, hechas, no)
}

// TestDeclaradasEnSpecYDoc exige que cada 501 declarado exista en su spec (nada
// caducado) y que docs/opensandbox.md lo liste.
func TestDeclaradasEnSpecYDoc(t *testing.T) {
	ops := map[operacion]bool{}
	for _, o := range todasLasOperaciones(t) {
		ops[o] = true
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "opensandbox.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range NoImplementadas {
		if d.Spec != specDiagnostico && !ops[operacion{d.Spec, d.Metodo, d.Ruta}] {
			t.Errorf("%s %s %s: declarada pero no está en la spec", d.Spec, d.Metodo, d.Ruta)
		}
		if !strings.Contains(string(doc), "`"+d.Metodo+" "+d.Ruta+"`") {
			t.Errorf("docs/opensandbox.md no lista `%s %s`", d.Metodo, d.Ruta)
		}
	}
	notice, err := os.ReadFile(filepath.Join("testdata", "NOTICE"))
	if err != nil || !strings.Contains(string(notice), "release-"+VersionOSB) || !strings.Contains(string(notice), "Apache License") {
		t.Fatalf("testdata/NOTICE debe citar release-%s y la licencia: %v", VersionOSB, err)
	}
	if !strings.Contains(string(doc), VersionOSB) {
		t.Fatalf("la doc no fija la versión %s", VersionOSB)
	}
}
