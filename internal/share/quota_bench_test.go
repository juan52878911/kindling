package share

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// P6: escritores que alargan ficheros distintos, con y sin cuota. Con cuota
// no deberían ir en fila india.
func BenchmarkEscribirAlargando(b *testing.B) {
	for _, conCuota := range []bool{false, true} {
		b.Run(fmt.Sprintf("cuota=%v", conCuota), func(b *testing.B) {
			dir := b.TempDir()
			srv, err := Open(dir, false, "")
			if err != nil {
				b.Fatal(err)
			}
			defer srv.Close()
			if conCuota {
				srv.SetQuota(NewQuota(Limits{MaxBytes: 1 << 50}))
			}
			const escritores = 8
			trozo := make([]byte, 64<<10)
			fs := make([]*os.File, escritores)
			for i := range fs {
				f, err := os.Create(filepath.Join(dir, fmt.Sprintf("f%d", i)))
				if err != nil {
					b.Fatal(err)
				}
				defer f.Close()
				fs[i] = f
			}
			b.SetBytes(int64(len(trozo)) * escritores)
			b.ResetTimer()
			var wg sync.WaitGroup
			for i := range fs {
				wg.Add(1)
				go func(f *os.File) {
					defer wg.Done()
					for j := range b.N {
						// Cada 512 vueltas se vuelve al principio, para que el
						// fichero no crezca sin fin, y alarga de nuevo.
						off := int64(j%512) * int64(len(trozo))
						if off == 0 {
							_ = srv.truncate(f, 0)
						}
						if _, e := srv.writeAt(f, trozo, off); e != 0 {
							b.Error(e)
							return
						}
					}
				}(fs[i])
			}
			wg.Wait()
		})
	}
}
