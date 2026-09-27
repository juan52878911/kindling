package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Lo que la CLI hace igual en todos los comandos: el "next:" de lo que crea
// algo, la confirmación de lo que destruye varias cosas, y las marcas ✓/✗ que
// se vuelven ASCII con NO_COLOR.

// next imprime el siguiente paso, como `doctor` imprime `fix:` y los errores
// `try:`. Es lo que hace que la CLI se aprenda sola. Va a stderr: stdout es
// de los datos y quien hace `kling run ... | cut` no quiere la pista dentro.
func next(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "next: "+format+"\n", a...)
}

// errAborted es lo que devuelve un comando destructivo cuando quien lo teclea
// dice que no. Sale con 1 y sin pista: la decisión fue suya.
var errAborted = errors.New("aborted")

// confirmMany pregunta antes de borrar varias cosas de golpe. Solo pregunta
// en una terminal: en un script (sin TTY) sigue adelante, que es lo que un
// script espera; -f la salta también en la terminal. Una sola cosa no se
// pregunta: es lo que se tecleó.
func confirmMany(what string, names []string) bool {
	if len(names) < 2 {
		return true
	}
	return confirm(fmt.Sprintf("remove %d %ss (%s)?", len(names), what, strings.Join(names, " ")))
}

// confirm pregunta sí/no en la terminal; sin terminal devuelve true.
func confirm(prompt string) bool {
	if !isTerminal(os.Stdin.Fd()) || !isTerminal(os.Stderr.Fd()) {
		return true
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// marks son las marcas de estado de doctor y status. Con NO_COLOR
// (https://no-color.org) se vuelven ASCII: quien lo pone no quiere adornos,
// y ✓ es uno.
type marks struct{ ok, warn, fail string }

func uiMarks() marks {
	if _, plain := os.LookupEnv("NO_COLOR"); plain {
		return marks{"ok", "!!", "xx"}
	}
	return marks{"✓", "!", "✗"}
}
