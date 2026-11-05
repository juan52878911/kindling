package api

import "testing"

func TestParseHostPort(t *testing.T) {
	for in, want := range map[string]HostPort{
		"11434":      {11434, 11434},
		"11434/tcp":  {11434, 11434},
		"8080:18080": {8080, 18080},
		" 1:65535 ":  {1, 65535},
	} {
		got, err := ParseHostPort(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "x", "11434/udp", "0", "70000", "1:2:3", "11434:53", "11434:80", "11434:443", "a:1"} {
		if _, err := ParseHostPort(in); err == nil {
			t.Errorf("aceptó %q", in)
		}
	}
	if err := ValidateHostPorts([]HostPort{{1, 9000}, {2, 9000}}); err == nil {
		t.Error("aceptó dos en el mismo puerto del invitado")
	}
	muchos := make([]HostPort, MaxHostPorts+1)
	for i := range muchos {
		muchos[i] = HostPort{1000 + i, 1000 + i}
	}
	if err := ValidateHostPorts(muchos); err == nil {
		t.Error("aceptó más de MaxHostPorts")
	}
	if (HostPort{11434, 11434}).String() != "11434" || (HostPort{8080, 18080}).String() != "8080:18080" {
		t.Error("String")
	}
}
