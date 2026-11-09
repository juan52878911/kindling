package guest

import "testing"

// El tope blando del runtime es una cuarta parte de la RAM del invitado,
// entre 32 y 128 MiB; con GOMEMLIMIT en el entorno manda ese, y sin
// /proc/meminfo no se toca.
func TestLimiteMemoriaAgente(t *testing.T) {
	for _, c := range []struct {
		total int
		env   string
		ok    bool
		want  int64
	}{
		{64, "", true, 32 << 20},
		{256, "", true, 64 << 20},
		{512, "", true, 128 << 20},
		{4096, "", true, 128 << 20},
		{256, "100MiB", false, 0},
		{0, "", false, 0},
	} {
		got, ok := limiteMemoriaAgente(c.total, c.env)
		if ok != c.ok || got != c.want {
			t.Errorf("limiteMemoriaAgente(%d, %q) = %d %v, want %d %v", c.total, c.env, got, ok, c.want, c.ok)
		}
	}
}
