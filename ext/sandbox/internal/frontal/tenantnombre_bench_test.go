package frontal

import (
	"fmt"
	"io"
	"log"
	"testing"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/inquilinos"
)

// BenchmarkTenantNombre mide buscar un inquilino por nombre con 1000 en el
// almacén (D-06): el ingress lo hace en cada petición con token.
func BenchmarkTenantNombre(b *testing.B) {
	alm, err := inquilinos.Abrir(b.TempDir(), inquilinos.Opciones{})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		n := fmt.Sprintf("t%04d", i)
		if _, err := alm.Crear(n, false, inquilinos.Topes{}); err != nil {
			b.Fatal(err)
		}
		if _, _, err := alm.NuevaClave(n, nil); err != nil {
			b.Fatal(err)
		}
	}
	s, err := Nuevo(Opciones{Hosts: hosts.Nuevo(nil), Almacen: alm, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s.tenantNombre("t0500") == nil {
			b.Fatal("no encontrado")
		}
	}
}
