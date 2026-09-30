package main

import (
	"flag"
	"reflect"
	"testing"

	"github.com/juan52878911/kindling/pkg/config"
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
		{"flag desconocido no se lleva otro flag", []string{"m", "-zzz", "-a"}, []string{"-zzz", "-a", "m"}},
		{"valor '-' (stdin) se queda con su flag", []string{"ref", "-f", "-"}, []string{"-f", "-", "ref"}},
		{"valor que empieza por '-' se queda con su flag", []string{"ref", "-f", "-x", "-a"}, []string{"-f", "-x", "-a", "ref"}},
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

// `kling image build <name> -builder b -spec -` (el orden de la ayuda) leía
// `-spec alpaca-cli` y fallaba con "open alpaca-cli". El nombre puede ir
// delante o detrás de los flags, con -spec a fichero o a stdin.
func TestReorderForImageBuild(t *testing.T) {
	mk := func() (*flag.FlagSet, *string, *string, *string) {
		fs := flag.NewFlagSet("image build", flag.ContinueOnError)
		hostFlag(fs)
		builder := fs.String("builder", "", "")
		spec := fs.String("spec", "", "")
		base := fs.String("base", "", "")
		fs.Int("grow", 0, "")
		return fs, builder, spec, base
	}
	for _, specArg := range []string{"-", "/tmp/x.json"} {
		flags := []string{"-builder", "base", "-base", "min", "-spec", specArg}
		for name, args := range map[string][]string{
			"nombre delante": append([]string{"alpaca-cli"}, flags...),
			"nombre detrás":  append(append([]string{}, flags...), "alpaca-cli"),
		} {
			t.Run(name+" spec="+specArg, func(t *testing.T) {
				fs, builder, spec, base := mk()
				if err := fs.Parse(reorderFor(fs, args)); err != nil {
					t.Fatal(err)
				}
				if fs.NArg() != 1 || fs.Arg(0) != "alpaca-cli" {
					t.Errorf("posicionales = %v, want [alpaca-cli]", fs.Args())
				}
				if *builder != "base" || *base != "min" || *spec != specArg {
					t.Errorf("builder=%q base=%q spec=%q, want base min %q", *builder, *base, *spec, specArg)
				}
			})
		}
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

// egressForRun: con -from y sin -egress explícito, se manda vacío para que
// runFrom() herede la política de la plantilla (A2). Sin -from, o con -egress
// dado, se mantiene el defecto de siempre.
func TestEgressForRun(t *testing.T) {
	mk := func(args []string) *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.String("from", "", "")
		fs.String("egress", "", "")
		fs.String("allow", "", "")
		_ = fs.Parse(args)
		return fs
	}
	cfg := &config.Config{}

	t.Run("from sin -egress hereda (vacío)", func(t *testing.T) {
		fs := mk([]string{"-from", "plantilla"})
		egress, allow := egressForRun(fs, "plantilla", "", "", cfg)
		if egress != "" || allow != nil {
			t.Errorf("egressForRun() = (%q, %v), want (\"\", nil)", egress, allow)
		}
	})

	t.Run("from con -egress explícito no hereda", func(t *testing.T) {
		fs := mk([]string{"-from", "plantilla", "-egress", "allowlist", "-allow", "a.com,b.com"})
		egress, allow := egressForRun(fs, "plantilla", "allowlist", "a.com,b.com", cfg)
		if egress != "allowlist" || !reflect.DeepEqual(allow, []string{"a.com", "b.com"}) {
			t.Errorf("egressForRun() = (%q, %v), want (\"allowlist\", [a.com b.com])", egress, allow)
		}
	})

	t.Run("sin -from mantiene el defecto none", func(t *testing.T) {
		fs := mk(nil)
		egress, allow := egressForRun(fs, "", "", "", cfg)
		if egress != "none" || allow != nil {
			t.Errorf("egressForRun() = (%q, %v), want (\"none\", nil)", egress, allow)
		}
	})

	t.Run("sin -from respeta -egress dado", func(t *testing.T) {
		fs := mk([]string{"-egress", "internet"})
		egress, allow := egressForRun(fs, "", "internet", "", cfg)
		if egress != "internet" || allow != nil {
			t.Errorf("egressForRun() = (%q, %v), want (\"internet\", nil)", egress, allow)
		}
	})
}
