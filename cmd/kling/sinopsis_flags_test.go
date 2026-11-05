package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/contrato"
	"github.com/juan52878911/kindling/pkg/api"
)

// La sinopsis de tree.go se escribe a mano y se queda atrás cuando un comando
// gana un flag. Este test lee el código de cmd/kling (sin ejecutarlo: los
// comandos crean su FlagSet dentro de la función y hablan con el daemon) y
// comprueba que cada flag de cada FlagSet con nombre literal sale en el bloque
// de uso de su comando.

// flagsSinSinopsis son los flags que no salen en la sinopsis a propósito:
// "*" vale para todos los comandos.
var flagsSinSinopsis = map[string][]string{
	// -H es global: lo explica CONNECTION al pie de la ayuda.
	"*": {"H"},
	// flagsCompose da -f, -p y -H a cada subcomando; la sinopsis los enseña
	// en up, down y ps y el resto se lee igual.
	"compose logs":    {"p"},
	"compose exec":    {"f", "p"},
	"compose start":   {"f", "p"},
	"compose stop":    {"f", "p"},
	"compose restart": {"f", "p"},
}

// fsSinonimos traduce el nombre con que se crea un FlagSet al comando de
// tree.go cuando no coinciden (nombres de antes de 1.0).
var fsSinonimos = map[string]string{
	"credential": "machine credential",
	"audit":      "machine audit",
	"squeeze":    "machine squeeze",
	"mmds":       "machine secret",
}

// fsSinArbol son los prefijos de FlagSets cuya ayuda no vive en tree.go: los
// de la extensión ai (builtins.go) y los internos.
var fsSinArbol = []string{"ai ", "models ", "chispa ", "builder", "dial-stdio"}

// metodosQueDefinen son los métodos de *flag.FlagSet que registran un flag.
var metodosQueDefinen = map[string]bool{
	"Bool": true, "BoolVar": true, "String": true, "StringVar": true,
	"Int": true, "IntVar": true, "Int64": true, "Int64Var": true,
	"Uint": true, "UintVar": true, "Uint64": true, "Uint64Var": true,
	"Float64": true, "Float64Var": true, "Duration": true, "DurationVar": true,
	"Var": true, "Func": true, "BoolFunc": true, "TextVar": true,
}

// primerLiteral es la primera cadena literal de args: el nombre del flag en
// fs.Bool("x", …), fs.StringVar(&v, "x", …) y units.MiBVar(fs, "x", …).
func primerLiteral(args []ast.Expr) (string, bool) {
	for _, a := range args {
		if l, ok := a.(*ast.BasicLit); ok && l.Kind == token.STRING {
			s, err := strconv.Unquote(l.Value)
			return s, err == nil
		}
	}
	return "", false
}

// esFlagSet dice si la expresión de tipo es *flag.FlagSet.
func esFlagSet(e ast.Expr) bool {
	st, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := st.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "flag" && sel.Sel.Name == "FlagSet"
}

// flagDef es un flag registrado y la variable en la que escribe: dos flags
// con la misma (-f y -file, -d y -detach) son el mismo con dos nombres, y
// basta con que la sinopsis enseñe uno.
type flagDef struct{ name, dest string }

type analizadorFlags struct {
	funcs     map[string]*ast.FuncDecl
	ayudantes map[string][]flagDef // flags que registra cada func(fs *flag.FlagSet, …)
}

// destino es la variable de fs.XxxVar(&v, …) o fs.Var(v, …), o "".
func destino(metodo string, args []ast.Expr) string {
	if (metodo == "Var" || strings.HasSuffix(metodo, "Var")) && len(args) > 0 {
		e := args[0]
		if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
			e = u.X
		}
		return types.ExprString(e)
	}
	return ""
}

// flagsDe son los flags que body registra sobre la variable v, siguiendo las
// funciones del paquete a las que se le pasa como primer argumento.
func (a *analizadorFlags) flagsDe(body ast.Node, v string, vistos map[string]bool) []flagDef {
	var out []flagDef
	// asignados: la variable que recibe x := fs.Bool("d", …), para casarla con
	// un fs.BoolVar(x, "detach", …) posterior.
	asignados := map[*ast.CallExpr]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
			for i, r := range as.Rhs {
				if c, ok := r.(*ast.CallExpr); ok {
					asignados[c] = types.ExprString(as.Lhs[i])
				}
			}
			return true
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if id, ok := fn.X.(*ast.Ident); ok && id.Name == v && metodosQueDefinen[fn.Sel.Name] {
				if s, ok := primerLiteral(call.Args); ok {
					d := destino(fn.Sel.Name, call.Args)
					if d == "" {
						d = asignados[call]
					}
					out = append(out, flagDef{s, d})
				}
				return true
			}
			// ef.register(fs): un método del paquete que registra sobre fs.
			if len(call.Args) > 0 {
				if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == v && a.funcs["."+fn.Sel.Name] != nil {
					out = append(out, a.ayudante("."+fn.Sel.Name, vistos)...)
					return true
				}
			}
			// units.MiBVar(fs, "mem", …) y compañía.
			if id, ok := fn.X.(*ast.Ident); ok && id.Name == "units" && len(call.Args) > 0 {
				if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == v {
					if s, ok := primerLiteral(call.Args[1:]); ok {
						out = append(out, flagDef{s, asignados[call]})
					}
				}
			}
		case *ast.Ident:
			if len(call.Args) == 0 {
				return true
			}
			if arg, ok := call.Args[0].(*ast.Ident); !ok || arg.Name != v {
				return true
			}
			if fn.Name == "flagRetirado" {
				return true // el nombre viejo solo existe para decir el nuevo
			}
			out = append(out, a.ayudante(fn.Name, vistos)...)
		}
		return true
	})
	return out
}

// ayudante son los flags que la función del paquete name registra sobre su
// primer parámetro, si es un *flag.FlagSet.
func (a *analizadorFlags) ayudante(name string, vistos map[string]bool) []flagDef {
	if fl, ok := a.ayudantes[name]; ok {
		return fl
	}
	fd := a.funcs[name]
	if fd == nil || vistos[name] || fd.Type.Params == nil || len(fd.Type.Params.List) == 0 {
		return nil
	}
	p := fd.Type.Params.List[0]
	if !esFlagSet(p.Type) || len(p.Names) == 0 {
		return nil
	}
	vistos[name] = true
	fl := a.flagsDe(fd.Body, p.Names[0].Name, vistos)
	for i := range fl {
		if fl[i].dest != "" {
			fl[i].dest = name + "." + fl[i].dest // sus variables no son las del que llama
		}
	}
	a.ayudantes[name] = fl
	return fl
}

// sinopsisDe es el bloque de uso de tree.go del comando de nombre fsName
// ("image import", "logs"), o "" si tree.go no lo tiene.
func sinopsisDe(fsName string) string {
	if s, ok := fsSinonimos[fsName]; ok {
		fsName = s
	}
	words := strings.Fields(fsName)
	if to, ok := removedAliases[words[0]]; ok {
		words = append(strings.Fields(to), words[1:]...)
	} else if to, ok := aliases[words[0]]; ok {
		words = append(strings.Fields(to), words[1:]...)
	}
	c := coreCommand(words[0])
	if c == nil {
		return ""
	}
	if len(words) > 1 && hasSub(c, words[1]) {
		// "context [ls]": el subcomando por defecto va en la línea del padre.
		if b := subBlock(c, words[1]); b != "" {
			return b
		}
	}
	return c.Usage
}

// paqueteFlags lee el código de cmd/kling (sin los tests) y prepara el
// analizador de flags con sus funciones.
func paqueteFlags(t *testing.T) (*token.FileSet, *analizadorFlags, []*ast.File) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	a := &analizadorFlags{funcs: map[string]*ast.FuncDecl{}, ayudantes: map[string][]flagDef{}}
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				// Los métodos (ef.register(fs)) van como ".nombre": desde la
				// llamada no se sabe el tipo del receptor.
				if fd.Recv != nil {
					a.funcs["."+fd.Name.Name] = fd
				} else {
					a.funcs[fd.Name.Name] = fd
				}
			}
		}
	}
	return fset, a, files
}

// porCadaFlagSet llama a fn con cada `v := flag.NewFlagSet("nombre", …)` de
// nombre literal: la función que lo crea, la variable y el nombre. Los de
// nombre calculado ("compose "+que, op) no se pueden casar con nada desde aquí.
func porCadaFlagSet(files []*ast.File, fn func(as *ast.AssignStmt, fd *ast.FuncDecl, v, fsName string)) {
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
					return true
				}
				call, ok := as.Rhs[0].(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "NewFlagSet" {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "flag" {
					return true
				}
				v, ok := as.Lhs[0].(*ast.Ident)
				if !ok {
					return true
				}
				if fsName, ok := primerLiteral(call.Args[:1]); ok {
					fn(as, fd, v.Name, fsName)
				}
				return true
			})
		}
	}
}

func TestSinopsisListaCadaFlag(t *testing.T) {
	fset, a, files := paqueteFlags(t)
	comprobados := 0
	porCadaFlagSet(files, func(as *ast.AssignStmt, fd *ast.FuncDecl, v, fsName string) {
		for _, p := range fsSinArbol {
			if strings.HasPrefix(fsName, p) {
				return
			}
		}
		syn := sinopsisDe(fsName)
		pos := fset.Position(as.Pos())
		if syn == "" {
			t.Errorf("%s: FlagSet %q has no synopsis in tree.go", pos, fsName)
			return
		}
		comprobados++
		permitidos := map[string]bool{}
		for _, k := range []string{"*", fsName} {
			for _, n := range flagsSinSinopsis[k] {
				permitidos[n] = true
			}
		}
		// Por variable destino: falta si no sale ninguno de sus nombres.
		enSinopsis := func(fl string) bool {
			re := regexp.MustCompile(`(^|[^\w-])-` + regexp.QuoteMeta(fl) + `($|[^\w-])`)
			return permitidos[fl] || re.MatchString(syn)
		}
		var orden []string
		grupos := map[string][]string{}
		for _, fl := range a.flagsDe(fd.Body, v, map[string]bool{}) {
			k := fl.dest
			if k == "" {
				k = "-" + fl.name
			}
			if _, ok := grupos[k]; !ok {
				orden = append(orden, k)
			}
			grupos[k] = append(grupos[k], fl.name)
		}
		var faltan []string
		for _, k := range orden {
			visto := false
			for _, fl := range grupos[k] {
				visto = visto || enSinopsis(fl)
			}
			if !visto {
				faltan = append(faltan, "-"+strings.Join(grupos[k], "/-"))
			}
		}
		if len(faltan) > 0 {
			sort.Strings(faltan)
			t.Errorf("%s: %s defines %s but its synopsis in tree.go does not show it", pos, fsName, strings.Join(faltan, " "))
		}
	})
	if comprobados < 40 {
		t.Fatalf("only %d flag sets checked: the parser lost track of how commands define their flags", comprobados)
	}
}

// TestSinopsisAlineada: en tree.go la descripción empieza en la columna 51;
// una que empieza en otra ("compose restart [service...]       restarts") se
// lee como parte de la sinopsis.
func TestSinopsisAlineada(t *testing.T) {
	const col = 51
	hueco := regexp.MustCompile(`\S( {2,})\S`)
	for _, s := range coreTree {
		for _, c := range s.cmds {
			for _, line := range strings.Split(c.Usage, "\n") {
				indent := len(line) - len(strings.TrimLeft(line, " "))
				if strings.TrimSpace(line) == "" || indent >= col {
					if indent > col {
						t.Errorf("%s: description past column %d: %q", c.Name, col, line)
					}
					continue
				}
				if m := hueco.FindStringSubmatchIndex(line); m != nil && m[3] != col {
					t.Errorf("%s: description at column %d, not %d: %q", c.Name, m[3], col, line)
				}
			}
		}
	}
}

// La lista de flags de cada comando, sacada de los FlagSets del código y
// comparada con testdata/contrato/flags.txt. Quitar un flag es incompatible
// (docs/compatibilidad.md) y falla aquí aunque se borre también de tree.go,
// que TestSinopsisListaCadaFlag no lo ve; uno nuevo pide -update:
//
//	go test ./cmd/kling -run TestContratoFlags -update
func TestContratoFlags(t *testing.T) {
	_, a, files := paqueteFlags(t)
	vistas := map[string]bool{}
	var lineas []string
	porCadaFlagSet(files, func(_ *ast.AssignStmt, fd *ast.FuncDecl, v, fsName string) {
		for _, p := range fsSinArbol {
			if strings.HasPrefix(fsName, p) {
				return
			}
		}
		for _, fl := range a.flagsDe(fd.Body, v, map[string]bool{}) {
			l := "kling " + fsName + ":  -" + fl.name
			if !vistas[l] {
				vistas[l] = true
				lineas = append(lineas, l)
			}
		}
	})
	if len(lineas) < 200 {
		t.Fatalf("only %d flags found: the parser lost track of how commands define their flags", len(lineas))
	}
	cab := []string{
		"Flags de cada comando del CLI: kling <FlagSet>:  -<flag>.",
		"Lo genera TestContratoFlags (cmd/kling/sinopsis_flags_test.go) leyendo",
		"los FlagSets de nombre literal del código; para regenerarlo:",
		"  go test ./cmd/kling -run TestContratoFlags -update",
		"Quitar o cambiar una línea exige subir api.APIVersion (internal/contrato).",
	}
	if err := contrato.Comparar("testdata/contrato/flags.txt", api.APIVersion, cab, lineas, *actualizar); err != nil {
		t.Fatal(err)
	}
}
