package main

import (
	"testing"

	"github.com/juan52878911/kindling/examples/domotica/internal/domotica"
)

func TestYAML(t *testing.T) {
	src := `---
# comentario
language: "es"
data:
  - sentences:
      - "<enciende> [el|la] {name}"  # comentario
      - '<apaga> {area}'
    slots:
      volume_step: "up"
    name_domains:
      - light
    speech_to_phrase: true
  - sentences: [a, "b"]
lists:
  cover_classes:
    values:
      - in: persiana[s]
        out: blind
      - toldo
expansion_rules:
  añadir: (añad(a|e|ir|í)|apunt[a|e])
responses:
  x: |
    línea uno
    línea dos
---
lists:
  brightness:
    range:
      from: 0
      to: 100
`
	docs, err := parseYAMLDocs(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("%d docs", len(docs))
	}
	d := ymap(docs[0])
	data := ylist(d["data"])
	b0 := ymap(data[0])
	if s := ylist(b0["sentences"]); len(s) != 2 || ystr(s[0]) != "<enciende> [el|la] {name}" || ystr(s[1]) != "<apaga> {area}" {
		t.Errorf("sentences %#v", s)
	}
	if ystr(ymap(b0["slots"])["volume_step"]) != "up" || ystr(ylist(b0["name_domains"])[0]) != "light" {
		t.Errorf("block %#v", b0)
	}
	if s := ylist(ymap(data[1])["sentences"]); len(s) != 2 || ystr(s[1]) != "b" {
		t.Errorf("flow list %#v", s)
	}
	if ystr(ymap(d["expansion_rules"])["añadir"]) != "(añad(a|e|ir|í)|apunt[a|e])" {
		t.Errorf("rules %#v", d["expansion_rules"])
	}
	if ystr(ymap(d["responses"])["x"]) != "línea uno\nlínea dos" {
		t.Errorf("block scalar %q", ymap(d["responses"])["x"])
	}
	l, err := parseList(ymap(ymap(d["lists"]))["cover_classes"])
	if err != nil || len(l.Values) != 2 || l.Values[0].Out != "blind" || l.Values[1].In != "toldo" {
		t.Errorf("list %+v %v", l, err)
	}
	r, err := parseList(ymap(ymap(docs[1])["lists"])["brightness"])
	if err != nil || !r.Range || r.To != 100 {
		t.Errorf("range %+v %v", r, err)
	}
	for _, bad := range []string{"a: [x", "a: {b: c}", "a: &anchor x", "- a\n b: c"} {
		if _, err := parseYAMLDocs(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseAnnot(t *testing.T) {
	text, spans, err := parseAnnot("apaga la [house_place : cocina] al [change_amount : cincuenta por ciento]")
	if err != nil || text != "apaga la cocina al cincuenta por ciento" || len(spans) != 2 {
		t.Fatalf("%q %v %v", text, spans, err)
	}
	if text[spans[0].Start:spans[0].End] != "cocina" {
		t.Errorf("span %+v", spans[0])
	}
	v, ok := valueSpan(text, spans[1])
	if !ok || text[v.Start:v.End] != "cincuenta" {
		t.Errorf("value span %q", text[v.Start:v.End])
	}
}

func TestMapHA(t *testing.T) {
	name := func(out string) map[string]domotica.Capture {
		return map[string]domotica.Capture{"name": {Slot: "name", Out: out}}
	}
	cases := []struct {
		b      haBlock
		caps   map[string]domotica.Capture
		intent string
		ok     bool
	}{
		{haBlock{intent: "HassTurnOn", inferred: "light"}, nil, "turn_on", true},
		{haBlock{intent: "HassTurnOff"}, name("lock|lock"), "unlock", true},
		{haBlock{intent: "HassTurnOn"}, map[string]domotica.Capture{"device_class": {Out: "garage"}}, "", false},
		{haBlock{intent: "HassTurnOff"}, map[string]domotica.Capture{"device_class": {Out: "blind"}}, "cover_close", true},
		{haBlock{intent: "HassTurnOn", fixed: map[string]string{"domain": "scene"}}, nil, domotica.OutOfScope, true},
		{haBlock{intent: "HassSetVolumeRelative", fixed: map[string]string{"volume_step": "down"}}, nil, "volume_down", true},
		{haBlock{intent: "HassStartTimer"}, nil, domotica.OutOfScope, true},
	}
	for _, c := range cases {
		if c.b.fixed == nil {
			c.b.fixed = map[string]string{}
		}
		got, _, ok := mapHA(c.b, c.caps)
		if got != c.intent || ok != c.ok {
			t.Errorf("%+v: got %q %v, want %q %v", c.b, got, ok, c.intent, c.ok)
		}
	}
}

func TestAssignSplits(t *testing.T) {
	var rows []domotica.Row
	for i := 0; i < 50; i++ {
		fam := string(rune('a'+i%10)) + "x"
		rows = append(rows, domotica.Row{Source: "ha", Intent: "lock", Family: fam}, domotica.Row{Source: "ha", Intent: "lock", Family: fam})
	}
	assignSplits(rows)
	bySplit := map[string]map[string]bool{}
	famSplit := map[string]string{}
	for _, r := range rows {
		if s, ok := famSplit[r.Family]; ok && s != r.Split {
			t.Fatalf("family %s in two splits", r.Family)
		}
		famSplit[r.Family] = r.Split
		if bySplit[r.Split] == nil {
			bySplit[r.Split] = map[string]bool{}
		}
		bySplit[r.Split][r.Family] = true
	}
	if len(bySplit["train"]) != 7 || len(bySplit["test"]) != 2 || len(bySplit["valid"]) != 1 {
		t.Errorf("splits %v", bySplit)
	}
}
