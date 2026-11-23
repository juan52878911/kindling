package frontal

import (
	"sort"
	"testing"
	"time"
)

// BenchmarkCodigo mide POST /code con print(1) (D-10) contra el daemon que
// ejecuta de verdad en esta máquina: sin context_id (contexto efímero: se crea
// y se borra en cada llamada) y con un contexto ya creado. Da el p50 y los
// exec y arranques de proceso que pide al daemon por llamada.
func BenchmarkCodigo(b *testing.B) {
	for _, conContexto := range []bool{false, true} {
		nombre := "efimero"
		if conContexto {
			nombre = "contexto"
		}
		b.Run(nombre, func(b *testing.B) {
			bc := montarCodigo(b)
			cid := ""
			if conContexto {
				resp := pide(b, bc.srv, tokenAlice, "POST", bc.base+"/code/contexts", map[string]any{"language": "python"})
				esperaCodigo(b, resp, 201)
				cid = decodifica[struct {
					ID string `json:"id"`
				}](b, resp).ID
			}
			bc.d.mu.Lock()
			execs, arranques := len(bc.d.execs), len(bc.d.arranques)
			bc.d.mu.Unlock()
			ds := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ini := time.Now()
				if got := texto(bc.corre(b, cid, "print(1)"), "stdout"); got != "1\n" {
					b.Fatalf("stdout %q", got)
				}
				ds = append(ds, time.Since(ini))
			}
			b.StopTimer()
			sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
			b.ReportMetric(float64(ds[len(ds)/2].Microseconds()), "p50-µs")
			bc.d.mu.Lock()
			b.ReportMetric(float64(len(bc.d.execs)-execs)/float64(b.N), "execs/op")
			b.ReportMetric(float64(len(bc.d.arranques)-arranques)/float64(b.N), "arranques/op")
			bc.d.mu.Unlock()
		})
	}
}
