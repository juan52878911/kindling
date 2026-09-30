// Package lazyre compila una expresión regular la primera vez que se usa y no
// al arrancar. Una regexp a nivel de paquete con regexp.MustCompile se paga en
// init() aunque el comando no la toque: kling reenvía "kling db ..." a la
// extensión sin validar nada, y aun así compilaba ~55 expresiones (~1 ms en el
// lab). Con lazyre.New la declaración queda igual de visible y los usos no
// cambian: los métodos son los de *regexp.Regexp que el código usa.
package lazyre

import (
	"regexp"
	"sync"
)

// Regexp es una *regexp.Regexp que se compila en el primer uso. Es segura
// para usarla desde varias goroutines, como la original.
type Regexp struct {
	expr string
	get  func() *regexp.Regexp
}

// todas guarda cada expresión declarada para que CompileAll las pruebe: un
// error de sintaxis ya no salta al arrancar, así que lo caza un test.
var (
	mu    sync.Mutex
	todas []*Regexp
)

// New declara la expresión sin compilarla. Como regexp.MustCompile, entra en
// pánico si no compila, pero en el primer uso.
func New(expr string) *Regexp {
	r := &Regexp{expr: expr, get: sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(expr)
	})}
	mu.Lock()
	todas = append(todas, r)
	mu.Unlock()
	return r
}

// CompileAll compila todas las expresiones declaradas en el binario y
// devuelve la primera que falle. Para tests.
func CompileAll() error {
	mu.Lock()
	defer mu.Unlock()
	for _, r := range todas {
		if _, err := regexp.Compile(r.expr); err != nil {
			return err
		}
	}
	return nil
}

// Regexp devuelve la expresión compilada.
func (r *Regexp) Regexp() *regexp.Regexp { return r.get() }

// String devuelve el texto de la expresión sin compilarla.
func (r *Regexp) String() string { return r.expr }

func (r *Regexp) MatchString(s string) bool { return r.get().MatchString(s) }

func (r *Regexp) FindStringSubmatch(s string) []string { return r.get().FindStringSubmatch(s) }

func (r *Regexp) ReplaceAllString(src, repl string) string {
	return r.get().ReplaceAllString(src, repl)
}

func (r *Regexp) ReplaceAllStringFunc(src string, repl func(string) string) string {
	return r.get().ReplaceAllStringFunc(src, repl)
}
