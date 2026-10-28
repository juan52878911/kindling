package api

import (
	"strings"
	"testing"
)

func TestEgressBlockedWarning(t *testing.T) {
	if w := (EgressBlocked{Egress: "none"}).Warning(); w != "" {
		t.Fatalf("sin cortes no hay aviso: %q", w)
	}
	e := EgressBlocked{Egress: "none", Total: 4, Destinations: []BlockedDest{
		{"example.com", "dns", 2}, {"cdn.example.net", "dns", 1}, {"1.2.3.4:443", "ip", 1}}}
	w := e.Warning()
	for _, want := range []string{"4 conexiones bloqueadas por egress none", "example.com", "1.2.3.4:443",
		"usa -egress allowlist -allow example.com,cdn.example.net"} {
		if !strings.Contains(w, want) {
			t.Errorf("falta %q en %q", want, w)
		}
	}
	// Una sola.
	one := EgressBlocked{Egress: "none", Total: 1, Destinations: []BlockedDest{{"example.com", "dns", 1}}}
	if w := one.Warning(); !strings.Contains(w, "1 conexión bloqueada por") {
		t.Errorf("singular: %q", w)
	}
	// En allowlist se añade a la lista.
	al := EgressBlocked{Egress: "allowlist", Total: 1, Destinations: []BlockedDest{{"example.org", "dns", 1}}}
	if w := al.Warning(); !strings.Contains(w, "añade -allow example.org") {
		t.Errorf("allowlist: %q", w)
	}
	// Solo IP directas: no se sugiere un allowlist que no las cubre.
	ip := EgressBlocked{Egress: "none", Total: 3, Destinations: []BlockedDest{{"1.2.3.4:80", "ip", 3}}}
	if w := ip.Warning(); strings.Contains(w, "-allow 1.2.3.4") || !strings.Contains(w, "IP directas") {
		t.Errorf("ip: %q", w)
	}
	// Muchos destinos: se resumen.
	var many []BlockedDest
	for i := 0; i < 20; i++ {
		many = append(many, BlockedDest{"h" + string(rune('a'+i)) + ".test", "dns", 1})
	}
	if w := (EgressBlocked{Egress: "none", Total: 20, Destinations: many}).Warning(); !strings.Contains(w, "(+12)") {
		t.Errorf("resumen: %q", w)
	}
}
