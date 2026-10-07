package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Ningún mensaje del código recomienda un nombre de antes que avisa: quien
// siguiera la pista ("redeploy with kling chispa deploy") se llevaría el
// "warning: kling chispa is now kling ai chispa". Mira solo literales de
// cadena de los .go que no son tests, en todo el repositorio (extensiones
// incluidas); los comentarios pueden contar la historia.
func TestMensajesSinAliasQueAvisan(t *testing.T) {
	var viejos []string
	for a := range aliases {
		if !permanentAliases[a] {
			viejos = append(viejos, regexp.QuoteMeta(a))
		}
	}
	for a := range extAliases {
		viejos = append(viejos, regexp.QuoteMeta(a))
	}
	sort.Strings(viejos)
	re := regexp.MustCompile(`\bkling (` + strings.Join(viejos, "|") + `)\b`)
	// El propio aviso y la ayuda de los alias nombran los de antes a propósito.
	permitido := regexp.MustCompile(`^warning: kling %s is now`)

	raiz, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var malos []string
	err = filepath.WalkDir(raiz, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "testdata" || n == "node_modules" || (strings.HasPrefix(n, ".") && p != raiz) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || permitido.MatchString(s) {
				return true
			}
			if m := re.FindString(s); m != "" {
				rel, _ := filepath.Rel(raiz, p)
				malos = append(malos, rel+":"+strconv.Itoa(fset.Position(lit.Pos()).Line)+": "+m)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(malos) > 0 {
		t.Errorf("mensajes con nombres de antes que avisan:\n%s", strings.Join(malos, "\n"))
	}
}
