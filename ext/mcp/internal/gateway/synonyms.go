package gateway

import "strings"

// El modelo pregunta en el idioma del usuario y las herramientas se describen
// casi siempre en inglés. Buscar "leer un fichero de texto" contra
// "Read the complete contents of a file" no casa ni un término, así que
// find_tools devolvía cualquier cosa.
//
// Una tabla pequeña de sinónimos del dominio arregla el 90% de los casos sin
// meter un motor de búsqueda. No pretende ser exhaustiva: cubre los verbos y
// sustantivos con los que se pide una herramienta.
var synonyms = map[string][]string{
	"leer":        {"read", "get", "open", "cat"},
	"lee":         {"read", "get"},
	"escribir":    {"write", "save", "put"},
	"escribe":     {"write", "save"},
	"guardar":     {"save", "write", "store", "remember"},
	"guarda":      {"save", "store"},
	"crear":       {"create", "make", "new", "add"},
	"crea":        {"create", "add"},
	"borrar":      {"delete", "remove", "rm"},
	"eliminar":    {"delete", "remove"},
	"buscar":      {"search", "find", "query", "grep"},
	"busca":       {"search", "find"},
	"listar":      {"list", "ls", "directory"},
	"lista":       {"list", "directory"},
	"mover":       {"move", "rename"},
	"renombrar":   {"rename", "move"},
	"editar":      {"edit", "modify", "change", "patch"},
	"modificar":   {"edit", "modify", "update"},
	"actualizar":  {"update", "edit"},
	"fichero":     {"file"},
	"ficheros":    {"file", "files", "multiple"},
	"archivo":     {"file"},
	"archivos":    {"file", "files", "multiple"},
	"carpeta":     {"directory", "folder", "dir"},
	"directorio":  {"directory", "dir", "folder"},
	"texto":       {"text", "content", "contents"},
	"contenido":   {"content", "contents", "text"},
	"ruta":        {"path"},
	"tamaño":      {"size", "sizes"},
	"memoria":     {"memory", "remember", "recall"},
	"recordar":    {"remember", "memory", "save"},
	"recuerda":    {"remember", "memory"},
	"nota":        {"note", "memo"},
	"notas":       {"note", "notes"},
	"entidad":     {"entity", "entities", "node"},
	"relación":    {"relation", "relations", "edge"},
	"relacion":    {"relation", "relations"},
	"grafo":       {"graph", "nodes"},
	"pensar":      {"think", "thinking", "reason"},
	"razonar":     {"think", "thinking", "reason"},
	"paso":        {"step", "sequential", "thought"},
	"pasos":       {"steps", "sequential", "thinking"},
	"árbol":       {"tree"},
	"arbol":       {"tree"},
	"información": {"info", "information"},
	"informacion": {"info", "information"},
	"imagen":      {"image", "media"},
	"permisos":    {"permissions", "perms", "info"},
}

// expandTerms devuelve los términos de la consulta más sus equivalentes.
//
// Se descartan las palabras de menos de tres letras: "de", "un" o "el" casan con
// todo y solo ensucian el orden.
func expandTerms(query string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if len(t) >= 3 && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, w := range strings.Fields(strings.ToLower(query)) {
		w = strings.Trim(w, ".,;:¿?¡!()\"'")
		add(w)
		for _, syn := range synonyms[w] {
			add(syn)
		}
		// Plurales y conjugaciones simples: "ficheros" -> "fichero".
		if strings.HasSuffix(w, "s") && len(w) > 4 {
			base := strings.TrimSuffix(w, "s")
			add(base)
			for _, syn := range synonyms[base] {
				add(syn)
			}
		}
	}
	return out
}
