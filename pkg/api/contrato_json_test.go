package api

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/contrato"
)

var actualizar = flag.Bool("update", false, "regenerate testdata/json-tags.txt")

// La forma JSON de pkg/api es contrato (docs/compatibilidad.md): lo que el
// daemon manda y lo que leen el CLI, las extensiones y clientes ajenos. Este
// test saca del código fuente cada campo de cada struct exportado (nombre en
// el JSON, opciones y tipo Go), cada tipo exportado que no es struct y cada
// constante de cadena exportada (estados, capacidades, códigos de error), y
// los compara con testdata/json-tags.txt:
//
//   - quitar, renombrar o cambiar de tipo un campo, o cambiar el valor de una
//     constante, es incompatible: hay que subir APIVersion (contrato.Comparar);
//   - añadir pide regenerar: go test ./pkg/api -run TestFormaJSON -update
//
// Se lee el fuente y no reflect porque reflect no sabe enumerar los tipos de
// un paquete: un struct nuevo quedaría fuera sin que nadie lo notara.
func TestFormaJSON(t *testing.T) {
	lineas, err := formaJSON(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(lineas) < 500 {
		t.Fatalf("solo %d líneas: ¿cambió la forma de leer el paquete?", len(lineas))
	}
	cab := []string{
		"Forma JSON de pkg/api: <Tipo>.<Campo> <tipo Go> json:<etiqueta>, los tipos",
		"exportados que no son struct y las constantes de cadena exportadas.",
		"Lo genera TestFormaJSON (pkg/api/contrato_json_test.go); para regenerarlo:",
		"  go test ./pkg/api -run TestFormaJSON -update",
		"Quitar o cambiar una línea exige subir api.APIVersion (internal/contrato).",
	}
	if err := contrato.Comparar("testdata/json-tags.txt", APIVersion, cab, lineas, *actualizar); err != nil {
		t.Fatal(err)
	}
}

// goldensDelContrato son todos los goldens que siguen la regla de
// APIVersion (apiversion.go). Si se añade uno, va aquí.
var goldensDelContrato = []string{
	"testdata/json-tags.txt",
	"../../internal/daemon/testdata/contrato/rutas.txt",
	"../../internal/daemon/testdata/contrato/codigos.txt",
	"../../cmd/kling/testdata/contrato/json.txt",
}

// Todos los goldens apuntan el APIVersion de ahora: subirla obliga a
// regenerarlos todos (y a revisar su diff), no solo el del test que se corrió.
func TestGoldensConAPIVersion(t *testing.T) {
	for _, g := range goldensDelContrato {
		lineas, api, err := contrato.Leer(g)
		if err != nil {
			t.Errorf("%s: %v", g, err)
			continue
		}
		if api != APIVersion {
			t.Errorf("%s was generated with api %d and APIVersion is %d: regenerate it with go test -update", g, api, APIVersion)
		}
		if len(lineas) == 0 {
			t.Errorf("%s is empty", g)
		}
	}
}

func formaJSON(dir string) ([]string, error) {
	fset := token.NewFileSet()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, dir+"/"+n, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range gd.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					if !sp.Name.IsExported() {
						continue
					}
					st, ok := sp.Type.(*ast.StructType)
					if !ok {
						out = append(out, "type "+sp.Name.Name+" "+types.ExprString(sp.Type))
						continue
					}
					out = append(out, camposJSON(sp.Name.Name, st)...)
				case *ast.ValueSpec:
					if gd.Tok != token.CONST {
						continue
					}
					for i, nm := range sp.Names {
						if !nm.IsExported() || i >= len(sp.Values) {
							continue
						}
						lit, ok := sp.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						tipo := ""
						if sp.Type != nil {
							tipo = types.ExprString(sp.Type) + " "
						}
						out = append(out, "const "+nm.Name+" "+tipo+"= "+lit.Value)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// camposJSON son las líneas de un struct: una por campo exportado (los no
// exportados no salen en el JSON) y una por campo embebido.
func camposJSON(tipo string, st *ast.StructType) []string {
	var out []string
	for _, f := range st.Fields.List {
		etiqueta := "(none)"
		if f.Tag != nil {
			s, _ := strconv.Unquote(f.Tag.Value)
			if j, ok := reflect.StructTag(s).Lookup("json"); ok {
				etiqueta = strconv.Quote(j)
			}
		}
		texto := types.ExprString(f.Type)
		if len(f.Names) == 0 {
			out = append(out, tipo+".(embedded) "+texto+" json:"+etiqueta)
			continue
		}
		for _, n := range f.Names {
			if n.IsExported() {
				out = append(out, tipo+"."+n.Name+" "+texto+" json:"+etiqueta)
			}
		}
	}
	return out
}
