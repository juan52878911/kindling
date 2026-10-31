package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// doc une líneas con su salto final: así se ven los espacios del final y
// los tabuladores, que en un literal crudo se pierden de vista.
func doc(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

// Escalares de bloque con los ejemplos del capítulo 8 de YAML 1.2.2 que
// entran en el subconjunto (sin etiquetas, anclas ni claves ?), y los casos
// de docker-compose que los motivaron.
func TestParseYAMLBloques(t *testing.T) {
	casos := []struct {
		nombre string
		doc    string
		want   string // JSON
	}{
		{"8.1 cabecera", doc(
			"- | # Empty header",
			" literal",
			"- >1 # Indentation indicator",
			"  folded",
			"- |+ # Chomping indicator",
			" keep",
			"",
			"- >1- # Both indicators",
			"  strip"),
			`["literal\n"," folded\n","keep\n\n"," strip"]`},
		{"8.2 sangría", doc(
			"- |",
			" detected",
			"- >",
			" ",
			"  ",
			"  # detected",
			"- |1",
			"  explicit",
			"- >",
			" \t",
			" detected"),
			`["detected\n","\n\n# detected\n"," explicit\n","\t\ndetected\n"]`},
		{"8.4 chomping", doc(
			"strip: |-",
			"  text",
			"clip: |",
			"  text",
			"keep: |+",
			"  text"),
			`{"clip":"text\n","keep":"text\n","strip":"text"}`},
		// La gramática (l-keep-empty) conserva la vacía antes del comentario
		// en keep; así lo da también la yaml-test-suite (F8F9).
		{"8.5 vacías y comentarios finales", doc(
			" # Strip",
			"  # Comments:",
			"strip: |-",
			"  # text",
			"  ",
			" # Clip",
			"  # comments:",
			"",
			"clip: |",
			"  # text",
			" ",
			" # Keep",
			"  # comments:",
			"",
			"keep: |+",
			"  # text",
			"",
			" # Trail",
			"  # comments."),
			`{"clip":"# text\n","keep":"# text\n\n","strip":"# text"}`},
		{"8.6 vacío", doc(
			"strip: >-",
			"",
			"clip: >",
			"",
			"keep: |+",
			""),
			`{"clip":"","keep":"\n","strip":""}`},
		{"8.7 literal", doc(
			"|",
			" literal",
			" \ttext",
			""),
			`"literal\n\ttext\n"`},
		{"8.8 contenido literal", doc(
			"|",
			" ",
			"  ",
			"  literal",
			"   ",
			"  ",
			"  text",
			"",
			" # Comment"),
			`"\n\nliteral\n \n\ntext\n"`},
		{"8.9 plegado", doc(
			">",
			" folded",
			" text",
			""),
			`"folded text\n"`},
		// 8.10-8.13 son el mismo documento: plegado, líneas más sangradas que
		// conservan sus saltos y vacías finales.
		{"8.10 líneas plegadas", doc(
			">",
			"",
			" folded",
			" line",
			"",
			" next",
			" line",
			"   * bullet",
			"",
			"   * list",
			"   * lines",
			"",
			" last",
			" line",
			"",
			"# Comment"),
			`"\nfolded line\nnext line\n  * bullet\n\n  * list\n  * lines\n\nlast line\n"`},
		{"8.15 elementos de lista", doc(
			"- # Empty",
			"- |",
			" block node",
			"- - one # Compact",
			"  - two # sequence",
			"- one: two # Compact mapping"),
			`[null,"block node\n",["one","two"],{"one":"two"}]`},
		// 8.20 y 8.21 sin las etiquetas (!!map, !foo), que no entran; de 8.20
		// falta también el escalar suelto en la línea de debajo del guion.
		{"8.20 tipos de nodo", doc(
			"- \"flow in block\"",
			"- >",
			" Block scalar"),
			`["flow in block","Block scalar\n"]`},
		{"8.21 indicador explícito", doc(
			"literal: |2",
			"  value",
			"folded: >1",
			" value"),
			`{"folded":"value\n","literal":"value\n"}`},

		// El caso de AuraCRM: las comillas y el ; del bloque son texto, y la
		// interpolación de compose ($$) no es cosa del parser.
		{"compose entrypoint plegado", doc(
			"services:",
			"  createbuckets:",
			"    image: minio/mc",
			"    entrypoint: >",
			"      /bin/sh -c \"",
			"      sleep 5;",
			"      mc alias set myminio http://minio:9000 $${MINIO_ROOT_USER:-minioadmin} $${MINIO_ROOT_PASSWORD:-minioadmin};",
			"      exit 0;",
			"      \"",
			"    restart: \"no\""),
			`{"services":{"createbuckets":{"entrypoint":"/bin/sh -c \" sleep 5; mc alias set myminio http://minio:9000 $${MINIO_ROOT_USER:-minioadmin} $${MINIO_ROOT_PASSWORD:-minioadmin}; exit 0; \"\n","image":"minio/mc","restart":"no"}}}`},
		{"literal con # y comillas sueltas", doc(
			"a: |",
			"  echo 'hola # no es comentario",
			"  \"",
			"b: 1"),
			`{"a":"echo 'hola # no es comentario\n\"\n","b":1}`},
		{"bloque en mapa dentro de lista", doc(
			"steps:",
			"  - run: |",
			"      make",
			"      make test",
			"    name: build",
			"  - |-",
			"    suelto",
			"tail: x"),
			`{"steps":[{"name":"build","run":"make\nmake test\n"},"suelto"],"tail":"x"}`},
		{"fin de fichero sin salto", "a: |\n  x\n  y",
			`{"a":"x\ny"}`},
		{"keep al final sin salto", "a: |+\n  x\n\n  ",
			`{"a":"x\n\n"}`},
		{"bloque vacío seguido del padre", doc(
			"a: >",
			"b: |",
			"   ",
			"c: 1"),
			`{"a":"","b":"","c":1}`},
		{"más sangradas en plegado", doc(
			"a: >-",
			"  uno",
			"    dos",
			"  tres",
			"  cuatro"),
			`{"a":"uno\n  dos\ntres cuatro"}`},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			v, err := parseYAML([]byte(c.doc))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(v)
			var got, want any
			_ = json.Unmarshal(b, &got)
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatalf("want no es JSON: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("\n got %s\nwant %s", b, c.want)
			}
		})
	}
}

// 8.3 y otros bloques mal formados son un error con su línea.
func TestParseYAMLBloquesRechaza(t *testing.T) {
	casos := map[string]string{
		"8.3 vacía inicial más sangrada": doc("- |", "  ", " text"),
		"8.3 línea menos sangrada":       doc("- >", "  text", " text"),
		"8.3 indicador mayor":            doc("- |2", " text"),
		"cabecera con texto":             doc("a: | x", "  b"),
		"dos chomping":                   doc("a: |+-", "  b"),
		"indicador cero":                 doc("a: |0", "  b"),
	}
	for nombre, d := range casos {
		if v, err := parseYAML([]byte(d)); err == nil {
			t.Errorf("%s: aceptado como %#v", nombre, v)
		} else if !strings.HasPrefix(err.Error(), "line ") {
			t.Errorf("%s: error sin línea: %v", nombre, err)
		}
	}
}

// Un compose con command: > y entrypoint: | se traduce: el plegado se parte
// como un comando de shell y el literal conserva los saltos de su guion;
// $$ y ${VAR:-x} se resuelven como en cualquier otra cadena.
func TestComposeBloques(t *testing.T) {
	compose := doc(
		"services:",
		"  cache:",
		"    image: redis:7",
		"    command: >",
		"      redis-server",
		"      --appendonly yes",
		"      --maxmemory ${MAXMEM:-64mb}",
		"  init:",
		"    image: minio/mc",
		"    entrypoint: |",
		"      /bin/sh -c \"",
		"      mc alias set m http://minio:9000 $${MINIO_ROOT_USER:-minioadmin};",
		"      exit 0",
		"      \"",
	)
	tr, err := traducirCompose([]byte(compose), "app", false, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tr.por["cache"].cmd, []string{"redis-server", "--appendonly", "yes", "--maxmemory", "64mb"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cache command = %q, want %q", got, want)
	}
	want := []string{"/bin/sh", "-c", "\nmc alias set m http://minio:9000 ${MINIO_ROOT_USER:-minioadmin};\nexit 0\n"}
	if got := tr.por["init"].entry; !reflect.DeepEqual(got, want) {
		t.Errorf("init entrypoint = %q, want %q", got, want)
	}
	// El grafo emitido (con el import multilínea en la cabecera) se vuelve a leer.
	if _, err := leerGrafo("app.graph.yaml", []byte(tr.Grafo), nil); err != nil {
		t.Fatalf("el grafo no se lee: %v\n%s", err, tr.Grafo)
	}
}
