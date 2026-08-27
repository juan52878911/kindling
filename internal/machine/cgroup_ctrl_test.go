package machine

import "testing"

// `cpuset` contiene "cpu" como subcadena. Con strings.Contains, un anfitrion
// donde cpuset este delegado y cpu NO daba un falso positivo: se saltaba el
// `+cpu` y a partir de ahi ninguna microVM tenia techo de CPU — en silencio, y
// justo en los anfitriones donde el techo mas falta hace.
func TestControladorPresenteNoConfundeCpuConCpuset(t *testing.T) {
	casos := []struct {
		lista  string
		quiero string
		hay    bool
	}{
		{"cpuset cpu io memory pids", "cpu", true},
		{"cpu", "cpu", true},
		{"memory cpu", "cpu", true},
		{"cpuset io memory pids", "cpu", false}, // el caso del fallo
		{"cpuset", "cpu", false},
		{"", "cpu", false},
		{"cpuacct", "cpu", false},
		{"  cpuset   cpu  ", "cpu", true}, // espacios de sobra
	}
	for _, c := range casos {
		if got := controladorPresente(c.lista, c.quiero); got != c.hay {
			t.Errorf("controladorPresente(%q, %q) = %v, esperaba %v", c.lista, c.quiero, got, c.hay)
		}
	}
}
