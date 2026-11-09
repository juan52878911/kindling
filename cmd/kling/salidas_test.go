package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Las marcas ✓/✗ pasan siempre por uiMarks: con NO_COLOR son ASCII. Una
// cadena con la marca escrita a mano en el código se salta esa regla.
func TestMarcasPasanPorUIMarks(t *testing.T) {
	fset := token.NewFileSet()
	ficheros, _ := filepath.Glob("*.go")
	for _, f := range ficheros {
		if strings.HasSuffix(f, "_test.go") || f == "ui.go" {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING &&
				(strings.Contains(lit.Value, "✓") || strings.Contains(lit.Value, "✗")) {
				t.Errorf("%s: marca escrita a mano en %s: usa uiMarks()", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
}

// stdout es para datos: los avisos de ps y su pie van a stderr, y con
// NO_COLOR no hay marcas Unicode.
func TestPSAvisosPorStderr(t *testing.T) {
	hermetic(t)
	sock := daemonRelleno(t)
	out, errOut, code := correrKling(t, []string{"KLING_HOST=unix://" + sock}, "ps", "-a")
	if code != 0 {
		t.Fatalf("ps: %d\n%s", code, errOut)
	}
	if strings.Contains(out, "\n! ") || strings.Contains(out, "machines' own disk") {
		t.Errorf("avisos en stdout:\n%s", out)
	}
	if !strings.Contains(errOut, "! m1: on hold") {
		t.Errorf("sin avisos en stderr:\n%s", errOut)
	}
	if !strings.HasPrefix(out, "ID ") {
		t.Errorf("stdout no es la tabla:\n%s", out)
	}
}

// `top -watch` solo borra la pantalla con ANSI en una terminal y sin NO_COLOR.
func TestBorradoDePantalla(t *testing.T) {
	t.Setenv("NO_COLOR", "x") // para que se restaure al acabar
	os.Unsetenv("NO_COLOR")
	if borradoDePantalla(true, true) != "\033[H\033[2J" {
		t.Error("terminal: sin borrado")
	}
	for _, c := range []struct{ tty, primera bool }{{false, true}, {false, false}} {
		if s := borradoDePantalla(c.tty, c.primera); strings.Contains(s, "\033") {
			t.Errorf("%+v: ANSI sin terminal: %q", c, s)
		}
	}
	if borradoDePantalla(false, false) != "\n" {
		t.Error("sin terminal, una línea en blanco entre fotos")
	}
	t.Setenv("NO_COLOR", "1")
	if s := borradoDePantalla(true, false); strings.Contains(s, "\033") {
		t.Errorf("NO_COLOR: %q", s)
	}
}

// La nota de `context ls` sobre KLING_HOST es un aviso: a stderr.
func TestContextLsNotaPorStderr(t *testing.T) {
	hermetic(t)
	if _, e, code := correrKling(t, nil, "context", "add", "lab", "ssh://u@h"); code != 0 {
		t.Fatalf("context add: %d %s", code, e)
	}
	out, errOut, _ := correrKling(t, nil, "context", "ls")
	if strings.Contains(out, "NOTE:") || !strings.Contains(errOut, "NOTE: $KLING_HOST") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", out, errOut)
	}
}
