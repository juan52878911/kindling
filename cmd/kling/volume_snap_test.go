package main

import "testing"

// "notas@" no puede acabar borrando el volumen entero creyendo borrar un
// snapshot.
func TestSplitVolumeSnapshot(t *testing.T) {
	for _, c := range []struct{ in, vol, snap string }{
		{"notas", "notas", ""},
		{"notas@uno", "notas", "uno"},
	} {
		v, s, err := splitVolumeSnapshot(c.in)
		if err != nil || v != c.vol || s != c.snap {
			t.Errorf("%q -> %q %q %v", c.in, v, s, err)
		}
	}
	for _, in := range []string{"notas@", "@uno", "", "a@b@c"} {
		if _, _, err := splitVolumeSnapshot(in); err == nil {
			t.Errorf("%q debería rechazarse", in)
		}
	}
}
