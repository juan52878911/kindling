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
