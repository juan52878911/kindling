package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestVersion(t *testing.T) {
	if got := Version("v0.14.0"); got != "v0.14.0" {
		t.Fatalf("la inyectada manda: %q", got)
	}
	if got := Version("dev"); got == "" {
		t.Fatal("dev sin build info debe seguir diciendo algo")
	}
	cases := []struct {
		name string
		bi   *debug.BuildInfo
		ok   bool
		want string
	}{
		{"sin build info", nil, false, "dev"},
		{"go install @tag", &debug.BuildInfo{Main: debug.Module{Version: "v0.14.0"}}, true, "v0.14.0"},
		{"devel sin vcs", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, "dev"},
		{"revisión limpia", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "450cfd6a1b2c3d4e5f60"}, {Key: "vcs.modified", Value: "false"}}}, true, "450cfd6a1b2c"},
		{"revisión sucia", &debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc"}, {Key: "vcs.modified", Value: "true"}}}, true, "abc-dirty"},
	}
	for _, c := range cases {
		if got := fromBuildInfo(c.bi, c.ok); got != c.want {
			t.Errorf("%s: %q, quería %q", c.name, got, c.want)
		}
	}
}
