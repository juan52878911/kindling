package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// VolcadosDeVMM cuenta en root las máquinas congeladas y los dorados hechos
// con el VMM nombre ("kling-vz"): su memoria solo la carga ese VMM. La
// elección automática de backend lo mira para no dejar sin descongelar lo que
// ya hay al pasar de kling-vz a kling-hv.
func VolcadosDeVMM(root, nombre string) int {
	de := func(vmm string) bool {
		n, _, _ := strings.Cut(vmm, " ")
		return n == nombre
	}
	total := 0
	congeladas, _ := filepath.Glob(filepath.Join(root, "machines", "*"))
	for _, dir := range congeladas {
		if de(vmmDelVolcado(dir)) {
			total++
		}
	}
	metas, _ := filepath.Glob(filepath.Join(root, "snapshots", "*", "meta.json"))
	for _, p := range metas {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s struct {
			VMM string `json:"vmm"`
		}
		if json.Unmarshal(raw, &s) == nil && de(s.VMM) {
			total++
		}
	}
	return total
}
