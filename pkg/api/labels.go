package api

// LabelService es la clave convencional para agrupar máquinas y snapshots por
// servicio: todas las instancias de un mismo servicio la comparten. La usa el
// scheduler para resolver servicio → snapshot, y `topo` y /metrics para agrupar.
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

// LabelOwner es el inquilino dueño de una máquina, y por herencia de los
// snapshots que salen de ella y de las máquinas que nacen de esos snapshots.
// Con una política de autorización (docs/authz.md) la pone el daemon a partir
// de quién llama: un inquilino no puede fijarla ni cambiarla, y solo ve y
// toca lo que la lleva con su nombre. Sin política es una etiqueta más.
const LabelOwner = "kling.owner"

// LabelDBPrefix es el prefijo de las etiquetas de las bases de datos de
// ext/db (kling.db.owner, kling.db.state...). kindling-sandbox lo reserva: un
// inquilino no puede fijarlas.
const LabelDBPrefix = "kling.db."
