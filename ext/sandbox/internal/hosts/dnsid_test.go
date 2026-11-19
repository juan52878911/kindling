package hosts

import "testing"

func TestIDDNSIda(t *testing.T) {
	for _, id := range []string{"lab/0123456789abcdef", "h-1.x_y/ff00ff00ff00ff00"} {
		e := IDaDNS(id)
		for _, c := range e {
			if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
				t.Fatalf("%q: carácter %q fuera de a-z2-7", e, c)
			}
		}
		got, err := IDdeDNS(e)
		if err != nil || got != id {
			t.Fatalf("ida y vuelta de %q = %q, %v", id, got, err)
		}
	}
	if _, err := IDdeDNS("no es base32!"); err == nil {
		t.Fatal("un id mal formado se aceptó")
	}
}
