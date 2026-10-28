package main

import (
	"strings"
	"testing"
)

func TestEmulacionImport(t *testing.T) {
	for _, c := range []struct {
		daemon, arch  string
		emulate, dado bool
		want          string
		emu, aviso    bool
		err           string
	}{
		{"arm64", "", false, false, "", false, false, ""},
		{"arm64", "arm64", false, false, "arm64", false, false, ""},
		{"arm64", "amd64", false, false, "amd64", true, true, ""},
		{"arm64", "amd64", true, true, "amd64", true, false, ""},
		{"arm64", "", true, true, "amd64", true, false, ""},
		// Construir para un anfitrión amd64, como antes.
		{"arm64", "amd64", false, true, "amd64", false, false, ""},
		{"amd64", "arm64", false, false, "arm64", false, false, ""},
		{"amd64", "arm64", true, true, "", false, false, "only runs amd64 images on an arm64"},
		{"amd64", "", true, true, "", false, false, "other than the daemon's"},
		{"", "amd64", false, false, "amd64", false, false, ""},
	} {
		a, emu, aviso, err := emulacionImport(c.daemon, c.arch, c.emulate, c.dado)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%+v: err %v", c, err)
			}
			continue
		}
		if err != nil || a != c.want || emu != c.emu || (aviso != "") != c.aviso {
			t.Errorf("%+v: %q %v %q %v", c, a, emu, aviso, err)
		}
	}
}

func TestNombreEmulada(t *testing.T) {
	if got := nombreEmulada("nginx-latest", "amd64"); got != "nginx-latest-amd64" {
		t.Fatal(got)
	}
	largo := strings.Repeat("a", 62)
	if got := nombreEmulada(largo, "amd64"); len(got) != 64 || !reBuildName.MatchString(got) || !strings.HasSuffix(got, "-amd64") {
		t.Fatal(got)
	}
}
