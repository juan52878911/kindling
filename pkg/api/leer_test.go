package api

import (
	"strings"
	"testing"
)

func TestLeerCuerpoFallaEnVezDeCortar(t *testing.T) {
	// Justo el tope: pasa.
	b, err := LeerCuerpo(strings.NewReader("12345"), 5)
	if err != nil || string(b) != "12345" {
		t.Errorf("justo en el tope deberia pasar entero: %q, %v", b, err)
	}
	// Por debajo: pasa.
	if b, err := LeerCuerpo(strings.NewReader("123"), 5); err != nil || string(b) != "123" {
		t.Errorf("por debajo del tope: %q, %v", b, err)
	}
	// Un byte de mas: FALLA. Con io.ReadAll(io.LimitReader(...)) esto devolveria
	// "12345" y err == nil, y el JSON llegaria cortado a quien lo parsea.
	if b, err := LeerCuerpo(strings.NewReader("123456"), 5); err == nil {
		t.Errorf("un cuerpo mayor que el tope se trunco en silencio: %q", b)
	}
	// Vacio: no es un error.
	if b, err := LeerCuerpo(strings.NewReader(""), 5); err != nil || len(b) != 0 {
		t.Errorf("un cuerpo vacio: %q, %v", b, err)
	}
}

// El error tiene que decir que fue el TAMANO. Si dijera "unexpected end of
// JSON", quien lo lea buscaria el fallo en el servidor equivocado.
func TestElErrorDeTamanoDiceQueEsDeTamano(t *testing.T) {
	_, err := LeerCuerpo(strings.NewReader(strings.Repeat("x", 100)), 10)
	if err == nil {
		t.Fatal("no fallo")
	}
	for _, quiero := range []string{"exceeds", "10"} {
		if !strings.Contains(err.Error(), quiero) {
			t.Errorf("el error no menciona %q: %v", quiero, err)
		}
	}
}
