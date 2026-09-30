package main

// KLING_DB_TRACE=1: tiempos por fase a stderr, para perfilar (kling db branch
// sobre todo). Cada línea es el momento de inicio desde que arrancó el
// proceso, lo que tardó y qué fue:
//
//	trace    12.3ms     4.1ms  git rev-parse
//
// Solo nombres de fase y de subcomando: nunca argumentos, que podrían llevar
// nombres de rama o de máquina a un log.

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const traceEnv = "KLING_DB_TRACE"

// procStart es lo más cerca del arranque del proceso que se puede medir desde Go.
var procStart = time.Now()

type tracer struct {
	mu sync.Mutex
	w  io.Writer
}

// newTracer devuelve nil (sin trazas) salvo que KLING_DB_TRACE esté puesto.
func newTracer(w io.Writer) *tracer {
	if os.Getenv(traceEnv) == "" {
		return nil
	}
	return &tracer{w: w}
}

// span empieza una fase; la función que devuelve la cierra y la escribe. Un
// tracer nil no hace nada.
func (t *tracer) span(name string) func() {
	if t == nil {
		return func() {}
	}
	start := time.Now()
	return func() {
		d := time.Since(start)
		t.mu.Lock()
		defer t.mu.Unlock()
		fmt.Fprintf(t.w, "trace %9.1fms %9.1fms  %s\n", ms(start.Sub(procStart)), ms(d), name)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// traceName es el nombre de una llamada a kling para la traza: el subcomando (y el
// siguiente para los que tienen dos niveles) y, en exec, la orden de dentro.
func traceName(args []string) string {
	if len(args) == 0 {
		return "kling"
	}
	name := "kling " + args[0]
	switch args[0] {
	case "sandbox", "template", "machine":
		if len(args) > 1 {
			name += " " + args[1]
		}
	case "exec":
		for i, a := range args {
			if a == "--" && i+1 < len(args) {
				name += " " + args[i+1]
				if args[i+1] == "su" && len(args) > i+5 {
					name += " …" + shortCmd(args[len(args)-1])
				}
				break
			}
		}
	}
	return name
}

// shortCmd es la primera palabra de una orden de sh -c (pg_isready, psql...).
func shortCmd(s string) string {
	for i, r := range s {
		if r == ' ' {
			return s[:i]
		}
	}
	return s
}

// tracedBackend apunta cada llamada al daemon.
type tracedBackend struct {
	backend
	t *tracer
}

func (b tracedBackend) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	defer b.t.span(traceName(args))()
	return b.backend.Run(ctx, stdin, args...)
}

func (b tracedBackend) List(ctx context.Context) ([]*api.Machine, error) {
	defer b.t.span("api list")()
	return b.backend.List(ctx)
}

func (b tracedBackend) Thaw(ctx context.Context, ref string) (*api.Machine, error) {
	end := b.t.span("api thaw")
	mc, err := b.backend.Thaw(ctx, ref)
	end()
	if err == nil && mc != nil && mc.Wake != nil {
		// Las fases del despertar según el daemon (kling inspect las enseña igual).
		w := mc.Wake
		b.t.mu.Lock()
		fmt.Fprintf(b.t.w, "trace   wake %s: net %.1f spawn %.1f socket %.1f load %.1f forwards %.1f resync %.1f total %.1f ms\n",
			w.Tier, w.NetMS, w.SpawnMS, w.SocketMS, w.LoadMS, w.ForwardsMS, w.ResyncMS, w.TotalMS)
		b.t.mu.Unlock()
	}
	return mc, err
}

func (b tracedBackend) SetLabels(ctx context.Context, ref string, labels map[string]string) error {
	defer b.t.span("api setlabels")()
	return b.backend.SetLabels(ctx, ref, labels)
}

func (b tracedBackend) SetCredential(ctx context.Context, ref string, spec api.CredentialSpec) error {
	defer b.t.span("api setcredential")()
	return b.backend.SetCredential(ctx, ref, spec)
}
