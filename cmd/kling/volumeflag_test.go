package main

import "testing"

// La sintaxis de -volume tiene que ser inequívoca: el nombre, el punto de
// montaje y el modo van en el mismo argumento, y equivocarse al partirlo montaría
// el volumen en otro sitio o en el modo contrario.
func TestSintaxisDeVolume(t *testing.T) {
	casos := []struct {
		entrada string
		nombre  string
		mount   string
		ro      bool
	}{
		{"libs", "libs", "", false},
		{"libs:/libs", "libs", "/libs", false},
		{"libs:/libs:ro", "libs", "/libs", true},
		{"datos:/data:rw", "datos", "/data", false},
		{"libs:ro", "libs", "", true},
	}
	for _, c := range casos {
		var f volumeFlag
		if err := f.Set(c.entrada); err != nil {
			t.Errorf("%q: %v", c.entrada, err)
			continue
		}
		got := f[0]
		if got.Name != c.nombre || got.Mount != c.mount || got.ReadOnly != c.ro {
			t.Errorf("%q -> %+v, quería {%s %s %v}", c.entrada, got, c.nombre, c.mount, c.ro)
		}
	}

	// Lo que no debe colar.
	for _, malo := range []string{"", ":/sin/nombre", "libs:relativa"} {
		var f volumeFlag
		if err := f.Set(malo); err == nil {
			t.Errorf("aceptó %q", malo)
		}
	}
}

// El orden en que se escriben los -volume es el orden de los discos. No es
// estético: la posición decide qué se monta dónde dentro del invitado.
func TestElOrdenDeLosVolumenesSeConserva(t *testing.T) {
	var f volumeFlag
	for _, v := range []string{"datos:/data", "libs:/libs:ro", "cache:/cache"} {
		if err := f.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := volumeSet(f, "", false)
	if err != nil {
		t.Fatal(err)
	}
	quiere := []string{"datos", "libs", "cache"}
	for i, n := range quiere {
		if got[i].Name != n {
			t.Errorf("posición %d = %q, quería %q", i, got[i].Name, n)
		}
	}

	// -mount con varios volúmenes es ambiguo —¿a cuál?— y se rechaza en vez de
	// adivinar.
	if _, err := volumeSet(f, "/data", false); err == nil {
		t.Error("aceptó -mount con varios volúmenes")
	}
	// Con uno solo sí: es la forma corta documentada.
	uno := volumeFlag{{Name: "datos"}}
	got, err = volumeSet(uno, "/data", true)
	if err != nil || got[0].Mount != "/data" || !got[0].ReadOnly {
		t.Errorf("la forma corta dejó de funcionar: %+v %v", got, err)
	}
}
