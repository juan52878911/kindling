package machine

import (
	"reflect"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// A-11: una copia del fondo ya está restaurada; lo que la petición pida y el
// fondo no sepa aplicar (puentes al host, caudal de red, anidada, un campo
// nuevo de RunRequest) se perdía en silencio si peticionLlana no lo
// nombraba. Se recorre RunRequest entero: cada campo, o el fondo lo aplica
// (o runFrom lo ignora igual), o con él la petición va al camino en frío.
func TestPeticionLlanaRecorreTodosLosCampos(t *testing.T) {
	// Lo que adjudicarDelFondo aplica, lo que runFrom ignora con From y lo
	// que otra capa resuelve después de crear (Publish lo publica el daemon).
	sinCondicion := map[string]bool{
		"Name": true, "From": true, "Labels": true, "TTLSeconds": true, "OnTTL": true,
		"WaitReady": true, "ReadyTimeoutSeconds": true,
		"VCPUs": true, "MemMiB": true, "MemMaxMiB": true, "DiskMiB": true, "Image": true,
		"Publish": true,
	}
	// Los que dependen de la plantilla: se prueban aparte.
	conCondicion := map[string]bool{"Egress": true, "AllowExec": true, "CPUPctDefault": true}

	snap := &api.Snapshot{}
	base := api.RunRequest{From: "plantilla"}
	if !peticionLlana(base, snap) {
		t.Fatal("una petición sin nada más que From no se conforma con el fondo")
	}
	tipo := reflect.TypeOf(base)
	for i := range tipo.NumField() {
		campo := tipo.Field(i)
		if !campo.IsExported() || conCondicion[campo.Name] {
			continue
		}
		req := base
		v := reflect.ValueOf(&req).Elem().Field(i)
		if !llenarCampo(v) {
			t.Fatalf("campo %s (%s): el test no sabe darle un valor; añádelo", campo.Name, campo.Type)
		}
		if got, quiere := peticionLlana(req, snap), sinCondicion[campo.Name]; got != quiere {
			t.Errorf("con %s puesto, peticionLlana = %v; quería %v", campo.Name, got, quiere)
		}
	}

	// Los condicionados.
	casos := []struct {
		nombre string
		req    api.RunRequest
		snap   api.Snapshot
		quiere bool
	}{
		{"egress igual", api.RunRequest{Egress: "open"}, api.Snapshot{Egress: "open"}, true},
		{"egress none sobre vacío", api.RunRequest{Egress: "none"}, api.Snapshot{}, true},
		{"egress distinto", api.RunRequest{Egress: "open"}, api.Snapshot{Egress: "none"}, false},
		{"exec en plantilla con exec", api.RunRequest{AllowExec: true}, api.Snapshot{AllowExec: true}, true},
		{"exec en plantilla sin exec", api.RunRequest{AllowExec: true}, api.Snapshot{}, false},
		{"techo por defecto que la plantilla pisa", api.RunRequest{CPUPctDefault: 50}, api.Snapshot{CPUPct: 80}, true},
		{"techo por defecto sin techo en la plantilla", api.RunRequest{CPUPctDefault: 50}, api.Snapshot{}, false},
		{"dueño explícito", api.RunRequest{Labels: map[string]string{api.LabelOwner: "a"}}, api.Snapshot{}, false},
	}
	for _, c := range casos {
		c.req.From = "plantilla"
		if got := peticionLlana(c.req, &c.snap); got != c.quiere {
			t.Errorf("%s: peticionLlana = %v; quería %v", c.nombre, got, c.quiere)
		}
	}
}

// llenarCampo da a v un valor distinto del cero de su tipo.
func llenarCampo(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(1)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(reflect.ValueOf("k").Convert(v.Type().Key()), reflect.Zero(v.Type().Elem()))
		v.Set(m)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	default:
		return false
	}
	return true
}
