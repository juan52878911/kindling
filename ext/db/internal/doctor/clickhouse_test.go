package doctor

import "testing"

func TestCHSoloLocal(t *testing.T) {
	for def, want := range map[string]bool{
		"CREATE USER default IDENTIFIED WITH plaintext_password HOST LOCAL SETTINGS PROFILE `default`": true,
		"CREATE USER default IDENTIFIED WITH plaintext_password HOST IP '127.0.0.1', IP '::1'":         true,
		"CREATE USER default IDENTIFIED WITH plaintext_password":                                       false,
		"CREATE USER default IDENTIFIED WITH plaintext_password HOST ANY":                              false,
		"CREATE USER default IDENTIFIED WITH plaintext_password HOST IP '::/0'":                        false,
		"CREATE USER default IDENTIFIED WITH plaintext_password HOST IP '127.0.0.1', NAME 'x.example'": false,
	} {
		if got := chSoloLocal(def); got != want {
			t.Errorf("chSoloLocal(%q) = %v, want %v", def, got, want)
		}
	}
}
