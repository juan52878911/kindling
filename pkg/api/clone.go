package api

import (
	"slices"
	"time"
)

// Clone devuelve una copia profunda de mc: nada en el resultado comparte
// memoria con el original, ni con ningún otro Clone().
//
// Existe porque persist() guarda una FOTO de cada máquina (ver manager.go) y,
// hasta ahora, esa foto era una copia por VALOR de *api.Machine. Eso basta
// para los campos escalares, pero Volumes, Shares, Forwards, AllowDomains,
// CredentialDomains y Labels son slices y mapas: una copia por valor solo copia la cabecera, y la
// foto y la máquina viva siguen apuntando al MISMO array o mapa subyacente.
// withDriveIDs (ver manager.go, M-03) muta ese slice compartido, así que una
// foto tomada mientras persist() serializa fuera del lock puede ver un array a
// medio escribir — una carrera de datos real, no solo teórica. Lo mismo vale
// para los punteros a time.Time (StartedAt, FrozenAt, FailedAt, TTLAt) y para
// Wake: sin copiar lo que señalan, dos *api.Machine acaban señalando la misma
// fecha o el mismo WakePhases, y mutar uno a través de su puntero muta al
// otro por debajo.
func (mc *Machine) Clone() *Machine {
	if mc == nil {
		return nil
	}
	out := *mc // cubre los campos escalares (ID, State, VCPUs, CreatedAt...)

	out.Volumes = append([]VolumeAttachment(nil), mc.Volumes...)
	out.Shares = append([]ShareAttachment(nil), mc.Shares...)
	out.AllowDomains = append([]string(nil), mc.AllowDomains...)
	out.CredentialDomains = append([]string(nil), mc.CredentialDomains...)
	out.CredentialAnyDatabase = append([]string(nil), mc.CredentialAnyDatabase...)

	if mc.Forwards != nil {
		out.Forwards = make(map[string]string, len(mc.Forwards))
		for k, v := range mc.Forwards {
			out.Forwards[k] = v
		}
	}
	if mc.Labels != nil {
		out.Labels = make(map[string]string, len(mc.Labels))
		for k, v := range mc.Labels {
			out.Labels[k] = v
		}
	}

	out.StartedAt = clonarFecha(mc.StartedAt)
	out.FrozenAt = clonarFecha(mc.FrozenAt)
	out.FailedAt = clonarFecha(mc.FailedAt)
	out.TTLAt = clonarFecha(mc.TTLAt)

	if mc.Wake != nil {
		w := *mc.Wake
		out.Wake = &w
	}
	if mc.Agent != nil {
		// slices.Clone y no append(nil, ...): unas Caps vacías pero no nil
		// son "anuncia y no tiene nada", y nil es "no anuncia" (GuestAgent).
		a := *mc.Agent
		a.Caps = slices.Clone(mc.Agent.Caps)
		out.Agent = &a
	}

	return &out
}

// clonarFecha copia lo que señala t a un puntero nuevo. nil se conserva nil.
func clonarFecha(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
