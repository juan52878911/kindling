package main

import (
	"flag"
	"reflect"
	"testing"
)

// reorderFor es lo que hace que `kling logs mivm -tail 50` respete el -tail en vez
// de descartarlo en silencio. Se prueba con un flagset representativo (flags con
// valor, booleanos, y `--`).
func TestReorderFor(t *testing.T) {
	mk := func() *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.Int("tail", 0, "")
		fs.String("f", "", "")
		fs.String("image", "", "")
		fs.Bool("a", false, "")
		fs.Bool("json", false, "")
		return fs
	}
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"flag con valor tras posicional", []string{"mivm", "-tail", "50"}, []string{"-tail", "50", "mivm"}},
		{"string flag tras posicional", []string{"ref", "-f", "store.json"}, []string{"-f", "store.json", "ref"}},
		{"bool flag NO se lleva el siguiente", []string{"m1", "-a", "m2"}, []string{"-a", "m1", "m2"}},
		{"bool flag solo", []string{"-a"}, []string{"-a"}},
		{"bool flag antes de un posicional", []string{"-json", "svc"}, []string{"-json", "svc"}},
		{"-- detiene el reorden", []string{"-image", "X", "--", "npm", "install"}, []string{"-image", "X", "--", "npm", "install"}},
		{"posicional antes de -- se conserva", []string{"name", "-image", "X", "--", "cmd", "arg"}, []string{"-image", "X", "name", "--", "cmd", "arg"}},
		{"ya ordenado no cambia", []string{"-tail", "50", "mivm"}, []string{"-tail", "50", "mivm"}},
		{"flag=valor no consume el siguiente", []string{"mivm", "-tail=50"}, []string{"-tail=50", "mivm"}},
		{"flag desconocido se trata como con valor", []string{"m", "-zzz", "x"}, []string{"-zzz", "x", "m"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := reorderFor(mk(), c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("reorderFor(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// resolveCPUPct: -cpu-pct manda; -cpu es alias deprecado; sin ninguno, 0.
func TestResolveCPUPct(t *testing.T) {
	mk := func(args []string) *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.Int("cpu-pct", 0, "")
		fs.Int("cpu", 0, "")
		_ = fs.Parse(args)
		return fs
	}
	if got := resolveCPUPct(mk([]string{"-cpu-pct", "100"}), 100, 0); got != 100 {
		t.Errorf("-cpu-pct 100 -> %d, want 100", got)
	}
	if got := resolveCPUPct(mk([]string{"-cpu", "80"}), 0, 80); got != 80 {
		t.Errorf("-cpu 80 (alias) -> %d, want 80", got)
	}
	if got := resolveCPUPct(mk(nil), 0, 0); got != 0 {
		t.Errorf("ninguno -> %d, want 0", got)
	}
	if got := resolveCPUPct(mk([]string{"-cpu-pct", "100", "-cpu", "50"}), 100, 50); got != 100 {
		t.Errorf("ambos: -cpu-pct debe ganar -> %d, want 100", got)
	}
}
