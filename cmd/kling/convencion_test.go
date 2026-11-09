package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// mutantes son los comandos que cambian algo y con -json dicen qué dejaron
// (docs/guide.md, "CLI conventions"). Cada uno tiene su forma fijada en
// testdata/contrato/json.txt por TestContratoJSON.
var mutantes = []string{
	"freeze", "thaw", "pause", "stop", "rm", "start", "save",
	"machine squeeze", "machine resize", "machine secret", "machine credential", "cp",
}

// sinIdentidad son los que no devuelven una cosa con id o nombre: cp cuenta
// un fichero (machine y path).
var sinIdentidad = map[string]bool{"cp": true}

// La convención del CLI (docs/guide.md, "CLI conventions: output, JSON,
// flags") es contrato y se comprueba aquí, contra la guía, el código y la
// salida -json fijada en json.txt.
func TestConvencionCLI(t *testing.T) {
	guia, err := os.ReadFile("../../docs/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	seccion := seccionDe(string(guia), "## CLI conventions: output, JSON, flags")
	if seccion == "" {
		t.Fatal("docs/guide.md no tiene la sección de convenciones del CLI")
	}

	t.Run("la guía dice las claves", func(t *testing.T) {
		for _, w := range []string{"stdout", "stderr", "-json", "`id`", "`name`", "`state`",
			"`error`", "`code`", "`try`", "NO_COLOR", "help <command>", "exit code `2`"} {
			if !strings.Contains(seccion, w) {
				t.Errorf("la sección no menciona %s", w)
			}
		}
		for _, m := range mutantes {
			if !strings.Contains(seccion, "`"+m+"`") && !strings.Contains(seccion, strings.TrimPrefix(m, "machine ")) {
				t.Errorf("la sección no cuenta el -json de %s", m)
			}
		}
	})

	t.Run("el error JSON tiene error, code y try", func(t *testing.T) {
		var claves []string
		tp := reflect.TypeOf(errorJSON{})
		for i := 0; i < tp.NumField(); i++ {
			claves = append(claves, strings.Split(tp.Field(i).Tag.Get("json"), ",")[0])
		}
		if !slices.Equal(claves, []string{"error", "code", "try"}) {
			t.Fatalf("errorJSON: %v", claves)
		}
	})

	t.Run("la tabla de códigos de salida es la del código", func(t *testing.T) {
		tabla := seccionDe(string(guia), "## Exit codes")
		var doc []int
		for _, m := range regexp.MustCompile("(?m)^\\| `(\\d+)` \\|").FindAllStringSubmatch(tabla, -1) {
			n, _ := strconv.Atoi(m[1])
			doc = append(doc, n)
		}
		codigo := []int{0, salidaError, salidaUso, salidaDaemonViejo, salidaDoradoObsoleto, salidaAgenteSinCap, salidaInterrumpido}
		sort.Ints(doc)
		sort.Ints(codigo)
		if !slices.Equal(doc, codigo) {
			t.Fatalf("guía %v, código %v", doc, codigo)
		}
	})

	t.Run("-json es siempre un flag booleano", func(t *testing.T) {
		fset := token.NewFileSet()
		ficheros, _ := filepath.Glob("*.go")
		for _, f := range ficheros {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			af, err := parser.ParseFile(fset, f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(af, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				for i, a := range call.Args {
					lit, ok := a.(*ast.BasicLit)
					if !ok || lit.Value != `"json"` {
						continue
					}
					// fs.Bool("json", …) o fs.BoolVar(&v, "json", …).
					bien := sel.Sel.Name == "Bool" && i == 0 || sel.Sel.Name == "BoolVar" && i == 1
					if !bien && (sel.Sel.Name == "String" || sel.Sel.Name == "StringVar" || sel.Sel.Name == "Var" || sel.Sel.Name == "Int") {
						t.Errorf("%s: -json definido con %s: tiene que ser un bool", fset.Position(call.Pos()), sel.Sel.Name)
					}
				}
				return true
			})
		}
	})

	formas := formasJSON(t)
	t.Run("cada comando que cambia algo tiene -json", func(t *testing.T) {
		for _, m := range mutantes {
			if _, ok := formas[m]; !ok {
				t.Errorf("kling %s no está en comandosJSON (testdata/contrato/json.txt)", m)
			}
		}
	})

	t.Run("id, name y state, y state antes que status", func(t *testing.T) {
		for cmd, claves := range formas {
			if tipo, ok := claves["status"]; ok && tipo != "" {
				if _, ok := claves["state"]; !ok {
					t.Errorf("kling %s: status sin state", cmd)
				}
			}
			for _, k := range []string{"id", "name", "state", "error", "code"} {
				if tipo, ok := claves[k]; ok && tipo != "string" {
					t.Errorf("kling %s: %s es %s, no string", cmd, k, tipo)
				}
			}
			if slices.Contains(mutantes, cmd) && !sinIdentidad[cmd] {
				_, id := claves["id"]
				_, nombre := claves["name"]
				if !id && !nombre {
					t.Errorf("kling %s -json no dice el id ni el nombre de lo que tocó", cmd)
				}
			}
		}
	})
}

// seccionDe es el texto de un título de la guía hasta el siguiente del mismo
// nivel o superior.
func seccionDe(md, titulo string) string {
	i := strings.Index(md, "\n"+titulo+"\n")
	if i < 0 {
		return ""
	}
	resto := md[i+1+len(titulo):]
	if j := regexp.MustCompile(`\n#{1,2} `).FindStringIndex(resto); j != nil {
		resto = resto[:j[0]]
	}
	return resto
}

// formasJSON lee json.txt: por comando (sin flags ni argumentos de prueba),
// sus claves de primer nivel con su tipo. Los que devuelven la máquina tal
// cual ("= kling inspect m1") toman las de inspect.
func formasJSON(t *testing.T) map[string]map[string]string {
	t.Helper()
	b, err := os.ReadFile("testdata/contrato/json.txt")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]string{}
	var maquina map[string]string
	var comoMaquina []string
	for _, l := range strings.Split(string(b), "\n") {
		cab, cuerpo, ok := strings.Cut(l, ":  ")
		if !ok || !strings.HasPrefix(cab, "kling ") {
			continue
		}
		cmd := nombreDeComando(strings.TrimPrefix(cab, "kling "))
		if out[cmd] == nil {
			out[cmd] = map[string]string{}
		}
		if cuerpo == "= kling inspect m1" {
			comoMaquina = append(comoMaquina, cmd)
			continue
		}
		ruta, tipo, _ := strings.Cut(cuerpo, " ")
		ruta = strings.TrimPrefix(ruta, "[].")
		if strings.ContainsAny(ruta, ".[") {
			continue // solo el primer nivel
		}
		out[cmd][ruta] = tipo
		if cmd == "inspect" {
			maquina = out[cmd]
		}
	}
	for _, c := range comoMaquina {
		out[c] = maquina
	}
	return out
}

// nombreDeComando quita de "machine squeeze -json m1" los flags y lo que
// sigue: "machine squeeze".
func nombreDeComando(s string) string {
	var ws []string
	for _, w := range strings.Fields(s) {
		if !esPalabraDeComando(w) || w == "m1" || w == "tpl" {
			break
		}
		ws = append(ws, w)
	}
	return strings.Join(ws, " ")
}
