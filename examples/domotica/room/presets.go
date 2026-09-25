package room

import (
	_ "embed"
	"encoding/json"
)

// Los botones de la página, agrupados por la capa que se espera que los
// decida, son datos de la demo (presets.json). Los «direct» salen de las
// plantillas de la demo (un test lo comprueba contra la capa 1); los demás son
// justo lo que las capas rápidas no saben y escalan.
//
//go:embed presets.json
var presetsJSON []byte

// Presets son las órdenes de ejemplo.
var Presets = func() []Preset {
	var ps []Preset
	if err := json.Unmarshal(presetsJSON, &ps); err != nil {
		panic("room: presets.json: " + err.Error())
	}
	return ps
}()
