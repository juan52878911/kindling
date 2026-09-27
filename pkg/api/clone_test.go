package api

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// llenarNoCero rellena v (y todo lo que cuelga de ella) con valores propios,
// no cero, usando reflect. Cada slice y cada mapa recibe un elemento nuevo, y
// cada puntero apunta a un valor recién asignado —nunca a nil—, para que
// CUALQUIER campo por referencia de la estructura, presente o futuro, quede
// cubierto por la comprobación de más abajo sin tener que enumerarlo a mano.
//
// El contador sem da a cada campo un valor distinto, para poder distinguir
// "el clon apunta a otra cosa" de "el clon apunta a lo mismo por casualidad".
func llenarNoCero(v reflect.Value, sem *int) {
	if !v.CanSet() {
		return // campo no exportado: no hay nada que clonar ahí
	}
	switch v.Kind() {
	case reflect.Ptr:
		p := reflect.New(v.Type().Elem())
		llenarNoCero(p.Elem(), sem)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		llenarNoCero(s.Index(0), sem)
		v.Set(s)
	case reflect.Map:
		mv := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		llenarNoCero(k, sem)
		val := reflect.New(v.Type().Elem()).Elem()
		llenarNoCero(val, sem)
		mv.SetMapIndex(k, val)
		v.Set(mv)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			*sem++
			v.Set(reflect.ValueOf(time.Unix(int64(1_700_000_000+*sem), 0)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			llenarNoCero(v.Field(i), sem)
		}
	case reflect.String:
		*sem++
		v.SetString(fmt.Sprintf("v%d", *sem))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		*sem++
		v.SetInt(int64(*sem))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		*sem++
		v.SetUint(uint64(*sem))
	case reflect.Float32, reflect.Float64:
		*sem++
		v.SetFloat(float64(*sem))
	}
}

// verificarCopiaProfunda compara orig y clon campo a campo (recursivamente) y
// falla si:
//   - un puntero, slice o mapa del clon señala al MISMO sitio que el del
//     original (Clone() se limitó a copiar la cabecera, no el contenido); o
//   - el valor, una vez seguido el puntero/slice/mapa, no es igual.
//
// Recorre la estructura entera y no solo el nivel de Machine: así un slice o
// mapa dentro de un slice también queda cubierto sin tener que enumerarlo.
func verificarCopiaProfunda(t *testing.T, ruta string, orig, clon reflect.Value) {
	t.Helper()
	switch orig.Kind() {
	case reflect.Ptr:
		if orig.IsNil() {
			if !clon.IsNil() {
				t.Errorf("%s: nil en el original pero no en el clon", ruta)
			}
			return
		}
		if clon.IsNil() {
			t.Errorf("%s: el clon perdió el puntero", ruta)
			return
		}
		if orig.Pointer() == clon.Pointer() {
			t.Errorf("%s: el clon comparte el MISMO puntero que el original; no es una copia profunda", ruta)
			return
		}
		verificarCopiaProfunda(t, ruta, orig.Elem(), clon.Elem())
	case reflect.Slice:
		if orig.IsNil() {
			return
		}
		if clon.IsNil() {
			t.Errorf("%s: el clon perdió el slice", ruta)
			return
		}
		if orig.Len() > 0 && orig.Pointer() == clon.Pointer() {
			t.Errorf("%s: el clon comparte el MISMO array subyacente que el original", ruta)
			return
		}
		if orig.Len() != clon.Len() {
			t.Errorf("%s: longitudes distintas tras clonar (%d != %d)", ruta, orig.Len(), clon.Len())
			return
		}
		for i := 0; i < orig.Len(); i++ {
			verificarCopiaProfunda(t, fmt.Sprintf("%s[%d]", ruta, i), orig.Index(i), clon.Index(i))
		}
	case reflect.Map:
		if orig.IsNil() {
			return
		}
		if clon.IsNil() {
			t.Errorf("%s: el clon perdió el mapa", ruta)
			return
		}
		if orig.Len() > 0 && orig.Pointer() == clon.Pointer() {
			t.Errorf("%s: el clon comparte el MISMO mapa que el original", ruta)
			return
		}
		for _, k := range orig.MapKeys() {
			cv := clon.MapIndex(k)
			if !cv.IsValid() {
				t.Errorf("%s: falta la clave %v en el clon", ruta, k)
				continue
			}
			verificarCopiaProfunda(t, fmt.Sprintf("%s[%v]", ruta, k), orig.MapIndex(k), cv)
		}
	case reflect.Struct:
		if orig.Type() == reflect.TypeOf(time.Time{}) {
			if !orig.Interface().(time.Time).Equal(clon.Interface().(time.Time)) {
				t.Errorf("%s: la fecha cambió al clonar", ruta)
			}
			return
		}
		for i := 0; i < orig.NumField(); i++ {
			verificarCopiaProfunda(t, ruta+"."+orig.Type().Field(i).Name, orig.Field(i), clon.Field(i))
		}
	default:
		if !reflect.DeepEqual(orig.Interface(), clon.Interface()) {
			t.Errorf("%s: %v != %v tras clonar", ruta, orig.Interface(), clon.Interface())
		}
	}
}

// TestCloneCubreTodoCampoPorReferencia es la prueba de cobertura que pide el
// plan: si algún día se añade a Machine un slice, un mapa o un puntero nuevo
// y Clone() no aprende a copiarlo, este test lo detecta solo —no hace falta
// que quien añade el campo se acuerde de actualizar una lista aparte—.
//
// La receta es la misma que expuso M-03: rellenar CADA campo por referencia
// con datos propios (llenarNoCero) y comprobar que el clon no comparte
// memoria con el original en ningún punto alcanzable (verificarCopiaProfunda).
// Un Clone() que hiciera `out := *mc` y nada más pasaría los tests que solo
// miran valores, pero no este: los punteros y slices seguirían siendo los
// mismos.
func TestCloneCubreTodoCampoPorReferencia(t *testing.T) {
	m := &Machine{}
	sem := 0
	llenarNoCero(reflect.ValueOf(m).Elem(), &sem)

	c := m.Clone()
	if c == nil {
		t.Fatal("Clone de una máquina no nil devolvió nil")
	}
	if c == m {
		t.Fatal("Clone devolvió el MISMO puntero que el original")
	}

	verificarCopiaProfunda(t, "Machine", reflect.ValueOf(m).Elem(), reflect.ValueOf(c).Elem())
}

// TestCloneDeNilNoPanica: fail() y persist() pueden recibir un puntero nil en
// algún camino de error; Clone tiene que devolver nil sin explotar.
func TestCloneDeNilNoPanica(t *testing.T) {
	var m *Machine
	if got := m.Clone(); got != nil {
		t.Fatalf("Clone(nil) = %v, quería nil", got)
	}
}

// TestCloneNoComparteNadaConElOriginalTrasMutar es el caso concreto de M-03:
// mutar un slice o un mapa del original (o del clon) después de clonar no debe
// verse en el otro. Es más fácil de leer que el test genérico de arriba, y
// deja fijado el caso real que rompía withDriveIDs.
func TestCloneNoComparteNadaConElOriginalTrasMutar(t *testing.T) {
	hace1h := time.Now().Add(-time.Hour)
	m := &Machine{
		ID:           "m1",
		Volumes:      []VolumeAttachment{{Name: "datos", Mount: "/data", DriveID: "vdc"}},
		Shares:       []ShareAttachment{{Mode: "ro", Mount: "/host"}},
		AllowDomains: []string{"a.example.com"},
		Forwards:     map[string]string{"8080": "127.0.0.1:1"},
		Labels:       map[string]string{"service": "web"},
		StartedAt:    &hace1h,
		Wake:         &WakePhases{Tier: "frozen", TotalMS: 30},
	}

	c := m.Clone()

	// Mutar el original tras clonar no debe cambiar el clon. hace1h se guarda
	// antes: m.StartedAt sigue apuntando a esa misma variable, así que mutar
	// *m.StartedAt más abajo también la cambiaría a ella.
	quieroStartedAt := hace1h
	m.Volumes[0].DriveID = "vdz"
	m.AllowDomains[0] = "otro.example.com"
	m.Forwards["8080"] = "127.0.0.1:2"
	m.Labels["service"] = "otro"
	*m.StartedAt = time.Now()
	m.Wake.Tier = "paused"

	if c.Volumes[0].DriveID != "vdc" {
		t.Errorf("Volumes: la mutación del original se filtró al clon: %q", c.Volumes[0].DriveID)
	}
	if c.AllowDomains[0] != "a.example.com" {
		t.Errorf("AllowDomains: la mutación del original se filtró al clon: %q", c.AllowDomains[0])
	}
	if c.Forwards["8080"] != "127.0.0.1:1" {
		t.Errorf("Forwards: la mutación del original se filtró al clon: %q", c.Forwards["8080"])
	}
	if c.Labels["service"] != "web" {
		t.Errorf("Labels: la mutación del original se filtró al clon: %q", c.Labels["service"])
	}
	if !c.StartedAt.Equal(quieroStartedAt) {
		t.Errorf("StartedAt: la mutación del original se filtró al clon: %v", c.StartedAt)
	}
	if c.Wake.Tier != "frozen" {
		t.Errorf("Wake: la mutación del original se filtró al clon: %q", c.Wake.Tier)
	}

	// Y al revés: mutar el clon no debe tocar al original.
	c.Labels["service"] = "clon"
	if m.Labels["service"] != "otro" {
		t.Errorf("Labels: la mutación del clon se filtró al original: %q", m.Labels["service"])
	}
}
