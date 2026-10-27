package api

// Etiquetas del fondo precalentado de una plantilla (internal/machine/fondo.go).
// Solo informan en `kling ps`: quién manda es el estado del daemon, y por eso
// la API las rechaza (ver ValidateNoGraphLabels): una máquina de un inquilino
// que las llevara no puede hacerse pasar por una copia del fondo.
const (
	LabelPool      = "kling.pool"
	LabelPoolReady = "kling.pool.ready"
)

// PoolMax es el tope de copias precalentadas por plantilla.
const PoolMax = 16

// PoolRequest fija el tamaño del fondo de una plantilla (0 lo apaga y lo vacía).
type PoolRequest struct {
	Size int `json:"size"`
}

// PoolInfo es el estado del fondo de una plantilla.
type PoolInfo struct {
	Template string `json:"template"`
	Size     int    `json:"size"`  // copias que se quieren
	Ready    int    `json:"ready"` // copias ya despiertas y reclamables
}
