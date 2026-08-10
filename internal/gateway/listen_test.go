package gateway

import "testing"

// IsLoopback decide si se permite activar pprof. Un falso positivo aquí expone
// volcados de goroutines y la línea de comandos a la red, así que la tabla
// incluye las formas de decir "todas las interfaces" que no lo parecen.
func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080":    true,
		"127.0.0.1":         true,
		"localhost:8080":    true,
		"[::1]:8080":        true,
		"::1":               true,
		"127.9.9.9:8080":    true,
		"0.0.0.0:8080":      false,
		"0.0.0.0":           false,
		":8080":             false,
		"":                  false,
		"[::]:8080":         false,
		"192.168.2.60:8080": false,
		"10.0.0.1":          false,
		"lab.local:8080":    false,
	}
	for addr, want := range cases {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
