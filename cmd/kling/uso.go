package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// Errores de uso: comando, flag o argumentos que no valen.
//
// Salen con 2 (salidaUso) y su pista es la ayuda del comando, `kling help
// <cmd>`. Antes se reconocían por el prefijo "usage: " del texto, y un
// "unknown subcommand" o un flag mal escrito (que el paquete flag resolvía con
// os.Exit(2) y el Usage crudo, sin error: ni try:) se quedaban fuera.

// errorUso es un error de uso de cmd ("machine", "image snapshots"; vacío si
// no se sabe de qué comando).
type errorUso struct {
	cmd string
	err error
}

func (e *errorUso) Error() string { return e.err.Error() }
func (e *errorUso) Unwrap() error { return e.err }

// hint es la ayuda del comando, o la general si no se sabe cuál es.
func (e *errorUso) hint() string {
	if c := comandoDeAyuda(e.cmd); c != "" {
		return "kling help " + c
	}
	return "kling help"
}

// errUso es fmt.Errorf como error de uso. El comando sale del propio texto
// cuando empieza por "usage: kling <cmd> …".
func errUso(format string, a ...any) error {
	err := fmt.Errorf(format, a...)
	return &errorUso{cmd: comandoDeUsage(err.Error()), err: err}
}

// errUsoDe marca err como error de uso de cmd.
func errUsoDe(cmd string, err error) error {
	return &errorUso{cmd: cmd, err: err}
}

// esUso dice si err es un error de uso.
func esUso(err error) bool {
	var u *errorUso
	return errors.As(err, &u)
}

// comandoDeUsage saca "machine squeeze" de "usage: kling machine squeeze <ref>":
// las palabras tras "kling" hasta el primer flag o marcador.
func comandoDeUsage(msg string) string {
	resto, ok := strings.CutPrefix(msg, "usage: kling ")
	if !ok {
		return ""
	}
	var ws []string
	for _, w := range strings.Fields(resto) {
		if !esPalabraDeComando(w) {
			break
		}
		ws = append(ws, w)
	}
	return strings.Join(ws, " ")
}

func esPalabraDeComando(w string) bool {
	if w == "" || w[0] < 'a' || w[0] > 'z' {
		return false
	}
	for _, r := range w {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// ayudaRetirada son los FlagSet con el nombre de antes de un comando: la
// ayuda se pide por el de ahora.
var ayudaRetirada = map[string]string{
	"credential": "machine credential",
	"plugins":    "plugin",
}

// comandoDeAyuda es lo que va tras `kling help` para cmd: como mucho comando
// y subcomando (lo que cmdHelp entiende), con los nombres retirados ya
// traducidos. Vacío si cmd no es un comando del núcleo ni de una extensión.
func comandoDeAyuda(cmd string) string {
	ws := strings.Fields(cmd)
	if len(ws) == 0 {
		return ""
	}
	if to, ok := ayudaRetirada[ws[0]]; ok {
		ws = append(strings.Fields(to), ws[1:]...)
	} else if to, ok := removedAliases[ws[0]]; ok {
		// "squeeze" → "machine squeeze"; "info" → "status -v" se queda en status.
		ws = append(strings.Fields(strings.Split(to, " -")[0]), ws[1:]...)
	}
	if coreCommand(ws[0]) == nil && extensions().Lookup(ws[0]) == nil {
		return ""
	}
	if len(ws) > 2 {
		ws = ws[:2]
	}
	return strings.Join(ws, " ")
}

// errAyudaDada es el -h de un FlagSet: la lista de flags ya salió por stderr
// y se sale con 0, como hacía flag.ExitOnError.
var errAyudaDada = &errConCodigo{code: 0, err: errors.New("")}

// parseFlags es fs.Parse para el CLI: un flag que no existe o un valor que no
// vale es un error de uso (sale con 2, "error:" y "try: kling help <cmd>"), y
// no el Usage del paquete flag por stderr seguido de os.Exit(2). Con -h
// imprime la lista de flags (lo que lee plugin.FlagHelp) y devuelve
// errAyudaDada.
func parseFlags(fs *flag.FlagSet, args []string) error {
	fs.Usage = func() {}
	out := fs.Output()
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	fs.SetOutput(out)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, flag.ErrHelp):
		fs.PrintDefaults()
		return errAyudaDada
	}
	return errUsoDe(fs.Name(), err)
}

// sinArgumentos es el error de uso de un comando que no admite argumentos y
// recibió alguno: `kling ps foo` no debe listar como si nada.
func sinArgumentos(fs *flag.FlagSet) error {
	if fs.NArg() == 0 {
		return nil
	}
	return errUsoDe(fs.Name(), fmt.Errorf("kling %s takes no arguments (got %q)", fs.Name(), fs.Arg(0)))
}

// sinPosicionales es sinArgumentos para un comando que deja pasar flags que
// no son suyos (status, con los de las extensiones: knownFlags), cuyo fs no
// ve los argumentos. Lo que sigue a un flag ajeno sin "=" se toma por su
// valor: `kling status -gateway x` vale, `kling status x` no.
func sinPosicionales(fs *flag.FlagSet, args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return nil
		}
		if len(a) < 2 || a[0] != '-' {
			return errUsoDe(fs.Name(), fmt.Errorf("kling %s takes no arguments (got %q)", fs.Name(), a))
		}
		name, _, conValor := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if conValor {
			continue
		}
		if f := fs.Lookup(name); f != nil {
			if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				continue
			}
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
		}
	}
	return nil
}
