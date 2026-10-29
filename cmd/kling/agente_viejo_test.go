package main

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func TestAgenteViejo(t *testing.T) {
	casos := []struct {
		ag    *api.GuestAgent
		kling string
		viejo string // "" = no; si no, un trozo del motivo
	}{
		{nil, "0.21.0", ""},
		{&api.GuestAgent{}, "0.21.0", "announces nothing"},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.21.0"}, "0.21.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.20.3"}, "v0.21.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.19.0"}, "0.21.0", "more than one minor"},
		{&api.GuestAgent{Version: "v0.17.0"}, "0.18.0", "older than v0.18"},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.20.0"}, "1.0.0", "more than one minor"},
		{&api.GuestAgent{Agent: "kling-guest", Version: "dev"}, "0.21.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.19.0"}, "dev", ""},
		{&api.GuestAgent{Agent: "kling-bridge", Version: "0.1.0"}, "0.21.0", ""},
		{&api.GuestAgent{Agent: "kling-guest", Version: "0.21.0-rc1"}, "0.22.0", ""},
	}
	for _, c := range casos {
		got := agenteViejo(c.ag, c.kling)
		if (c.viejo == "") != (got == "") || !strings.Contains(got, c.viejo) {
			t.Errorf("agenteViejo(%+v, %s) = %q; quería %q", c.ag, c.kling, got, c.viejo)
		}
	}
}

func TestCheckAgentesViejos(t *testing.T) {
	if _, ok := checkAgentesViejos([]api.Image{{Name: "nueva", Agent: &api.GuestAgent{Version: "0.21.0"}}}, "0.21.0"); ok {
		t.Fatal("avisó de una imagen al día")
	}
	c, ok := checkAgentesViejos([]api.Image{
		{Name: "nueva", Agent: &api.GuestAgent{Version: "0.21.0"}},
		{Name: "vieja", Agent: &api.GuestAgent{Agent: "kling-guest", Version: "0.17.2"}},
	}, "0.21.0")
	if !ok || c.State != doctorWarn || !strings.Contains(c.Detail, "vieja") || !strings.Contains(c.Fix, "image import -replace") {
		t.Fatalf("check = %+v", c)
	}
}
