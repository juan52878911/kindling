package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// kling logs [-f] <ref>.
//
// El daemon no tiene un flujo de la consola: solo sirve las últimas N líneas.
// Seguirla es sondear esa ventana y escribir lo que no se había escrito. Se
// hace así, y no con un endpoint nuevo, para que -f funcione contra cualquier
// daemon ya desplegado: un CLI nuevo con un daemon viejo es lo normal durante
// una actualización.

// followEvery es cada cuánto se pregunta; followWindow, cuántas líneas se
// piden cada vez. Con 2000 líneas cada medio segundo solo se pierde algo si la
// consola escupe más de 4000 líneas por segundo, y entonces nadie la lee.
const (
	followEvery  = 500 * time.Millisecond
	followWindow = 2000
)

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	host := hostFlag(fs)
	tail := fs.Int("tail", 200, "last N lines (0 = all)")
	follow := fs.Bool("f", false, "keep printing new lines until Ctrl-C or until the machine stops running")
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: kling logs [-f] <ref> [-tail N]")
	}
	ref := fs.Arg(0)

	ctx, stop := ctxWithSignals()
	defer stop()

	c := api.NewClient(hostOf(*host))
	out, err := c.Logs(ctx, ref, *tail)
	if err != nil {
		return err
	}
	fmt.Print(out)
	if !*follow {
		return nil
	}
	if out != "" && !strings.HasSuffix(out, "\n") {
		fmt.Println()
	}
	err = followLogs(ctx, os.Stdout, logSource{c: c, ref: ref}, splitLines(out), followEvery)
	if ctx.Err() != nil {
		return nil // Ctrl-C es la forma normal de terminar
	}
	return err
}

// logSource es lo que followLogs necesita del daemon; una interfaz para poder
// probarlo sin uno.
type logFetcher interface {
	fetch(ctx context.Context) (string, error)
	running(ctx context.Context) (bool, string, error)
}

type logSource struct {
	c   *api.Client
	ref string
}

func (s logSource) fetch(ctx context.Context) (string, error) {
	return s.c.Logs(ctx, s.ref, followWindow)
}

func (s logSource) running(ctx context.Context) (bool, string, error) {
	mc, err := s.c.Get(ctx, s.ref)
	if err != nil {
		return false, "", err
	}
	// created cuenta como viva: es la máquina que está arrancando, justo la
	// que uno quiere ver con -f.
	return mc.State == api.StateRunning || mc.State == api.StateCreated, string(mc.State), nil
}

// followLogs sondea hasta que se cancele ctx o la máquina deje de correr, y
// escribe solo las líneas nuevas. printed son las líneas ya escritas.
func followLogs(ctx context.Context, w io.Writer, src logFetcher, printed []string, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		// El estado se mira ANTES de traer las líneas: así las últimas que
		// escribió una máquina que acaba de pararse salen en esta vuelta.
		alive, state, err := src.running(ctx)
		if err != nil {
			return err
		}
		out, err := src.fetch(ctx)
		if err != nil {
			return err
		}
		cur := splitLines(out)
		for _, l := range newLines(printed, cur) {
			fmt.Fprintln(w, l)
		}
		printed = cur
		if !alive {
			fmt.Fprintf(os.Stderr, "(machine is %s; stopped following)\n", state)
			return nil
		}
	}
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// logAnchor es cuántas de las últimas líneas escritas se buscan en la ventana
// nueva para saber dónde se quedó uno.
const logAnchor = 50

// newLines devuelve las líneas de cur que no estaban en printed.
//
// Las dos son ventanas del final de la misma consola. Se buscan las últimas
// líneas escritas dentro de cur, desde el final, y lo nuevo es lo que viene
// detrás. Si la última línea escrita estaba a medias (la consola aún no había
// escrito su salto de línea), no aparecerá igual: se reintenta sin ella y esa
// línea se vuelve a escribir entera, que es mejor que perderla. Con ventanas
// pequeñas, lo más viejo del ancla puede haber salido ya de cur: se prueba con
// anclas cada vez más cortas. Si no se encuentra nada, la consola avanzó más
// que una ventana entera y se escribe toda.
func newLines(printed, cur []string) []string {
	if len(printed) == 0 {
		return cur
	}
	tail := func(drop, k int) []string {
		a := printed[:len(printed)-drop]
		if k > len(a) {
			k = len(a)
		}
		return a[len(a)-k:]
	}
	try := func(anchor []string) ([]string, bool) {
		if len(anchor) == 0 {
			return nil, false
		}
		if j := lastIndexSeq(cur, anchor); j >= 0 {
			return cur[j+len(anchor):], true
		}
		return nil, false
	}
	for _, drop := range []int{0, 1} {
		if drop < len(printed) {
			if out, ok := try(tail(drop, logAnchor)); ok {
				return out
			}
		}
	}
	for k := min(len(printed), logAnchor) - 1; k > 0; k-- {
		if out, ok := try(tail(0, k)); ok {
			return out
		}
	}
	return cur
}

// lastIndexSeq es el último índice de cur donde empieza seq, o -1.
func lastIndexSeq(cur, seq []string) int {
	for i := len(cur) - len(seq); i >= 0; i-- {
		ok := true
		for k := range seq {
			if cur[i+k] != seq[k] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}
