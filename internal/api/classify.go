package api

import "strings"

// EL CRITERIO.
//
// En kindling una microVM efímera muere con TODO lo suyo: memoria y disco. Así
// que la pregunta no es "¿el servidor guarda estado?" sino otra más concreta:
//
//	¿algo que escribe una llamada tiene que verlo una llamada posterior?
//
// Si la respuesta es sí, el servicio no puede ser efímero. Y eso incluye casos
// que no parecen "con estado": un servidor de ficheros escribe en el disco del
// invitado, que es tan volátil como su memoria.
//
// La señal fiable es ESTRUCTURAL: que el servidor exponga a la vez herramientas
// que escriben y herramientas que leen. Quien escribe espera que alguien lea
// después, y si nadie puede, el servidor no sirve para nada.
//
// Se probó también buscar palabras como "session" o "sequence" en las
// descripciones, y clasificaba TODO como persistente: esas palabras aparecen de
// pasada en cualquier texto ("a sequence of edits"). Las pistas por palabra solo
// se miran en el NOMBRE de la herramienta, que es donde significan algo.

var writeVerbs = []string{
	"create", "add", "write", "save", "store", "set", "put", "update", "insert",
	"append", "edit", "delete", "remove", "move", "rename", "record",
	"crear", "guardar", "escribir", "anadir", "añadir", "borrar", "actualizar",
}

var readVerbs = []string{
	"read", "get", "list", "search", "find", "query", "show", "fetch",
	"open", "tree", "info", "view", "leer", "listar", "buscar", "consultar", "ver",
}

// stateNames son palabras que, EN EL NOMBRE de una herramienta, delatan a un
// servidor que acumula contexto. Sirven para servidores de una sola herramienta,
// donde la señal de escribir-y-leer no puede aparecer.
var stateNames = []string{
	"memory", "memoria", "remember", "recuerda", "graph", "grafo",
	"history", "historial", "sequential", "secuencial", "thinking", "thought",
	"context", "contexto", "accumulate", "chain",
}

// StatefulVerdict explica por qué un servicio puede o no ser efímero.
type StatefulVerdict struct {
	Stateful bool
	Reason   string
}

func matchesAny(hay string, needles []string) string {
	for _, n := range needles {
		if strings.Contains(hay, n) {
			return n
		}
	}
	return ""
}

// ClassifyTools decide si un servicio puede correr en máquinas efímeras.
//
// Ante la duda gana lo conservador: marcar como efímero algo que no lo es
// produce pérdida SILENCIOSA de datos —la llamada responde bien y lo escrito
// desaparece—, mientras que marcar como persistente algo que no lo necesitaba
// solo cuesta una instancia congelada, que no gasta ni CPU ni RAM.
func ClassifyTools(tools []ToolSpec) StatefulVerdict {
	if len(tools) == 0 {
		return StatefulVerdict{false, "no tools to analyze"}
	}

	var writers, readers []string
	for _, t := range tools {
		name := strings.ToLower(t.Name)

		if w := matchesAny(name, stateNames); w != "" {
			return StatefulVerdict{true,
				t.Name + " suggests it accumulates context"}
		}
		if matchesAny(name, writeVerbs) != "" {
			writers = append(writers, t.Name)
		}
		if matchesAny(name, readVerbs) != "" {
			readers = append(readers, t.Name)
		}
	}

	switch {
	case len(writers) > 0 && len(readers) > 0:
		return StatefulVerdict{true,
			"it writes with " + writers[0] + " and reads with " + readers[0] +
				": what it writes must outlive the call"}
	case len(writers) > 0:
		return StatefulVerdict{false,
			"it only writes (" + writers[0] + "): its effect leaves the microVM"}
	default:
		return StatefulVerdict{false, "it only queries: nothing to preserve"}
	}
}
