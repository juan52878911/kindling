package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/domotica"
)

func TestParseVONMetrics(t *testing.T) {
	m := `# HELP kling_ai_von_wake_seconds x
kling_ai_von_wake_seconds_sum{model="g",how="restore"} 3.5
kling_ai_von_wake_seconds_count{model="g",how="restore"} 1
kling_ai_von_wake_seconds_sum{model="g",how="thaw"} 1.5
kling_ai_von_wake_seconds_count{model="g",how="thaw"} 2
kling_ai_von_wake_seconds_count{model="other",how="thaw"} 9
kling_ai_von_replicas{model="g",state="running"} 1
kling_ai_von_replicas{model="g",state="warm"} 0
`
	st := parseVONMetrics([]byte(m), "g")
	if st.Restores != 1 || st.Thaws != 2 || st.Running != 1 || st.Warm != 0 || st.WakeMS < 1666 || st.WakeMS > 1667 {
		t.Fatalf("%+v", st)
	}
}

// La capa 4 solo se enciende con un registro del mismo dorado, prompt y
// modelos rápidos, y con el alcance más amplio que pasó.
func TestLayer4Backed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KLING_DOMOTICA_MODELS", dir)
	if sc, why := layer4Backed("g", "", ""); sc != "" || !strings.Contains(why, "no eval record") {
		t.Fatal(sc, why)
	}
	rec := layer4Record{Golden: "g", PromptID: domotica.PromptID(), FastSHA: fastSHA("", ""), Scopes: map[string]scopeGate{
		domotica.ScopeAll:       {Pass: false, Why: "no"},
		domotica.ScopeUncertain: {Pass: true, Why: "yes"},
	}}
	write := func() {
		b, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(dir, "layer4-g.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if sc, _ := layer4Backed("g", "", ""); sc != domotica.ScopeUncertain {
		t.Fatalf("scope %q", sc)
	}
	rec.Scopes[domotica.ScopeAll] = scopeGate{Pass: true}
	write()
	if sc, _ := layer4Backed("g", "", ""); sc != domotica.ScopeAll {
		t.Fatalf("scope %q", sc)
	}
	rec.PromptID = "other"
	write()
	if sc, _ := layer4Backed("g", "", ""); sc != "" {
		t.Fatal("a record for another prompt must not back it")
	}
	rec.PromptID = domotica.PromptID()
	rec.FastSHA = "x"
	write()
	if sc, _ := layer4Backed("g", "", ""); sc != "" {
		t.Fatal("a record for other fast models must not back it")
	}
}

func TestBuildCascadeWithoutVON(t *testing.T) {
	m, err := demoMatcher()
	if err != nil {
		t.Fatal(err)
	}
	b := buildCascade(&domotica.Decider{Matcher: m}, nil, false, "", "")
	if b.von != "unavailable" || len(b.cascade.Slow) != 2 {
		t.Fatalf("%+v", b)
	}
}
