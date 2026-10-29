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
//   - Si se añade un campo, hay que CLASIFICARLO en clasesCampos (abajo):
//     "ignorable" si un kling anterior que lo pierda al reescribir el fichero
//     no cambia el comportamiento de nadie, o "con efecto" si sí, y entonces
//     con la versión del fichero que lo protege, que tiene que ser nueva (un
//     kling anterior la rechaza en vez de perder el campo). Sin clase, o con
//     efecto sin subir la versión, el test falla y -update se niega. Para
//     state.json, además, el escritor tiene que poner esa versión cuando el
//     campo va lleno (TestCamposConEfectoSubenElEsquema).
//
// Los campos que ya estaban antes de las clases (≤ v0.18) no la llevan: se
// miraron uno a uno al añadirlos, con la regla de entonces.
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
		{"credentials.enc", versionAlmacenMax, "versionAlmacen/versionAlmacenMax (internal/machine/credenciales.go)",
			[]reflect.Type{reflect.TypeOf(credproxy.Credential{}), reflect.TypeOf(api.CredentialSpec{})}},
	}
}

const rutaCampos = "testdata/esquema/campos-persistidos.txt"

// claseCampo es qué pasa si un kling que no conoce el campo lo pierde al
// reescribir el fichero.
type claseCampo struct {
	// efecto: perderlo cambia el comportamiento. version es la del fichero
	// que lo protege: la que un kling anterior rechaza.
	efecto  bool
	version int
	// deuda: con efecto, pero llegó sin subir la versión y ya está en todos
	// los hosts; solo vale para campos anteriores a las clases.
	deuda bool
	// porque es la justificación: se lee al revisar el cambio.
	porque string
}

func (c claseCampo) String() string {
	switch {
	case c.deuda:
		return "debt"
	case c.efecto:
		return fmt.Sprintf("effect:%d", c.version)
	}
	return "ignorable"
}

// clasesCampos clasifica cada campo persistido añadido desde v0.19 (la clave
// es "<fichero> <tipo> <campo json>", como en campos-persistidos.txt).
var clasesCampos = map[string]claseCampo{
	// state.json (v0.19/v0.20 los añadieron sin subir el 2; el 3 los protege).
	"state.json api.Machine cmd":         {efecto: true, version: 3, porque: "a Start would run the image's command instead"},
	"state.json api.Machine entrypoint":  {efecto: true, version: 3, porque: "a Start would run the image's entrypoint instead"},
	"state.json api.Machine share_wait":  {efecto: true, version: 3, porque: "the agent would start the service before its shares"},
	"state.json api.Machine volume_wait": {efecto: true, version: 3, porque: "a commit would release and hand back volumes it must not"},
	"state.json api.Machine net_mibps":   {efecto: true, version: 3, porque: "a cold start would use the daemon's rate instead of the requested one"},
	"state.json api.Machine vmm": {porque: "the next launch sets it again; a freeze carries its own in volcado.ok, " +
		"and an older kling records its own VMM in the goldens it makes anyway"},
	"state.json api.Machine published": {porque: "never persisted with the machine: the daemon fills it in responses; publish.json keeps the ports"},
	"state.json api.PortPublish guest": {porque: "only inside published (see above)"},
	"state.json api.PortPublish host":  {porque: "only inside published (see above)"},
	"state.json api.PortPublish proto": {porque: "only inside published (see above)"},
	// meta.json: v0.19 los añadió sin subir el 2. Un kling ≤ v0.18 solo
	// reescribe un meta al anotarlo, y él no los usa: los pierde la vuelta
	// (v0.19+ → v0.18 anota → v0.19+). Subir a 3 haría que v0.19-v0.20, que
	// sí los entienden, rechazaran todos los dorados nuevos.
	"meta.json api.Snapshot share_wait": {deuda: true, porque: "lost only if a kling <= v0.18 annotates the golden"},
	"meta.json api.Snapshot sin_agente": {deuda: true, porque: "lost only if a kling <= v0.18 annotates the golden"},
	"meta.json api.Snapshot arch":       {porque: "an older kling that drops it does not compare architectures, which it never did"},
	// credentials.enc: ver versionAlmacenSigV4 (credenciales.go).
	"credentials.enc credproxy.Credential AccessKeyID": {efecto: true, version: 2, porque: "a kling <= v0.18 would use a sigv4 credential as HTTP and drop it"},
	"credentials.enc credproxy.Credential Region":      {efecto: true, version: 2, porque: "as AccessKeyID"},
	"credentials.enc credproxy.Credential Service":     {efecto: true, version: 2, porque: "as AccessKeyID"},
}

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
			*out = append(*out, fmt.Sprintf("%s %s %s %s", fichero, t.String(), nombre, tipoPersistido(f.Type)))
			hijos = append(hijos, f.Type)
		}
	}
	recorrer(t)
	for _, h := range hijos {
		camposDe(fichero, h, vistos, out)
	}
}

// tipoRaw es json.RawMessage, que desde Go 1.25 con jsonv2 es un alias de
// jsontext.Value: su String() depende del toolchain y la lista no puede.
var tipoRaw = reflect.TypeOf(json.RawMessage(nil))

// tipoPersistido describe t igual en cualquier versión de Go: como
// t.String(), pero con json.RawMessage siempre con ese nombre.
func tipoPersistido(t reflect.Type) string {
	if t == tipoRaw {
		return "json.RawMessage"
	}
	if t.Name() != "" {
		return t.String()
	}
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + tipoPersistido(t.Elem())
	case reflect.Slice:
		return "[]" + tipoPersistido(t.Elem())
	case reflect.Array:
		return fmt.Sprintf("[%d]%s", t.Len(), tipoPersistido(t.Elem()))
	case reflect.Map:
		return "map[" + tipoPersistido(t.Key()) + "]" + tipoPersistido(t.Elem())
	}
	return t.String()
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
			p := strings.Fields(c)
			if cl, ok := clasesCampos[strings.Join(p[:3], " ")]; ok {
				c += " " + cl.String()
			}
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
	clases    map[string]string // la quinta columna, si la hay
}

func leerListaCampos(b []byte) (listaLeida, error) {
	l := listaLeida{versiones: map[string]int{}, campos: map[string]string{}, clases: map[string]string{}}
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
		case len(p) == 5:
			l.campos[strings.Join(p[:3], " ")] = p[3]
			l.clases[strings.Join(p[:3], " ")] = p[4]
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
	rotos = append(rotos, clasesSinCumplir(antes, ahora, clasesCampos)...)
	sort.Strings(rotos)
	sort.Strings(otros)
	return rotos, otros
}

// clasesSinCumplir son los campos de ahora cuya clase falta o no se cumple:
//   - uno nuevo (no está en antes) sin clase en clases;
//   - uno nuevo de deuda (solo vale para los que ya estaban);
//   - uno con efecto, nuevo o que cambia de clase, cuya versión no es mayor
//     que la que ya hay en los hosts (antes) o que no ha llegado (ahora).
func clasesSinCumplir(antes, ahora listaLeida, clases map[string]claseCampo) []string {
	var out []string
	for clave := range ahora.campos {
		fichero, _, _ := strings.Cut(clave, " ")
		_, estaba := antes.campos[clave]
		cl, ok := clases[clave]
		switch {
		case !ok && !estaba:
			out = append(out, fmt.Sprintf("added without a class: %s (classify it in clasesCampos: ignorable, or with effect and a schema bump)", clave))
		case !ok:
		case cl.deuda && !estaba:
			out = append(out, fmt.Sprintf("added as debt: %s (only fields that were already on every host can be debt)", clave))
		case cl.efecto && (!estaba || antes.clases[clave] != cl.String()):
			if cl.version <= antes.versiones[fichero] || ahora.versiones[fichero] < cl.version {
				out = append(out, fmt.Sprintf("field with effect without a schema bump: %s needs %s %d, and hosts already have %d (this binary reads %d)",
					clave, fichero, cl.version, antes.versiones[fichero], ahora.versiones[fichero]))
			}
		}
	}
	return out
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
		t.Fatalf("a struct persisted on disk changed in a way that needs a schema bump (or a field needs its class):\n  %s\n\n"+
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
	// Añadir un campo sin clasificarlo no se puede regenerar.
	anadido := copia(1, map[string]string{"state.json api.Machine id": "string", "state.json api.Machine ip": "string",
		"state.json api.Machine nuevo": "bool"})
	if rotos, otros := cambiosCampos(antes, anadido); len(rotos) != 1 || !strings.Contains(rotos[0], "without a class") || len(otros) != 1 {
		t.Errorf("añadir un campo sin clase: rotos = %v, otros = %v", rotos, otros)
	}
	const nuevo = "state.json api.Machine nuevo"
	casos := []struct {
		nombre  string
		version int
		clase   claseCampo
		roto    string
	}{
		{"ignorable", 1, claseCampo{}, ""},
		{"con efecto sin subir", 1, claseCampo{efecto: true, version: 1}, "without a schema bump"},
		{"con efecto y versión vieja", 2, claseCampo{efecto: true, version: 1}, "without a schema bump"},
		{"con efecto sin llegar", 2, claseCampo{efecto: true, version: 3}, "without a schema bump"},
		{"con efecto y subida", 2, claseCampo{efecto: true, version: 2}, ""},
		{"deuda nueva", 1, claseCampo{deuda: true}, "as debt"},
	}
	for _, c := range casos {
		ahora := copia(c.version, map[string]string{"state.json api.Machine id": "string", "state.json api.Machine ip": "string", nuevo: "bool"})
		rotos := clasesSinCumplir(antes, ahora, map[string]claseCampo{nuevo: c.clase})
		if (c.roto == "") != (len(rotos) == 0) || (c.roto != "" && !strings.Contains(rotos[0], c.roto)) {
			t.Errorf("%s: rotos = %v", c.nombre, rotos)
		}
	}
	// Un campo que ya estaba puede quedar como deuda, o pasar a con efecto
	// solo subiendo la versión.
	ya := copia(1, map[string]string{"state.json api.Machine id": "string", "state.json api.Machine ip": "string"})
	if rotos := clasesSinCumplir(antes, ya, map[string]claseCampo{"state.json api.Machine ip": {deuda: true}}); len(rotos) != 0 {
		t.Errorf("deuda de uno que ya estaba: %v", rotos)
	}
	if rotos := clasesSinCumplir(antes, ya, map[string]claseCampo{"state.json api.Machine ip": {efecto: true, version: 1}}); len(rotos) != 1 {
		t.Errorf("con efecto sin subir uno que ya estaba: %v", rotos)
	}
}

// Para state.json, que escribe el esquema más bajo que describe el estado
// (esquemaParaEscribir), cada campo de api.Machine con efecto tiene que subir
// el esquema cuando va lleno: si no, la clase sería mentira.
func TestCamposConEfectoSubenElEsquema(t *testing.T) {
	tm := reflect.TypeOf(api.Machine{})
	for clave, cl := range clasesCampos {
		p := strings.Fields(clave)
		if p[0] != "state.json" || p[1] != "api.Machine" || !cl.efecto {
			continue
		}
		var mc api.Machine
		v := reflect.ValueOf(&mc).Elem()
		hecho := false
		for i := 0; i < tm.NumField(); i++ {
			nombre, _, _ := strings.Cut(tm.Field(i).Tag.Get("json"), ",")
			if nombre != p[2] {
				continue
			}
			llenar(t, v.Field(i))
			hecho = true
		}
		if !hecho {
			t.Fatalf("%s: no está en api.Machine", clave)
		}
		if got := esquemaParaEscribir([]api.Machine{mc}); got < cl.version {
			t.Errorf("%s lleno: esquemaParaEscribir = %d, y su clase pide %d (añádelo a necesitaV3)", clave, got, cl.version)
		}
	}
	if got := esquemaParaEscribir([]api.Machine{{ID: "x", VMM: "kling-hv 0.20.0"}}); got != versionEstado {
		t.Errorf("una máquina sin campos con efecto: esquema %d, quería %d", got, versionEstado)
	}
}

// llenar pone un valor no vacío en v (lo que omitempty sí escribe).
func llenar(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(1)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(reflect.Zero(v.Type().Key()), reflect.Zero(v.Type().Elem()))
		v.Set(m)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	default:
		t.Fatalf("llenar: tipo %s sin caso", v.Type())
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
		oculto int    //lint:ignore U1000 el campo sin exportar es lo que se prueba que no cuenta
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

// La lista tiene que salir igual con cualquier Go: con jsonv2 (Go 1.25+ por
// defecto) json.RawMessage es jsontext.Value, y la CI usa otra versión.
func TestTipoPersistidoIndependienteDelToolchain(t *testing.T) {
	casos := map[reflect.Type]string{
		reflect.TypeOf(json.RawMessage(nil)):         "json.RawMessage",
		reflect.TypeOf(map[string]json.RawMessage{}): "map[string]json.RawMessage",
		reflect.TypeOf([]*json.RawMessage{}):         "[]*json.RawMessage",
		reflect.TypeOf([2]json.RawMessage{}):         "[2]json.RawMessage",
		reflect.TypeOf(map[string][]string{}):        "map[string][]string",
		reflect.TypeOf((*api.Snapshot)(nil)):         "*api.Snapshot",
	}
	for tipo, quiero := range casos {
		if got := tipoPersistido(tipo); got != quiero {
			t.Errorf("tipoPersistido(%v) = %q, want %q", tipo, got, quiero)
		}
	}
	if bytes.Contains(listaCampos(), []byte("jsontext")) {
		t.Errorf("the persisted field list depends on the Go toolchain (jsontext)")
	}
}
