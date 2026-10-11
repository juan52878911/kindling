package api

import "testing"

func TestParseOnRestore(t *testing.T) {
	for _, c := range []struct {
		v      string
		action string
		sig    int
		ok     bool
	}{
		{"", OnRestoreNone, 0, true},
		{"none", OnRestoreNone, 0, true},
		{"restart", OnRestoreRestart, 0, true},
		{"signal:SIGUSR1", OnRestoreSignal, 10, true}, // número de Linux
		{"signal:hup", OnRestoreSignal, 1, true},
		{"signal:", "", 0, false},
		{"signal:NOPE", "", 0, false},
		{"reload", "", 0, false},
	} {
		a, n, err := ParseOnRestore(c.v)
		if (err == nil) != c.ok || a != c.action || n != c.sig {
			t.Errorf("ParseOnRestore(%q) = %q, %d, %v", c.v, a, n, err)
		}
	}
}
