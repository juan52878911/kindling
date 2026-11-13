package buildver

import (
	"runtime/debug"
	"testing"
)

func TestResolver(t *testing.T) {
	con := func(main string, ajustes ...string) *debug.BuildInfo {
		bi := &debug.BuildInfo{Main: debug.Module{Version: main}}
		for i := 0; i+1 < len(ajustes); i += 2 {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: ajustes[i], Value: ajustes[i+1]})
		}
		return bi
	}
	casos := []struct {
		v    string
		bi   *debug.BuildInfo
		want string
	}{
		// Lo que fijó el enlazador manda siempre.
		{"1.0.0", con("v9.9.9"), "1.0.0"},
		{"v0.12.0-5-gabc", con("(devel)"), "v0.12.0-5-gabc"},
		// go install …@v1.0.0: la del módulo, sin la v (como release.yml).
		{"dev", con("v1.0.0"), "1.0.0"},
		// Un go build en un checkout: la revisión.
		{"dev", con("(devel)", "vcs.revision", "0123456789abcdef", "vcs.modified", "false"), "dev+01234567"},
		{"dev", con("v1.0.1-0.20261109120000-0123456789ab+dirty", "vcs.revision", "0123456789abcdef", "vcs.modified", "true"), "dev+01234567.dirty"},
		{"dev", con("(devel)"), "dev"},
		{"dev", nil, "dev"},
		{"", con("(devel)", "vcs.revision", "abc"), "dev+abc"},
	}
	for _, c := range casos {
		if got := resolver(c.v, c.bi); got != c.want {
			t.Errorf("resolver(%q, %+v) = %q; quería %q", c.v, c.bi, got, c.want)
		}
	}
}

func TestIsDev(t *testing.T) {
	for v, dev := range map[string]bool{"": true, "dev": true, "dev+01234567": true, "dev+0123.dirty": true,
		"1.0.0": false, "v1.0.0": false, "0.12.0-5-gabc": false, "devel": false} {
		if IsDev(v) != dev {
			t.Errorf("IsDev(%q) = %v", v, !dev)
		}
	}
}

func TestCompare(t *testing.T) {
	// Cada una es estrictamente anterior a la siguiente.
	orden := []string{"0.20.1", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-rc.1",
		"v1.0.0-rc.2", "1.0.0-rc.10", "1.0.0", "1.0.1-rc.1", "v1.0.1", "1.10.0"}
	for i := range orden {
		for j := range orden {
			c, ok := Compare(orden[i], orden[j])
			if want := signo(i - j); !ok || c != want {
				t.Errorf("Compare(%q, %q) = %d, %v; quería %d", orden[i], orden[j], c, ok, want)
			}
		}
	}
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v0.17.0-12-gabc1234", "0.17.0", 0},   // describe: la de su etiqueta
		{"1.0.0-rc.2-3-gabc1234", "1.0.0", -1}, // describe sobre una rc
		{"0.13.0-dirty", "0.13.0", 0},
		{"1.0", "1.0.0", 0},
	} {
		if got, ok := Compare(c.a, c.b); !ok || got != c.want {
			t.Errorf("Compare(%q, %q) = %d, %v; quería %d", c.a, c.b, got, ok, c.want)
		}
	}
	for _, v := range []string{"dev", "dev+0123abcd", "", "1.0.0-", "1.0.0-rc..1", "x.1.0", "1.2.3.4"} {
		if _, ok := Compare(v, "1.0.0"); ok {
			t.Errorf("Compare(%q) se pudo leer", v)
		}
	}
}

func TestIsRelease(t *testing.T) {
	for v, rel := range map[string]bool{"1.0.0": true, "v1.0.0-rc.2": true, "0.21.0-e2e.abc": true,
		"dev": false, "dev+01234567": false, "v0.12.0-5-gabc123": false, "0.13.0-dirty": false,
		"1.0.0-rc.2-1-gabcdef0": false, "1.0.0+meta": false, "": false} {
		if IsRelease(v) != rel {
			t.Errorf("IsRelease(%q) = %v", v, !rel)
		}
	}
}

func TestResolverPreRelease(t *testing.T) {
	// go install …@v1.0.0-rc.2 lo baja del proxy de módulos: sin vcs.*.
	if got := resolver("dev", &debug.BuildInfo{Main: debug.Module{Version: "v1.0.0-rc.2"}}); got != "1.0.0-rc.2" {
		t.Errorf("rc: %q", got)
	}
	// Una pseudoversión sobre una rc sigue sin ser una release.
	if got := resolver("dev", &debug.BuildInfo{Main: debug.Module{Version: "v1.0.0-rc.2.0.20261109120000-0123456789ab"}}); got != "dev" {
		t.Errorf("pseudo: %q", got)
	}
}
