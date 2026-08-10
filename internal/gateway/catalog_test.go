package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// El haystack sustituye a un strings.ToLower(Qualified+" "+Description) que se
// hacía dentro del bucle de find_tools. Este test fija que sea EXACTAMENTE lo
// mismo: si divergen, la búsqueda empieza a fallar en silencio.
func TestNewToolHaystack(t *testing.T) {
	cases := []struct{ service, name, desc string }{
		{"filesystem", "read_file", "Read the contents of a file"},
		{"eco", "echo", ""},
		{"notas", "guardar_nota", "Guarda una NOTA con Acentós y MAYÚSCULAS"},
	}
	for _, c := range cases {
		got := newTool(c.service, c.name, c.desc, nil)
		want := strings.ToLower(c.service + "." + c.name + " " + c.desc)
		if got.haystack != want {
			t.Errorf("haystack de %s.%s = %q, want %q", c.service, c.name, got.haystack, want)
		}
		if got.Qualified != c.service+"."+c.name {
			t.Errorf("Qualified = %q", got.Qualified)
		}
	}
}

// El haystack no debe salir al cable: es un detalle interno, y duplicaría el
// tamaño del catálogo que ve el modelo.
func TestHaystackNoSeSerializa(t *testing.T) {
	b, err := json.Marshal(newTool("filesystem", "read_file", "Read a file", nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "haystack") {
		t.Errorf("el haystack se está serializando: %s", b)
	}
}

// Una herramienta construida sin newTool sería invisible para find_tools sin dar
// ningún error. Este test no puede impedirlo, pero sí deja constancia de por qué
// el constructor existe: con haystack vacío, no la encuentra ni su propio nombre.
func TestToolSinHaystackNoSeEncuentra(t *testing.T) {
	mano := Tool{Service: "x", Name: "y", Qualified: "x.y", Description: "buscable"}
	if strings.Contains(mano.haystack, "buscable") {
		t.Fatal("el struct a mano no debería tener haystack")
	}
	if bueno := newTool("x", "y", "buscable", nil); !strings.Contains(bueno.haystack, "buscable") {
		t.Error("newTool debería dejarla buscable")
	}
}
