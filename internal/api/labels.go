package api

// LabelService es la clave convencional para agrupar por servidor MCP.
const LabelService = "service"

// LabelStateful marca los servicios que ACUMULAN estado entre llamadas.
//
// El modo efímero destruye la máquina tras cada acción, y con ella todo lo que
// el servidor guardara en memoria o en su disco. Para un servidor de ficheros
// da igual —el estado está fuera—, pero un grafo de conocimiento o un
// razonamiento por pasos perderían su contenido en cada invocación.
//
// Los servicios marcados así usan una instancia persistente, que se congela al
// quedar ociosa y vuelve en milisegundos conservando lo que tenía.
const LabelStateful = "stateful"

// Service devuelve el servicio al que pertenece la máquina, o "" si no tiene.
func (m *Machine) Service() string {
	if m.Labels == nil {
		return ""
	}
	return m.Labels[LabelService]
}

// Service devuelve el servicio del snapshot.
func (s *Snapshot) Service() string {
	if s.Labels == nil {
		return ""
	}
	return s.Labels[LabelService]
}

// Stateful indica si el servicio debe conservar estado entre llamadas.
func (s *Snapshot) Stateful() bool {
	return s.Labels != nil && s.Labels[LabelStateful] == "true"
}

// MergeLabels combina etiquetas; las de override ganan.
func MergeLabels(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}
