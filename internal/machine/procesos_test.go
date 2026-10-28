package machine

import "testing"

func TestVMMsDeTabla(t *testing.T) {
	root := "/Users/juan/Library/Application Support/kindling"
	prefix := root + "/machines/"
	tabla := map[int]string{
		1:   "/sbin/launchd",
		812: "/usr/local/bin/kling-vz --api-sock /Users/juan/Library/Application Support/kindling/machines/aa11bb22cc33dd44/fc.sock",
		813: "/usr/local/bin/kling-vz --api-sock /otra/raiz/machines/ee55ff66aa77bb88/fc.sock",
		900: "vim /Users/juan/Library/Application Support/kindling/machines/notas.txt",
	}
	got := vmmsDeTabla(tabla, prefix)
	if len(got) != 1 || got["aa11bb22cc33dd44"] != 812 {
		t.Fatalf("vmms = %v", got)
	}
}

func TestVMMDeLineaRechazaRutasRaras(t *testing.T) {
	prefix := "/r/machines/"
	for _, l := range []string{
		"x --api-sock /r/machines//fc.sock",
		"x --api-sock /r/machines/a/b/fc.sock",
		"x --api-sock /r/machines/abc/otro.sock",
	} {
		if id := vmmDeLinea(l, prefix); id != "" {
			t.Errorf("%q -> %q", l, id)
		}
	}
}

// Solo cuenta los VMM de esta raíz con ese ejecutable, aunque su ruta lleve
// espacios.
func TestVivosDeVMMEnTabla(t *testing.T) {
	pre := "/Users/j/Library/Application Support/kindling/machines/"
	tabla := map[int]string{
		1: "/Users/j/.local/bin/kling-vz --socket " + pre + "a/fc.sock",
		2: "/Users/j/Library/Application Support/x/kling-vz --socket " + pre + "b/fc.sock",
		3: "/Users/j/.local/bin/kling-hv --socket " + pre + "c/fc.sock",
		4: "kling-vz --socket /otra/machines/d/fc.sock",
		5: "/usr/bin/kling-vz-viejo --socket " + pre + "e/fc.sock",
	}
	if n := vivosDeVMMEnTabla(tabla, pre, "kling-vz"); n != 2 {
		t.Fatalf("kling-vz = %d, want 2", n)
	}
	if n := vivosDeVMMEnTabla(tabla, pre, "kling-hv"); n != 1 {
		t.Fatalf("kling-hv = %d, want 1", n)
	}
}
