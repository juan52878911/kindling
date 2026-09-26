package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// MEDICIÓN DE LOS CAMINOS CALIENTES DEL GATEWAY.
//
// Aquí no se comprueba nada: se mide. El valor está en poder repetir la medida
// tras un cambio y ver si el coste se movió, porque find_tools y la reparación
// de tipos se ejecutan en CADA petición del modelo y no hay ningún sitio donde
// una regresión aquí se disimule.
//
// Nada de esto toca KVM, red ni el daemon: corre en CI como un test normal.

// benchCatalog fabrica un catálogo con la forma del real: nombres cualificados
// "servicio.herramienta" y descripciones en inglés, que es como las publican los
// servidores MCP. Se construye con newTool a propósito, para que el haystack
// salga idéntico al de producción y la medida no mienta.
func benchCatalog(n int) []Tool {
	services := []string{"filesystem", "memory", "sequential-thinking", "git", "fetch"}
	names := []string{
		"read_file", "write_file", "list_directory", "search_files", "move_file",
		"get_file_info", "create_entities", "read_graph", "search_nodes",
	}
	descs := []string{
		"Read the complete contents of a file from the file system",
		"Create a new file or completely overwrite an existing file with new content",
		"Get a detailed listing of all files and directories in a specified path",
		"Recursively search for files and directories matching a pattern",
		"Move or rename files and directories between locations",
		"Retrieve detailed metadata about a file or directory, including size and permissions",
		"Create multiple new entities in the knowledge graph",
		"Read the entire knowledge graph with all its nodes and relations",
		"Search for nodes in the knowledge graph matching an open-ended query",
	}
	out := make([]Tool, 0, n)
	for i := range n {
		j := i % len(names)
		// El sufijo evita que mil herramientas sean nueve repetidas: en un
		// catálogo real los nombres cualificados son todos distintos, y con
		// cadenas idénticas el reparto de aciertos dejaría de ser representativo.
		name := fmt.Sprintf("%s_%d", names[j], i/len(names))
		out = append(out, newTool(services[i%len(services)], name, descs[j], nil))
	}
	return out
}

// La consulta típica: el usuario pregunta en español y las herramientas están
// descritas en inglés, que es justo el caso para el que existe expandTerms.
const benchQuery = "leer un fichero de texto"

// Sumidero de resultados. Evita que el compilador se lleve por delante el
// trabajo del bucle al ver que nadie mira lo que devuelve.
var benchSink int

// BenchmarkFindToolsBuscar mide el barrido de find_tools con el haystack
// precomputado: por cada herramienta del catálogo y cada término expandido, un
// strings.Contains y nada más.
//
// Se mide SOLO el barrido. La ordenación y el formateo del resultado quedan
// fuera para que la comparación con la variante sin haystack aísle exactamente
// lo que cambia entre las dos.
func BenchmarkFindToolsBuscar(b *testing.B) {
	terms := expandTerms(benchQuery)
	for _, n := range []int{10, 100, 1000} {
		tools := benchCatalog(n)
		b.Run(fmt.Sprintf("herramientas=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			hits := 0
			for b.Loop() {
				hits = 0
				for _, t := range tools {
					for _, term := range terms {
						if strings.Contains(t.haystack, term) {
							hits++
						}
					}
				}
			}
			benchSink = hits
		})
	}
}

// BenchmarkFindToolsBuscarSinHaystack es la misma búsqueda como estaba antes de
// precomputar el campo: la cadena a buscar se arma dentro del bucle.
//
// Existe para poder defender la optimización con números en vez de con
// intuición. La diferencia no es el ToLower en sí, sino que se paga una
// concatenación y una minúscula POR HERRAMIENTA Y POR BÚSQUEDA, cuando el
// resultado es inmutable desde que se conoce la herramienta.
func BenchmarkFindToolsBuscarSinHaystack(b *testing.B) {
	terms := expandTerms(benchQuery)
	for _, n := range []int{10, 100, 1000} {
		tools := benchCatalog(n)
		b.Run(fmt.Sprintf("herramientas=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			hits := 0
			for b.Loop() {
				hits = 0
				for _, t := range tools {
					hay := strings.ToLower(t.Qualified + " " + t.Description)
					for _, term := range terms {
						if strings.Contains(hay, term) {
							hits++
						}
					}
				}
			}
			benchSink = hits
		})
	}
}

// BenchmarkExpandTerms mide la expansión de sinónimos, que se paga una vez por
// búsqueda. Es el término independiente del coste de find_tools: si creciera
// mucho, dejaría de dar igual frente al barrido del catálogo pequeño.
func BenchmarkExpandTerms(b *testing.B) {
	b.ReportAllocs()
	var out []string
	for b.Loop() {
		out = expandTerms(benchQuery)
	}
	benchSink = len(out)
}

// ── reparación de tipos ───────────────────────────────────────────────────────

// Sumidero para las variantes de coerce.
var benchArgs json.RawMessage

// BenchmarkCoerceArgs mide la reparación completa tal como la ve un tools/call:
// deserializar argumentos y esquema, arreglar y volver a serializar.
//
// Se paga en TODAS las llamadas a herramienta, también en las que no hay nada
// que arreglar — por eso el caso "sano" se mide aparte: es el que más se
// ejecuta y el que no debería costar.
func BenchmarkCoerceArgs(b *testing.B) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string"},
			"head":{"type":"integer"},
			"dryRun":{"type":"boolean"},
			"paths":{"type":"array","items":{"type":"string"}},
			"edits":{"type":"array","items":{"type":"object","properties":{
				"oldText":{"type":"string"},"newText":{"type":"string"}}}}
		}
	}`)

	cases := []struct {
		name string
		args json.RawMessage
	}{
		{"sano", json.RawMessage(
			`{"path":"/tmp/a.txt","head":5,"dryRun":false,"paths":["/a","/b"],` +
				`"edits":[{"oldText":"x","newText":"y"}]}`)},
		{"array-como-objeto-indexado", json.RawMessage(
			`{"path":"/tmp/a.txt","paths":{"0":"/a","1":"/b","2":"/c"}}`)},
		{"array-en-string-json", json.RawMessage(
			`{"path":"/tmp/a.txt","edits":"[{\"oldText\":\"x\",\"newText\":\"y\"}]"}`)},
		{"escalares-como-string", json.RawMessage(
			`{"path":"/tmp/a.txt","head":"5","dryRun":"true"}`)},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			var out json.RawMessage
			for b.Loop() {
				out = coerceArgs(c.args, schema)
			}
			benchArgs = out
		})
	}
}

// BenchmarkCoerce mide solo el arreglo, sobre valores ya deserializados.
//
// Sirve para saber a quién culpar: si coerceArgs sale caro y esto sale barato,
// el coste está en el JSON de ida y vuelta y no en el recorrido del esquema.
func BenchmarkCoerce(b *testing.B) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(
		`{"type":"object","properties":{
			"path":{"type":"string"},
			"head":{"type":"integer"}}}`), &schema); err != nil {
		b.Fatal(err)
	}
	// Daño de objeto y escalares, sin arrays: coerceObject devuelve un mapa
	// nuevo y coerceNumber no toca la entrada, así que el mismo valor se puede
	// reusar en todas las vueltas y se mide siempre el mismo trabajo. Con un
	// array por medio no valdría: coerceArray reescribe la rebanada en sitio y
	// la segunda vuelta ya encontraría el valor reparado.
	var v any
	if err := json.Unmarshal([]byte(`{"path":"/tmp/a.txt","head":"5"}`), &v); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	var out any
	for b.Loop() {
		out = coerce(v, schema)
	}
	_ = out
}
