package api

// LabelService es la clave convencional para agrupar por servidor MCP.
const LabelService = "service"

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
