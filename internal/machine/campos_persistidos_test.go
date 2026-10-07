package machine

// Guarda de los structs persistidos (docs/actualizar.md §5, PR 6).
//
// Lo que kindling deja en disco se escribe tal cual desde unos pocos structs:
// api.Machine en state.json, api.Snapshot en meta.json y credproxy.Credential
// y api.CredentialSpec cifrados en credentials.enc. Cambiar uno de ellos es
// cambiar el formato de un fichero que ya existe en cada host, y quien lo
// cambia no siempre lo ve: renombrar una etiqueta JSON compila, pasa los
// tests y, al actualizar, deja ese campo vacío en todas las máquinas.
//
// Este test compara los campos de esos structs (nombre en el JSON y tipo, y
// los de los structs del módulo que cuelgan de ellos) con la lista comiteada
// en testdata/esquema/campos-persistidos.txt, que también apunta la versión de
// cada fichero:
//
//   - Si un campo desaparece o cambia de tipo, el fichero viejo se leería mal:
//     hay que subir su versión (y escribir la migración) antes de poder
//     regenerar la lista; -update se niega mientras no se haya hecho.
//   - Si solo se añaden campos, la decisión es de quien los añade: si un kling
//     anterior puede ignorarlos, basta con regenerar la lista; si no, hay que
//     subir la versión igual. Regenerarla es dejar constancia de que se miró.
//
// Para regenerarla: go test ./internal/machine -run TestCamposPersistidos -update

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

var actualizarCampos = flag.Bool("update", false, "regenerate testdata/esquema/campos-persistidos.txt")

// ficheroPersistido es un fichero de disco: los structs que se escriben en él
// y la constante con su versión, que es lo que hay que subir.
type ficheroPersistido struct {
	nombre    string
	version   int
	constante string // dónde se sube, para el mensaje
	raices    []reflect.Type
}

func ficherosPersistidos() []ficheroPersistido {
	return []ficheroPersistido{
		// versionEstadoMax y no versionEstado: un formato nuevo tiene que subir
		// lo que este binario sabe leer, sea cual sea el que escribe por defecto.
		{"state.json", versionEstadoMax, "versionEstado/versionEstadoMax (internal/machine/manager.go)",
			[]reflect.Type{reflect.TypeOf(api.Machine{})}},
		{"meta.json", metaSchema, "metaSchema (internal/machine/meta.go)",
			[]reflect.Type{reflect.TypeOf(api.Snapshot{})}},
		{"credentials.enc", versionAlmacen, "versionAlmacen (internal/machine/credenciales.go)",
			[]reflect.Type{reflect.TypeOf(credproxy.Credential{}), reflect.TypeOf(api.CredentialSpec{})}},
	}
}

const rutaCampos = "testdata/esquema/campos-persistidos.txt"

// camposDe lista "<fichero> <tipo> <campo json> <tipo go>" de cada campo que
// encoding/json escribe de t y de los structs del módulo a los que llega
// (por puntero, slice, array o mapa). Cada struct sale una vez por fichero.
func camposDe(fichero string, t reflect.Type, vistos map[reflect.Type]bool, out *[]string) {
	t = structDe(t)
	if t == nil || vistos[t] {
		return
	}
	vistos[t] = true
	var hijos []reflect.Type
	var recorrer func(s reflect.Type)
	recorrer = func(s reflect.Type) {
		for i := 0; i < s.NumField(); i++ {
			f := s.Field(i)
			nombre, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if nombre == "-" {
				continue
			}
			// Un struct incrustado sin nombre en el JSON aplana sus campos.
			if f.Anonymous && nombre == "" && f.Type.Kind() == reflect.Struct {
				recorrer(f.Type)
				continue
			}
			if !f.IsExported() {
				continue
			}
			if nombre == "" {
				nombre = f.Name
			}
			*out = append(*out, fmt.Sprintf("%s %s %s %s", fichero, t.String(), nombre, f.Type.String()))
			hijos = append(hijos, f.Type)
		}
	}
	recorrer(t)
	for _, h := range hijos {
		camposDe(fichero, h, vistos, out)
	}
}

// structDe devuelve el struct del módulo que hay detrás de t, o nil. Los de
// fuera (time.Time) se serializan a su manera y no son de este repo.
func structDe(t reflect.Type) reflect.Type {
	for {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
			continue
		case reflect.Struct:
			if strings.HasPrefix(t.PkgPath(), "github.com/juan52878911/kindling/") {
				return t
			}
		}
		return nil
	}
}

// listaCampos es el contenido de campos-persistidos.txt para este binario.
func listaCampos() []byte {
	var b bytes.Buffer
	b.WriteString("# Campos que kindling persiste en disco. Generado por TestCamposPersistidos\n")
	b.WriteString("# (internal/machine/campos_persistidos_test.go): léelo antes de tocarlo.\n")
	for _, f := range ficherosPersistidos() {
		fmt.Fprintf(&b, "version %s %d\n", f.nombre, f.version)
	}
	for _, f := range ficherosPersistidos() {
		var campos []string
		vistos := map[reflect.Type]bool{}
		for _, r := range f.raices {
			camposDe(f.nombre, r, vistos, &campos)
		}
		sort.Strings(campos)
		for _, c := range campos {
			b.WriteString(c + "\n")
		}
	}
	return b.Bytes()
}

// listaLeida es campos-persistidos.txt ya interpretado: la versión de cada
// fichero y, por "<fichero> <tipo> <campo>", su tipo Go.
type listaLeida struct {
	versiones map[string]int
	campos    map[string]string
}

func leerListaCampos(b []byte) (listaLeida, error) {
	l := listaLeida{versiones: map[string]int{}, campos: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		linea := strings.TrimSpace(sc.Text())
		if linea == "" || strings.HasPrefix(linea, "#") {
			continue
		}
		p := strings.Fields(linea)
		switch {
		case len(p) == 3 && p[0] == "version":
			var v int
			if _, err := fmt.Sscanf(p[2], "%d", &v); err != nil {
				return l, fmt.Errorf("line %d: %q", n, linea)
			}
			l.versiones[p[1]] = v
		case len(p) == 4:
			l.campos[strings.Join(p[:3], " ")] = p[3]
		default:
			return l, fmt.Errorf("line %d: %q", n, linea)
		}
	}
	return l, sc.Err()
}

// cambiosCampos compara la lista comiteada con la de ahora. rotos son los
// campos que desaparecen o cambian de tipo en un fichero cuya versión no ha
// subido (lo que impide regenerar); otros, lo demás que difiere.
func cambiosCampos(antes, ahora listaLeida) (rotos, otros []string) {
	subio := func(fichero string) bool { return ahora.versiones[fichero] > antes.versiones[fichero] }
	for clave, tipo := range antes.campos {
		fichero, _, _ := strings.Cut(clave, " ")
		nuevo, ok := ahora.campos[clave]
		var cambio string
		switch {
		case !ok:
			cambio = fmt.Sprintf("removed: %s %s", clave, tipo)
		case nuevo != tipo:
			cambio = fmt.Sprintf("type changed: %s %s -> %s", clave, tipo, nuevo)
		default:
			continue
		}
		if subio(fichero) {
			otros = append(otros, cambio)
		} else {
			rotos = append(rotos, cambio)
		}
	}
	for clave, tipo := range ahora.campos {
		if _, ok := antes.campos[clave]; !ok {
			otros = append(otros, fmt.Sprintf("added: %s %s", clave, tipo))
		}
	}
	for f, v := range ahora.versiones {
		if antes.versiones[f] != v {
			otros = append(otros, fmt.Sprintf("version: %s %d -> %d", f, antes.versiones[f], v))
		}
	}
	sort.Strings(rotos)
	sort.Strings(otros)
	return rotos, otros
}

func TestCamposPersistidos(t *testing.T) {
	ahoraB := listaCampos()
	ahora, err := leerListaCampos(ahoraB)
	if err != nil {
		t.Fatal(err)
	}
	antesB, err := os.ReadFile(filepath.FromSlash(rutaCampos))
	if os.IsNotExist(err) && *actualizarCampos {
		escribirListaCampos(t, ahoraB)
		return
	}
	if err != nil {
		t.Fatalf("%s: %v (run with -update to create it)", rutaCampos, err)
	}
	antes, err := leerListaCampos(antesB)
	if err != nil {
		t.Fatalf("%s: %v", rutaCampos, err)
	}
	rotos, otros := cambiosCampos(antes, ahora)
	if len(rotos) > 0 {
		var constantes []string
		for _, f := range ficherosPersistidos() {
			for _, r := range rotos {
				if strings.Contains(r, ": "+f.nombre+" ") {
					constantes = append(constantes, f.constante)
					break
				}
			}
		}
		t.Fatalf("a struct persisted on disk lost or retyped fields without a schema bump:\n  %s\n\n"+
			"Files already on every host have those fields, and this binary would read them wrong. "+
			"Bump %s, migrate the old version where the file is decoded (see docs/actualizar.md and pkg/esquema), "+
			"add a fixture of the old format under testdata/esquema, and only then run\n"+
			"  go test ./internal/machine -run TestCamposPersistidos -update\n"+
			"(-update refuses until the version is bumped.)",
			strings.Join(rotos, "\n  "), strings.Join(constantes, ", "))
	}
	if len(otros) == 0 {
		if !bytes.Equal(antesB, ahoraB) && *actualizarCampos {
			escribirListaCampos(t, ahoraB) // solo forma (orden, comentarios)
		}
		return
	}
	if *actualizarCampos {
		escribirListaCampos(t, ahoraB)
		return
	}
	t.Fatalf("the structs persisted on disk changed:\n  %s\n\n"+
		"If an older kling can read and rewrite the file without misreading it or losing something that "+
		"matters (typically a new omitempty field it simply ignores), record it with\n"+
		"  go test ./internal/machine -run TestCamposPersistidos -update\n"+
		"Otherwise bump the file's schema version and add the migration first (docs/actualizar.md).",
		strings.Join(otros, "\n  "))
}

func escribirListaCampos(t *testing.T, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.FromSlash(rutaCampos), b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", rutaCampos)
}

// La guarda en sí: quitar o cambiar un campo sin subir la versión no se puede
// regenerar; con la versión subida, o si solo se añade, sí.
func TestCambiosCamposExigeSubirVersion(t *testing.T) {
	antes := listaLeida{
		versiones: map[string]int{"state.json": 1},
		campos:    map[string]string{"state.json api.Machine id": "string", "state.json api.Machine ip": "string"},
	}
	copia := func(v int, campos map[string]string) listaLeida {
		return listaLeida{versiones: map[string]int{"state.json": v}, campos: campos}
	}

	quitado := copia(1, map[string]string{"state.json api.Machine id": "string"})
	if rotos, _ := cambiosCampos(antes, quitado); len(rotos) != 1 || !strings.Contains(rotos[0], "removed") {
		t.Errorf("quitar un campo sin subir la versión: rotos = %v", rotos)
	}
	retipado := copia(1, map[string]string{"state.json api.Machine id": "string", "state.json api.Machine ip": "int"})
	if rotos, _ := cambiosCampos(antes, retipado); len(rotos) != 1 || !strings.Contains(rotos[0], "type changed") {
		t.Errorf("cambiar el tipo sin subir la versión: rotos = %v", rotos)
	}
	subido := copia(2, map[string]string{"state.json api.Machine id": "string"})
	if rotos, otros := cambiosCampos(antes, subido); len(rotos) != 0 || len(otros) != 2 {
		t.Errorf("con la versión subida: rotos = %v, otros = %v", rotos, otros)
	}
	anadido := copia(1, map[string]string{"state.json api.Machine id": "string", "state.json api.Machine ip": "string",
		"state.json api.Machine nuevo": "bool"})
	if rotos, otros := cambiosCampos(antes, anadido); len(rotos) != 0 || len(otros) != 1 {
		t.Errorf("añadir un campo: rotos = %v, otros = %v", rotos, otros)
	}
}

// La lista sigue a encoding/json: etiqueta o nombre Go, "-" fuera, los
// incrustados aplanados y los structs anidados del módulo una vez.
func TestCamposDeSigueAJSON(t *testing.T) {
	type hijo struct {
		A int `json:"a"`
	}
	type incrustado struct {
		B string `json:"b"`
	}
	type raiz struct {
		incrustado
		Sin    string
		Fuera  string `json:"-"`
		Hijos  []hijo `json:"hijos,omitempty"`
		Otro   *hijo  `json:"otro"`
		oculto int
	}
	var campos []string
	camposDe("f", reflect.TypeOf(raiz{}), map[reflect.Type]bool{}, &campos)
	sort.Strings(campos)
	quiero := []string{
		"f machine.hijo a int",
		"f machine.raiz Sin string",
		"f machine.raiz b string",
		"f machine.raiz hijos []machine.hijo",
		"f machine.raiz otro *machine.hijo",
	}
	if !reflect.DeepEqual(campos, quiero) {
		got, _ := json.MarshalIndent(campos, "", "  ")
		t.Fatalf("campos = %s", got)
	}
}
