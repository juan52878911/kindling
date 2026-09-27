package domotica

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

func newTestMatcher(t testing.TB) *Matcher {
	t.Helper()
	m, err := NewMatcher(DemoTemplates)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Las órdenes de la demo que la capa 1 garantiza, con su resultado exacto.
var demoCases = []struct {
	text, intent string
	want         string // pares de Slots.Pairs() unidos por coma
}{
	{"Enciende las luces del salón, por favor", "turn_on", "device=light,area=living_room"},
	{"apaga la luz de la cocina", "turn_off", "device=light,area=kitchen"},
	{"PON LA TEMPERATURA A VEINTIDÓS GRADOS", "set_temperature", "device=thermostat,value=22,unit=°C"},
	{"pon la calefacción a 21,5", "set_temperature", "device=thermostat,value=21.5,unit=°C"},
	{"sube la persiana al 50%", "cover_set_position", "device=blinds,value=50,unit=%"},
	{"deja las cortinas del salón al cuarenta y tres por ciento", "cover_set_position", "device=blinds,area=living_room,value=43,unit=%"},
	{"reanuda la tele", "media_resume", "device=tv"},
	{"baja las persianas del dormitorio", "cover_close", "device=blinds,area=bedroom"},
	{"pon la luz azul", "set_color", "device=light,color=blue"},
	{"activa la alarma", "alarm_arm", "device=alarm"},
	{"desactiva la alarma", "alarm_disarm", "device=alarm"},
	{"cierra la puerta con llave", "lock", "device=lock"},
	{"baja el volumen de la tele", "volume_down", "device=tv"},
	{"pon el volumen al máximo", "volume_set", "value=100,unit=%"},
	{"qué temperatura hace en el dormitorio", "get_temperature", "device=thermostat,area=bedroom"},
	{"turn on the kitchen lights", "turn_on", "device=light,area=kitchen"},
	{"please set the thermostat to 21.5 degrees", "set_temperature", "device=thermostat,value=21.5,unit=°C"},
	{"set the lights to fifty percent", "set_brightness", "device=light,value=50,unit=%"},
	{"close the blinds in the bedroom", "cover_close", "device=blinds,area=bedroom"},
	{"unlock the front door", "unlock", "device=lock"},
	{"mute the tv", "volume_mute", "device=tv"},
	{"set the fan to 30 percent", "fan_set_speed", "device=fan,value=30,unit=%"},
}

func TestMatcherDemo(t *testing.T) {
	m := newTestMatcher(t)
	for _, c := range demoCases {
		got := m.Match(c.text)
		if !got.OK || got.Intent != c.intent || strings.Join(got.Slots.Pairs(), ",") != c.want {
			t.Errorf("%q: got %v %s %v, want %s %s", c.text, got.OK, got.Intent, got.Slots.Pairs(), c.intent, c.want)
		}
	}
	for _, s := range []string{"aquí hace frío", "pon una alarma a las siete", "enciende la luz y baja la persiana",
		"sube la temperatura a 22 y a 23", "turn on the coffee maker", ""} {
		if got := m.Match(s); got.OK {
			t.Errorf("%q should not match, got %s %v", s, got.Intent, got.Slots.Pairs())
		}
	}
}

func TestMatcherNoAlloc(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are meaningless under -race")
	}
	m := newTestMatcher(t)
	for _, s := range []string{"pon la temperatura del salón a veintidós grados", "no es una orden de la demo"} {
		m.Match(s)
		if n := testing.AllocsPerRun(200, func() { m.Match(s) }); n != 0 {
			t.Errorf("Match(%q) allocates %v times", s, n)
		}
	}
}

func TestNumbers(t *testing.T) {
	cases := map[string]float64{
		"22": 22, "21,5": 21.5, "21.5": 21.5, "veintidós": 22, "treinta y cinco": 35, "cien": 100,
		"ciento veinte": 120, "veintiuno y medio": 21.5, "twenty five": 25, "twenty-five": 25,
		"one hundred and five": 105, "a hundred": 100, "máximo": 100, "al mínimo": 0, "dos mil": 2000,
		"quince coma cinco": 15.5, "seventy two point five": 72.5,
	}
	for s, want := range cases {
		if got, ok := ParseValue(s); !ok || got != want {
			t.Errorf("ParseValue(%q) = %v %v, want %v", s, got, ok, want)
		}
	}
	for _, s := range []string{"un poco", "a bit", "la luz"} {
		if v, ok := ParseValue(s); ok {
			t.Errorf("ParseValue(%q) = %v, want no number", s, v)
		}
	}
	// Ida y vuelta: lo que genera NumberWords se vuelve a leer igual.
	for _, lang := range []string{"es", "en"} {
		for n := 0; n <= 100; n++ {
			if got, ok := ParseValue(NumberWords(n, lang)); !ok || got != float64(n) {
				t.Errorf("%s %d: %q parses to %v", lang, n, NumberWords(n, lang), got)
			}
		}
	}
}

func TestCanonAndSpans(t *testing.T) {
	for in, want := range map[string]string{"las luces": DevLight, "lámpara del techo": DevLight, "el enchufe de mi wemo": DevPlug,
		"la tele": DevTV, "air conditioning": DevThermostat} {
		if got, ok := CanonDevice(in); !ok || got != want {
			t.Errorf("CanonDevice(%q) = %q", in, got)
		}
	}
	if got, _ := CanonArea("the living room"); got != "living_room" {
		t.Errorf("CanonArea = %q", got)
	}
	if got, known := CanonArea("el cobertizo"); known || got != "cobertizo" {
		t.Errorf("unknown area = %q %v", got, known)
	}
	text := "enciende la luz roja del salón"
	var got []string
	for _, s := range FindSpans(text) {
		got = append(got, s.Slot+":"+text[s.Start:s.End])
	}
	if strings.Join(got, ",") != "device:luz,color:roja,area:salón" {
		t.Errorf("FindSpans = %v", got)
	}
	s := SlotsFromSpans("pon el salón a veinte grados", []slots.Span{{Slot: "area", Start: 7, End: 13}, {Slot: "value", Start: 16, End: 22}})
	if s.Area != "living_room" || !s.HasValue || s.Value != 20 || s.Unit != UnitCelsius {
		t.Errorf("SlotsFromSpans = %+v", s)
	}
}

func TestTemplate(t *testing.T) {
	g := &Grammar{Lang: "es", Rules: map[string]string{"luz": "(luz|luces)"},
		Lists: map[string]*List{"area": {Values: []ListValue{{In: "cocina", Out: "kitchen"}, {In: "(salón|sala)", Out: "living_room"}}},
			"n": {Range: true, From: 1, To: 3}}}
	n, err := Parse("(enciende|prende) [la|las] <luz> [de la {area}] (a {n};ya)")
	if err != nil {
		t.Fatal(err)
	}
	all, err := g.Enumerate(n, 1000, func(l, _ string) string { return "<" + l + ">" })
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2*3*2*2*2 {
		t.Errorf("enumerated %d sentences", len(all))
	}
	r1, r2 := &Rand{S: 7}, &Rand{S: 7}
	for i := 0; i < 50; i++ {
		a, err := g.Sample(n, r1)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := g.Sample(n, r2)
		if a.Text != b.Text {
			t.Fatal("sampling is not deterministic")
		}
		for _, c := range a.Captures {
			if c.List == "area" && Norm(a.Text[c.Start:c.End]) != Norm(map[string]string{"kitchen": "cocina", "living_room": a.Text[c.Start:c.End]}[c.Out]) {
				t.Errorf("capture %+v does not point at its value in %q", c, a.Text)
			}
		}
	}
	for _, bad := range []string{"(a|b", "[a", "<>", "{x", "a)"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if err := g.Check(&Node{kind: nRule, name: "nope"}); err == nil {
		t.Error("unknown rule accepted")
	}
}

func TestDecideWithoutModels(t *testing.T) {
	d := &Decider{Matcher: newTestMatcher(t)}
	got := d.Decide("apaga la luz del baño", "")
	if got.Layer != LayerTemplate || !got.Confident || got.Intent != "turn_off" || got.Lang != "es" {
		t.Errorf("demo command: %+v", got)
	}
	got = d.Decide("it's freezing in here", "auto")
	if got.Confident || got.Escalate != EscalateTo || got.Reason != ReasonNoModel || got.Lang != "en" {
		t.Errorf("unknown command: %+v", got)
	}
	if !MultiCommand("enciende la luz y baja la persiana") || MultiCommand("sube la luz y la persiana") ||
		!MultiCommand("turn off the lights and lock the door") {
		t.Error("MultiCommand heuristic")
	}
}

func TestResolveAndKeywords(t *testing.T) {
	s := Resolve("turn_on", Slots{Device: DevPlug, Value: 1, HasValue: true})
	if s.HasValue {
		t.Error("value kept on an intent without unit")
	}
	if s := Resolve("set_brightness", Slots{Value: 30, HasValue: true}); s.Device != DevLight || s.Unit != UnitPercent {
		t.Errorf("defaults not applied: %+v", s)
	}
	if intent, s := Keywords("please dim the lights in the kitchen"); intent != "brightness_down" || s.Area != "kitchen" {
		t.Errorf("Keywords: %s %+v", intent, s)
	}
}

func TestRowsAndChallenge(t *testing.T) {
	r := Row{Text: "pon el salón a 20", Lang: "es", Intent: "set_temperature",
		Slots: Slots{Area: "living_room", Value: 20, HasValue: true}, Spans: []slots.Span{{Slot: "area", Start: 7, End: 13}}, Source: "demo"}
	b, _ := json.Marshal(r)
	rows, err := ReadRows(bytes.NewReader(b))
	if err != nil || len(rows) != 1 || rows[0].Slots != r.Slots {
		t.Fatalf("round trip: %v %+v", err, rows)
	}
	if _, err := ReadRows(strings.NewReader(`{"text":"x","intent":"a","spans":[{"slot":"a","start":0,"end":9}]}`)); err == nil {
		t.Error("span out of range accepted")
	}
	ch := Challenge()
	if len(ch) < 40 {
		t.Fatalf("challenge set has %d rows", len(ch))
	}
	for _, c := range ch {
		if Intent(c.Intent) == nil || c.Class == "" {
			t.Errorf("bad challenge row %+v", c)
		}
	}
	// Ninguna frase de reto la contesta la capa 1 con algo que no sea lo esperado.
	m := newTestMatcher(t)
	for _, c := range ch {
		if got := m.Match(c.Text); got.OK && (got.Intent != c.Intent || !SlotsEqual(got.Slots, Resolve(c.Intent, c.Slots))) {
			t.Errorf("template layer answers %q wrongly: %s %v", c.Text, got.Intent, got.Slots.Pairs())
		}
	}
}

func BenchmarkMatch(b *testing.B) {
	m := newTestMatcher(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.Match("pon la temperatura del salón a veintidós grados")
	}
}
