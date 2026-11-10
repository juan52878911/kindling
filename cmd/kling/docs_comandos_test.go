package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Los `kling <sub>` que cita docs/ tienen que existir: un comando que se
// retiró o se renombró y sigue en la documentación manda al lector a un error
// de uso. Se comprueba contra el árbol del núcleo (coreTree, aliases), las
// extensiones de dentro (ai) y los manifiestos de ext/* (leídos del fuente:
// son módulos aparte y no se pueden importar).

// docsHistoricos cuentan cómo era algo en una versión pasada, con los nombres
// de entonces: notas de release, evaluaciones e informes fechados.
var docsHistoricos = map[string]bool{
	"RELEASE-v0.2.0.md":                true,
	"RELEASE-v0.3.0.md":                true,
	"CHISPA-EVAL.md":                   true,
	"CI-TRIAGE-EVAL.md":                true,
	"DOMOTICA-EVAL.md":                 true,
	"auditoria-estabilidad-2026-08.md": true,
	"hallazgos.md":                     true,
	"vz-mac-prototipo.md":              true,
}

// nodoCmd es un comando con los subcomandos que admite. subs nil = no se
// comprueba la palabra siguiente (toma argumentos libres).
type nodoCmd struct {
	subs map[string]*nodoCmd
}

func (n *nodoCmd) hijo(nombre string) *nodoCmd {
	if n.subs == nil {
		n.subs = map[string]*nodoCmd{}
	}
	if c, ok := n.subs[nombre]; ok {
		return c
	}
	c := &nodoCmd{}
	n.subs[nombre] = c
	return c
}

// arbolConocido arma el árbol de todo lo que `kling` entiende.
func arbolConocido(t *testing.T) *nodoCmd {
	t.Helper()
	raiz := &nodoCmd{}
	for _, s := range coreTree {
		for _, c := range s.cmds {
			n := raiz.hijo(c.Name)
			for _, sub := range c.Subcommands {
				n.hijo(sub)
			}
		}
	}
	// help acepta cualquier comando detrás.
	raiz.subs["help"].subs = nil
	for alias, a := range aliases {
		raiz.subs[alias] = raiz.subs[strings.Fields(a)[0]]
	}
	for _, b := range builtinExtensions() {
		ns := raiz.hijo(b.Manifest.Name)
		for _, c := range b.Manifest.Commands {
			n := ns.hijo(c.Name)
			for _, sub := range c.Subcommands {
				n.hijo(sub)
			}
		}
	}
	dirs, err := filepath.Glob("../../ext/*/cmd/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no encuentro las extensiones en ext/*/cmd: %v", err)
	}
	var vistas []string
	for _, d := range dirs {
		if ns := extensionDelFuente(t, raiz, d); ns != "" {
			vistas = append(vistas, ns)
		}
	}
	sort.Strings(vistas)
	// Si cambia la forma de declarar el manifiesto, que se note aquí y no
	// como cientos de comandos «inexistentes».
	if got := strings.Join(vistas, " "); got != "db mcp phone sandbox" {
		t.Fatalf("extensiones leídas de ext/: %q, quería \"db mcp phone sandbox\"", got)
	}
	return raiz
}

// extensionDelFuente lee de los .go de dir el plugin.Manifest (su Name) y
// los []plugin.Command, y los cuelga del árbol: bajo el nombre de la
// extensión, o en primer nivel si son TopLevel. Devuelve el nombre de la
// extensión, o "" si dir no declara ninguna (herramientas como kling-dbbench).
func extensionDelFuente(t *testing.T, raiz *nodoCmd, dir string) string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	cadena := func(e ast.Expr) string {
		if l, ok := e.(*ast.BasicLit); ok && l.Kind == token.STRING {
			s, _ := strconv.Unquote(l.Value)
			return s
		}
		return ""
	}
	esPlugin := func(e ast.Expr, nombre string) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		x, ok := s.X.(*ast.Ident)
		return ok && x.Name == "plugin" && s.Sel.Name == nombre
	}
	type cmdFuente struct {
		name     string
		subs     []string
		topLevel bool
	}
	var ns string
	var cmds []cmdFuente
	for _, p := range pkgs {
		for _, f := range p.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				cl, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if esPlugin(cl.Type, "Manifest") {
					for _, e := range cl.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok && kv.Key.(*ast.Ident).Name == "Name" {
							ns = cadena(kv.Value)
						}
					}
					return true
				}
				at, ok := cl.Type.(*ast.ArrayType)
				if !ok || !esPlugin(at.Elt, "Command") {
					return true
				}
				for _, e := range cl.Elts {
					el, ok := e.(*ast.CompositeLit)
					if !ok {
						continue
					}
					var c cmdFuente
					for _, kv := range el.Elts {
						kv, ok := kv.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						switch kv.Key.(*ast.Ident).Name {
						case "Name":
							c.name = cadena(kv.Value)
						case "TopLevel":
							id, ok := kv.Value.(*ast.Ident)
							c.topLevel = ok && id.Name == "true"
						case "Subcommands":
							if sl, ok := kv.Value.(*ast.CompositeLit); ok {
								for _, s := range sl.Elts {
									c.subs = append(c.subs, cadena(s))
								}
							}
						}
					}
					if c.name != "" {
						cmds = append(cmds, c)
					}
				}
				return true
			})
		}
	}
	if ns == "" || len(cmds) == 0 {
		return ""
	}
	for _, c := range cmds {
		padre := raiz.hijo(ns)
		if c.topLevel {
			padre = raiz
		}
		n := padre.hijo(c.name)
		for _, s := range c.subs {
			n.hijo(s)
		}
	}
	return ns
}

// ejemplosDeDocs son comandos que la documentación inventa para enseñar algo:
// la extensión de ejemplo de docs/extensions.md.
var ejemplosDeDocs = map[string]bool{"hello": true}

var (
	reKling    = regexp.MustCompile(`\bkling[ \t]+`)
	rePalabra  = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	reInline   = regexp.MustCompile("`[^`\n]+`")
	reAsignar  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S*$`)
	reRetirado = regexp.MustCompile(`(?i)retir|removed`)
)

// llamadasEn devuelve lo que sigue a cada «kling» de frag que está en
// posición de comando: al principio o tras un prompt, sudo, env, VAR=x o una
// ruta (~/.local/bin/kling). En un bloque de código vale también tras una
// tubería, ;, && o (. «systemctl start kling» o un mensaje de error que nombra
// kling no son llamadas.
func llamadasEn(frag string, bloque bool) []string {
	var out []string
	for _, m := range reKling.FindAllStringIndex(frag, -1) {
		if strings.HasSuffix(frag[:m[0]], "-") || strings.HasSuffix(frag[:m[0]], ".") {
			continue // dev.kling, x-kling: \b los deja pasar
		}
		antes := strings.TrimRight(frag[:m[0]], " \t")
		ultimo := ""
		if f := strings.Fields(antes); len(f) > 0 {
			ultimo = f[len(f)-1]
		}
		suelto := antes != frag[:m[0]] // un espacio delante: no es <kling…>
		ok := antes == "" || strings.HasSuffix(antes, "/") ||
			suelto && (ultimo == "$" || ultimo == "sudo" || ultimo == "env" || reAsignar.MatchString(ultimo))
		if bloque && !ok {
			for _, s := range []string{"|", ";", "&&", "(", "!"} {
				ok = ok || strings.HasSuffix(antes, s)
			}
		}
		if ok {
			out = append(out, frag[m[1]:])
		}
	}
	return out
}

// comandosCitados devuelve, por línea de md, las llamadas a kling de los
// bloques ``` (sin sus comentarios) y de lo que va entre comillas invertidas.
// La prosa no cuenta: «kling arranca…» no es un comando.
func comandosCitados(md string) map[int][]string {
	out := map[int][]string{}
	enBloque := false
	for i, linea := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(linea), "```") {
			enBloque = !enBloque
			continue
		}
		if enBloque {
			if strings.HasPrefix(strings.TrimSpace(linea), "#") {
				continue
			}
			if j := strings.Index(linea, " #"); j >= 0 {
				linea = linea[:j]
			}
			out[i+1] = append(out[i+1], llamadasEn(linea, true)...)
			continue
		}
		for _, tr := range reInline.FindAllString(linea, -1) {
			out[i+1] = append(out[i+1], llamadasEn(strings.Trim(tr, "`"), false)...)
		}
	}
	return out
}

// comprobarLlamada recorre las palabras de args por el árbol y devuelve la
// ruta que no existe ("" si existe). Se para en el primer argumento: una
// flag, un <marcador>, o cualquier cosa tras un comando sin subcomandos.
func comprobarLlamada(raiz *nodoCmd, args string) string {
	n := raiz
	var ruta []string
	for _, w := range strings.Fields(args) {
		if len(ruta) == 0 && strings.HasPrefix(w, "-") {
			continue // kling -h, kling --version
		}
		if n.subs == nil || !rePalabra.MatchString(w) {
			return ""
		}
		ruta = append(ruta, w)
		c, ok := n.subs[w]
		if !ok {
			return strings.Join(ruta, " ")
		}
		n = c
	}
	return ""
}

// inexistentes son las llamadas de md que no existen, como "fichero:línea: ruta".
func inexistentes(raiz *nodoCmd, nombre, md string) []string {
	lineas := strings.Split(md, "\n")
	citas := comandosCitados(md)
	orden := make([]int, 0, len(citas))
	for l := range citas {
		orden = append(orden, l)
	}
	sort.Ints(orden)
	var out []string
	for _, l := range orden {
		for _, args := range citas[l] {
			// Los alias que se quedan (commit, snapshots, plugins) funcionan,
			// pero la documentación enseña el nombre de ahora.
			if f := strings.Fields(args); len(f) > 0 {
				if a, ok := aliases[f[0]]; ok && !strings.Contains(lineas[l-1], "alias") {
					out = append(out, nombre+":"+strconv.Itoa(l)+": kling "+f[0]+" (alias de kling "+a+")")
					continue
				}
			}
			mal := comprobarLlamada(raiz, args)
			if mal == "" || ejemplosDeDocs[mal] {
				continue
			}
			// Un nombre retirado en 1.0 se puede citar para decir que se retiró.
			_, r1 := removedAliases[mal]
			_, r2 := removedExtAliases[mal]
			if (r1 || r2) && reRetirado.MatchString(lineas[l-1]) {
				continue
			}
			out = append(out, nombre+":"+strconv.Itoa(l)+": kling "+mal)
		}
	}
	return out
}

func TestDocsCitanComandosQueExisten(t *testing.T) {
	raiz := arbolConocido(t)
	ficheros, err := filepath.Glob("../../docs/*.md")
	if err != nil || len(ficheros) == 0 {
		t.Fatalf("no encuentro docs/*.md: %v", err)
	}
	readmes, _ := filepath.Glob("../../ext/*/README*.md")
	ficheros = append(append(ficheros, readmes...), "../../README.md", "../../README.es.md", "../../SECURITY.md")
	for _, f := range ficheros {
		if docsHistoricos[filepath.Base(f)] {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range inexistentes(raiz, strings.TrimPrefix(f, "../../"), string(b)) {
			t.Errorf("%s: ese comando no existe (kling help all, o los manifiestos de ext/)", m)
		}
	}
}

// Un comando inventado en un bloque de código o entre comillas tiene que
// hacer fallar el test; la prosa, systemctl y los mensajes de error, no.
func TestDocsComandosDetectaInexistentes(t *testing.T) {
	raiz := arbolConocido(t)
	casos := []struct{ md, malo string }{
		{"```\nkling squeeze web\n```", "squeeze"},
		{"usa `kling machine credaudit m1`", "machine credaudit"},
		{"```\n$ sudo KLING_HOST=x kling sbx nope\n```", "sbx nope"},
		{"```\nkling ps | kling frobnicate\n```", "frobnicate"},
		{"```\nkling db brnch ls\n```", "db brnch"},
		{"`kling squeeze` se retiró en 1.0", ""},
		{"```\nkling machine squeeze web\n```", ""},
		{"```\nkling db branch ls\n```", ""},
		{"```\nkling run alpine -- echo hola\n```", ""},
		{"```\nkling commit web\n```", "commit (alias de kling save)"},
		{"`kling plugins` es un alias de `kling plugin`", ""},
		{"```\nkling connect claude\n```", ""},
		{"```\nkling image refresh -agent web\n```", ""},
		{"```\nsudo systemctl start kling kling-gateway\n```", ""},
		{"`ELF: amd64; kling runs images of the host`", ""},
		{"```\nOLD_DIR=<kling y kling-vz> x.sh\n```", ""},
		{"kling squeeze en prosa no cuenta", ""},
		{"```\n# kling squeeze, en un comentario\n```", ""},
	}
	for _, c := range casos {
		var malos []string
		for _, m := range inexistentes(raiz, "x", c.md) {
			malos = append(malos, strings.TrimPrefix(m[strings.Index(m, ": ")+2:], "kling "))
		}
		if got := strings.Join(malos, ","); got != c.malo {
			t.Errorf("%q: inexistentes %q, quería %q", c.md, got, c.malo)
		}
	}
}

// titulosDe son los títulos de md fuera de los bloques de código.
func titulosDe(md string) []string {
	var out []string
	enBloque := false
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			enBloque = !enBloque
			continue
		}
		if !enBloque && strings.HasPrefix(l, "#") {
			if i := strings.Index(l, " "); i > 0 && strings.Trim(l[:i], "#") == "" {
				out = append(out, l)
			}
		}
	}
	return out
}

func nivel(titulo string) int { return len(titulo) - len(strings.TrimLeft(titulo, "#")) }

// rutaDe es el comando de args hasta donde el árbol lo conoce ("image ls").
func rutaDe(raiz *nodoCmd, args string) string {
	n := raiz
	var ruta []string
	for _, w := range strings.Fields(args) {
		c, ok := n.subs[w]
		if !ok {
			break
		}
		ruta = append(ruta, w)
		n = c
	}
	return strings.Join(ruta, " ")
}

// guide.md y guia.md son la misma guía en dos idiomas: mismos títulos en el
// mismo orden y nivel, y los mismos comandos citados. Una sección que se añade
// a una y no a la otra hace fallar esto.
func TestGuiaYGuideSeCorresponden(t *testing.T) {
	leer := func(f string) string {
		b, err := os.ReadFile("../../docs/" + f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	en, es := leer("guide.md"), leer("guia.md")
	te, ts := titulosDe(en), titulosDe(es)
	for i := 0; i < len(te) || i < len(ts); i++ {
		var a, b string
		if i < len(te) {
			a = te[i]
		}
		if i < len(ts) {
			b = ts[i]
		}
		if nivel(a) != nivel(b) {
			t.Fatalf("título %d: guide.md %q, guia.md %q: las dos guías tienen que llevar las mismas secciones en el mismo orden", i+1, a, b)
		}
	}
	raiz := arbolConocido(t)
	rutas := func(md string) map[string]bool {
		out := map[string]bool{}
		for _, lista := range comandosCitados(md) {
			for _, args := range lista {
				if r := rutaDe(raiz, args); r != "" {
					out[r] = true
				}
			}
		}
		return out
	}
	re, rs := rutas(en), rutas(es)
	for r := range re {
		if !rs[r] {
			t.Errorf("guide.md cita «kling %s» y guia.md no", r)
		}
	}
	for r := range rs {
		if !re[r] {
			t.Errorf("guia.md cita «kling %s» y guide.md no", r)
		}
	}
}
