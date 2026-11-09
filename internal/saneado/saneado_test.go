package saneado

import "testing"

// Ni OSC 52 (el portapapeles), ni el título, ni CSI para borrar o mover el
// cursor llegan en crudo a la terminal.
func TestConsola(t *testing.T) {
	for in, want := range map[string]string{
		"hola\n":                                 "hola\n",
		"a\tb\r\n":                               "a\tb\r\n",
		"ñandú ✓\n":                              "ñandú ✓\n",
		"\x1b]52;c;Y3VybCBldmlsLnNoIHwgc2g=\x07": `\x1b]52;c;Y3VybCBldmlsLnNoIHwgc2g=\x07`,
		"\x1b]0;root@prod\x07":                   `\x1b]0;root@prod\x07`,
		"ok\x1b[2K\x1b[1A[ OK ] fake":            `ok\x1b[2K\x1b[1A[ OK ] fake`,
		"c1 \u009b31m":                           `c1 \u009b31m`,
		"del\x7f bs\x08 nul\x00":                 `del\x7f bs\x08 nul\x00`,
		"roto \xff\xfe":                          `roto \xff\xfe`,
	} {
		if got := Consola(in); got != want {
			t.Errorf("Consola(%q) = %q, want %q", in, got, want)
		}
	}
}

// Texto deja los saltos de línea y escapa el \r, que taparía lo anterior.
func TestTexto(t *testing.T) {
	for in, want := range map[string]string{
		"a\nb\tc":                "a\nb\tc",
		"ok\rerror: falso":       `ok\x0derror: falso`,
		"\x1b]52;c;ZXZpbA==\x07": `\x1b]52;c;ZXZpbA==\x07`,
	} {
		if got := Texto(in); got != want {
			t.Errorf("Texto(%q) = %q, want %q", in, got, want)
		}
	}
}

// Linea no deja partir ni desalinear la fila.
func TestLinea(t *testing.T) {
	for in, want := range map[string]string{
		"public.users.email": "public.users.email",
		"a\nb\tc\rd":         `a\x0ab\x09c\x0dd`,
		"x\x1b[2Jy":          `x\x1b[2Jy`,
	} {
		if got := Linea(in); got != want {
			t.Errorf("Linea(%q) = %q, want %q", in, got, want)
		}
	}
}
