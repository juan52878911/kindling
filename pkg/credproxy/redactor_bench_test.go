package credproxy

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

// credsBanco son n credenciales distintas con el formato de las reales.
func credsBanco(n int) []Credential {
	cs := make([]Credential, n)
	for i := range cs {
		cs[i] = Credential{
			Placeholder: fmt.Sprintf("kling-cred-%040d", i),
			Secret:      fmt.Sprintf("sk_live_SECRETO_%02d_0123456789abcdef", i),
		}
	}
	return cs
}

// BenchmarkRedactor mide redactar una respuesta de 256 KiB sin claves (el
// caso habitual) tal como la sirve ServeHTTP: redactor nuevo por petición y
// el cuerpo en trozos de 32 KiB.
func BenchmarkRedactor(b *testing.B) {
	body := bytes.Repeat([]byte(`{"id":"obj_123","name":"algo de texto normal"},`), (256<<10)/47)
	for _, n := range []int{1, 4} {
		cs := credsBanco(n)
		b.Run(fmt.Sprintf("creds=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				r := nuevoRedactor(io.Discard, cs)
				if _, err := r.ReadFrom(bytes.NewReader(body)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRedactorPrecalculado es el camino real de ServeHTTP desde que
// SetCredentials precalcula los pares por dominio.
func BenchmarkRedactorPrecalculado(b *testing.B) {
	body := bytes.Repeat([]byte(`{"id":"obj_123","name":"algo de texto normal"},`), (256<<10)/47)
	for _, n := range []int{1, 4} {
		pares := paresRedaccion(credsBanco(n))
		b.Run(fmt.Sprintf("creds=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				r := nuevoRedactorPares(io.Discard, pares)
				if _, err := r.ReadFrom(bytes.NewReader(body)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Con los pares compartidos entre redactores, una clave partida entre trozos
// se sigue redactando en cada uno y no se mezcla lo retenido de otro, ni se
// toca el slice del llamador.
func TestRedactorParesCompartidosYTrozos(t *testing.T) {
	cs := credsBanco(4)
	pares := paresRedaccion(cs)
	full := "a " + cs[0].Secret + " b " + cs[3].Secret + " c"
	for _, paso := range []int{1, 3, 7, 1000} {
		var o1, o2 bytes.Buffer
		r1, r2 := nuevoRedactorPares(&o1, pares), nuevoRedactorPares(&o2, pares)
		for i := 0; i < len(full); i += paso {
			trozo := []byte(full[i:min(i+paso, len(full))])
			copia := string(trozo)
			r1.Write(trozo)
			r2.Write(trozo)
			if string(trozo) != copia {
				t.Fatalf("paso %d: el redactor modificó el trozo del llamador", paso)
			}
		}
		r1.vaciar()
		r2.vaciar()
		want := "a " + cs[0].Placeholder + " b " + cs[3].Placeholder + " c"
		if o1.String() != want || o2.String() != want {
			t.Errorf("paso %d: %q / %q, quería %q", paso, o1.String(), o2.String(), want)
		}
	}
}

// Un par añadido en una petición (r.par) no aparece en el redactor de otra
// que comparte los pares precalculados.
func TestRedactorParesPropiosNoSeComparten(t *testing.T) {
	pares := paresRedaccion(credsBanco(2))
	pares = append(pares, make([]par, 8)...)[:len(pares)] // con capacidad sobrante
	r1 := nuevoRedactorPares(io.Discard, pares)
	r1.par("firma-de-esta-peticion-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "marcador")
	r1.par("z", "y")
	var out bytes.Buffer
	r2 := nuevoRedactorPares(&out, pares)
	if len(r2.pares) != len(pares) {
		t.Fatalf("r2 ve %d pares, quería %d", len(r2.pares), len(pares))
	}
	r2.Write([]byte("z firma-de-esta-peticion-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"))
	r2.vaciar()
	if out.String() != "z firma-de-esta-peticion-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" {
		t.Fatalf("r2 usó pares de r1: %q", out.String())
	}
	for i, p := range paresRedaccion(credsBanco(2)) {
		if string(pares[i].old) != string(p.old) {
			t.Fatalf("par %d cambiado en el slice compartido: %q", i, pares[i].old)
		}
	}
}
