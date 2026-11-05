package machine

import (
	"slices"
	"testing"
)

// El VMM no hereda el entorno entero del daemon: ni KLING_TOKEN ni claves de
// otras herramientas; sí lo que lo configura (KLING_HV_*, KLING_VZ_*) y lo
// que necesita para correr, más lo que el daemon le añade.
func TestEntornoProcesoVMM(t *testing.T) {
	t.Setenv("KLING_TOKEN", "secreto")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secreto")
	t.Setenv("KLING_HV_EVENT_IDX", "0")
	t.Setenv("KLING_VZ_MTU", "1500")
	t.Setenv("PATH", "/usr/bin")
	env := entornoProceso([]string{"KLING_VZ_CONFINE_ROOT=/r"})
	for _, k := range []string{"KLING_TOKEN=secreto", "AWS_SECRET_ACCESS_KEY=secreto"} {
		if slices.Contains(env, k) {
			t.Errorf("%s reached the VMM's environment", k)
		}
	}
	for _, k := range []string{"KLING_HV_EVENT_IDX=0", "KLING_VZ_MTU=1500", "PATH=/usr/bin", "KLING_VZ_CONFINE_ROOT=/r"} {
		if !slices.Contains(env, k) {
			t.Errorf("%s missing from the VMM's environment", k)
		}
	}
}
