package api

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestMachineEnvMap(t *testing.T) {
	m, err := MachineEnvMap([]string{"PW=a", "URL=x=y", "VACIA=", "PW=b", "MULTI=l1\nl2"})
	if err != nil || len(m) != 4 || m["PW"] != "b" || m["URL"] != "x=y" || m["VACIA"] != "" || m["MULTI"] != "l1\nl2" {
		t.Fatalf("%v %v", m, err)
	}
	if !slices.Equal(MachineEnvKeys(m), []string{"MULTI", "PW", "URL", "VACIA"}) {
		t.Fatalf("keys %q", MachineEnvKeys(m))
	}
	if m, err := MachineEnvMap(nil); m != nil || err != nil {
		t.Fatal("nil")
	}
	for _, bad := range [][]string{
		{"secreto-sin-igual"}, {"=secreto"}, {"1A=secreto"}, {"A-B=secreto"}, {"A=secreto\x00"},
		{"A=" + strings.Repeat("s", MaxMachineEnvBytes)},
		make([]string, MaxMachineEnvVars+1),
	} {
		if _, err := MachineEnvMap(bad); err == nil || strings.Contains(err.Error(), "secreto") {
			t.Errorf("%.40q: err %v (must fail without the value)", bad, err)
		}
	}
}

// Los valores no tienen sitio en Machine ni en Snapshot: solo los nombres.
func TestMachineSoloNombres(t *testing.T) {
	b, _ := json.Marshal(Machine{EnvKeys: []string{"PW"}})
	if !strings.Contains(string(b), `"env_keys":["PW"]`) {
		t.Fatal(string(b))
	}
	b, _ = json.Marshal(RunRequest{Image: "x", Env: []string{"PW=v"}})
	var back RunRequest
	if err := json.Unmarshal(b, &back); err != nil || !slices.Equal(back.Env, []string{"PW=v"}) {
		t.Fatalf("RunRequest.Env no viaja: %s", b)
	}
}
