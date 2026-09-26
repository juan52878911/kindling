package main

import (
	"flag"
	"fmt"
	"github.com/juan52878911/kindling/pkg/api"
	"os"
	"strings"
)

// resolveCPUPct devuelve el techo de CPU eligiendo entre el flag nuevo -cpu-pct y
// el alias deprecado -cpu. -cpu se mantiene por compatibilidad pero avisa: colisiona
// visualmente con -cpus (número de vCPUs), a un solo carácter y en el mismo comando.
func resolveCPUPct(fs *flag.FlagSet, pct, old int) int {
	usedOld := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "cpu" {
			usedOld = true
		}
	})
	if usedOld {
		fmt.Fprintln(os.Stderr, "warning: -cpu is deprecated; use -cpu-pct (it looks like -cpus, which sets vCPU count)")
		if pct == 0 {
			return old
		}
	}
	return pct
}

// labelFlag acumula -label k=v repetidos.
type labelFlag map[string]string

func (l *labelFlag) String() string { return "" }

func (l *labelFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("invalid label %q: use key=value", v)
	}
	if *l == nil {
		*l = labelFlag{}
	}
	(*l)[k] = val
	return nil
}

// merge añade -service como la etiqueta convencional "service".
func (l labelFlag) merge(service string) map[string]string {
	if service == "" && len(l) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range l {
		out[k] = v
	}
	if service != "" {
		out[api.LabelService] = service
	}
	return out
}

// knownFlags deja en args solo los flags que fs conoce (con su valor), para
// poder parsear lo propio y pasar el resto tal cual a una extensión. Un flag
// desconocido no es un error aquí: puede ser de otra.
func knownFlags(fs *flag.FlagSet, args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			continue
		}
		name := strings.TrimLeft(a, "-")
		hasValue := false
		if k, _, ok := strings.Cut(name, "="); ok {
			name, hasValue = k, true
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		out = append(out, a)
		if hasValue {
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}
