package machine

import "testing"

func TestParsearPSyVMMs(t *testing.T) {
	root := "/Users/juan/Library/Application Support/kindling"
	prefix := root + "/machines/"
	out := `    1 /sbin/launchd
  812 /usr/local/bin/kling-vz --api-sock /Users/juan/Library/Application Support/kindling/machines/aa11bb22cc33dd44/fc.sock
  813 /usr/local/bin/kling-vz --api-sock /otra/raiz/machines/ee55ff66aa77bb88/fc.sock
  900 vim /Users/juan/Library/Application Support/kindling/machines/notas.txt
  abc basura
`
	tabla := parsearPS(out)
	if len(tabla) != 4 {
		t.Fatalf("tabla = %v", tabla)
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
