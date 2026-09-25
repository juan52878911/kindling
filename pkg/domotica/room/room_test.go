package room

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/domotica"
)

// Los botones «direct» tienen que salir de las plantillas de la demo: los
// contesta la capa 1, siempre.
func TestPresetsDirectAreTemplates(t *testing.T) {
	m, err := domotica.NewMatcher(domotica.DemoTemplates)
	if err != nil {
		t.Fatal(err)
	}
	d := &domotica.Decider{Matcher: m}
	for _, p := range Presets {
		dec := d.Decide(p.Text, p.Lang)
		if p.Group == "direct" && (dec.Layer != domotica.LayerTemplate || !dec.Confident) {
			t.Errorf("%q is not answered by the template layer: %+v", p.Text, dec)
		}
		if p.Group != "direct" && dec.Layer == domotica.LayerTemplate {
			t.Errorf("%q (%s) is a template: move it to direct", p.Text, p.Group)
		}
	}
}

func a(intent string, s domotica.Slots) domotica.Action {
	return domotica.Action{Intent: intent, Slots: domotica.Resolve(intent, s)}
}

func TestApply(t *testing.T) {
	r := New()
	eff, st := r.Apply([]domotica.Action{
		a("turn_on", domotica.Slots{Area: "kitchen"}),
		a("set_color", domotica.Slots{Area: "bedroom", Color: "blue"}),
		a("cover_set_position", domotica.Slots{Value: 50, HasValue: true}),
		a("set_temperature", domotica.Slots{Value: 23, HasValue: true}),
		a("unlock", domotica.Slots{}),
		a("fan_set_speed", domotica.Slots{Value: 60, HasValue: true}),
	})
	if len(eff) != 6 {
		t.Fatal(eff)
	}
	for _, e := range eff {
		if !e.OK || e.SayES == "" || e.SayEN == "" || len(e.Changed) == 0 {
			t.Errorf("effect %+v", e)
		}
	}
	if !st.Lights[1].On || st.Lights[2].Color != "blue" || !st.Lights[2].On || st.Blinds[0].Position != 50 || st.Blinds[1].Position != 50 ||
		st.Thermostat.Target != 23 || st.Locked || st.Fan != 60 {
		t.Fatalf("state %+v", st)
	}
	// Una zona sin ese dispositivo: no se toca nada y se dice.
	eff, _ = r.Apply([]domotica.Action{a("cover_close", domotica.Slots{Area: "kitchen"})})
	if eff[0].OK {
		t.Fatalf("blinds in the kitchen: %+v", eff[0])
	}
	// «Apaga todo»: todas las luces.
	_, st = r.Apply([]domotica.Action{a("turn_off", domotica.Slots{Area: "house"})})
	for _, l := range st.Lights {
		if l.On {
			t.Fatalf("light still on: %+v", l)
		}
	}
	// Volumen sin dispositivo: el que suena (el altavoz al empezar).
	_, st = r.Apply([]domotica.Action{a("volume_up", domotica.Slots{})})
	if st.Speaker.Volume != 40 {
		t.Fatalf("speaker volume %d", st.Speaker.Volume)
	}
}

func TestTick(t *testing.T) {
	r := New()
	for i := 0; i < 30 && r.Tick(); i++ {
	}
	if s := r.Snapshot(); s.Thermostat.Current != s.Thermostat.Target {
		t.Fatalf("thermostat did not converge: %+v", s.Thermostat)
	}
}

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	m, err := domotica.NewMatcher(domotica.DemoTemplates)
	if err != nil {
		t.Fatal(err)
	}
	c := &domotica.Cascade{Fast: &domotica.Decider{Matcher: m}, Slow: []domotica.NamedLayer{{Name: domotica.LayerVON, Layer: domotica.Unavailable}}}
	s := NewServer(Options{Decide: c.Decide, Presets: Presets, LoopbackOnly: true,
		Layers: []LayerInfo{{Name: "template", Status: "on"}, {Name: "von", Status: "unavailable"}}})
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return s, hs
}

func post(t *testing.T, url, ct, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestServerCommand(t *testing.T) {
	s, hs := testServer(t)
	resp := post(t, hs.URL+"/api/command", "application/json", `{"text":"apaga la luz del salón","lang":"es"}`)
	defer resp.Body.Close()
	var cr CommandResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, err)
	}
	if cr.Trace.Decided != domotica.LayerTemplate || len(cr.Effects) != 1 || cr.State.Lights[0].On {
		t.Fatalf("%+v", cr)
	}
	if st := s.stats(); st.Total != 1 || st.ByLayer["template"] != 1 {
		t.Fatalf("stats %+v", st)
	}
	// Lo que escala sin capas lentas: nada.
	resp = post(t, hs.URL+"/api/command", "application/json", `{"text":"aquí hace frío"}`)
	_ = json.NewDecoder(resp.Body).Decode(&cr)
	resp.Body.Close()
	if cr.Trace.Decided != "none" || len(cr.Effects) != 0 || cr.Trace.Steps[1].Status != domotica.StepUnavailable {
		t.Fatalf("%+v", cr.Trace)
	}
}

func TestServerGuards(t *testing.T) {
	_, hs := testServer(t)
	cases := []struct {
		ct, body string
		want     int
	}{
		{"text/plain", `{"text":"x"}`, http.StatusUnsupportedMediaType},
		{"", `{"text":"x"}`, http.StatusUnsupportedMediaType},
		{"application/json", `{"text":""}`, http.StatusBadRequest},
		{"application/json", `{"text":"x","extra":1}`, http.StatusBadRequest},
		{"application/json", `{"text":"` + strings.Repeat("a", 301) + `"}`, http.StatusBadRequest},
		{"application/json", `{"text":"` + strings.Repeat("a", 5000) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		resp := post(t, hs.URL+"/api/command", c.ct, c.body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %.40s: got %d, want %d", c.ct, c.body, resp.StatusCode, c.want)
		}
	}
	// DNS rebinding: un Host que no es de loopback se rechaza.
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/api/state", nil)
	req.Host = "evil.example:8088"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host: %d", resp.StatusCode)
	}
	// La página y su CSP.
	resp, err = http.Get(hs.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("index: %d %v", resp.StatusCode, resp.Header)
	}
	for _, p := range []string{"/app.js", "/app.css"} {
		resp, err = http.Get(hs.URL + p)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %v %v", p, err, resp)
		}
		resp.Body.Close()
	}
}

func TestServerEvents(t *testing.T) {
	_, hs := testServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hs.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatal(ct)
	}
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil { // retry:
		t.Fatal(err)
	}
	go func() {
		r := post(t, hs.URL+"/api/command", "application/json", `{"text":"enciende la tele"}`)
		r.Body.Close()
	}()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "event: decision") {
			data, _ := br.ReadString('\n')
			if !strings.Contains(data, `"decided_by":"template"`) {
				t.Fatalf("decision event: %s", data)
			}
			return
		}
	}
}
