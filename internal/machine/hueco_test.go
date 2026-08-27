package machine

import (
	"path/filepath"
	"testing"
)

// Los numeros salen de la observacion que destapo el fallo: las imagenes se
// construyen ajustadas al byte, y meter un puente de 9,7 MB fallaba con "no
// space left on device" aunque el anfitrion tenia 3,5 GB libres. El sintoma
// apunta al disco; la causa estaba dentro del ext4.
func TestFaltaParaElPuenteExigeSitioParaLasDosCopias(t *testing.T) {
	const mib = 1 << 20
	const puente = 9_700_000 // el puente medido en el laboratorio

	casos := []struct {
		nombre string
		libre  int64
		crece  bool
	}{
		{"sequentialthinking, 6 MiB libres", 6 * mib, true},
		{"node, imagen al 100%", 0, true},
		{"justo el puente, sin holgura para el recambio", puente, true},
		{"puente mas holgura, exactos", puente + holguraRefresh, false},
		{"imagen con aire de sobra", 100 * mib, false},
	}

	for _, c := range casos {
		falta := faltaParaElPuente(c.libre, puente)
		if (falta > 0) != c.crece {
			t.Errorf("%s: falta=%d, esperaba crecer=%v", c.nombre, falta, c.crece)
		}
		if falta == 0 {
			continue
		}
		if falta%mib != 0 {
			t.Errorf("%s: falta=%d no es multiplo de MiB", c.nombre, falta)
		}
		// La invariante que de verdad importa: despues de crecer, CABE.
		if libre := c.libre + falta; libre < puente+holguraRefresh {
			t.Errorf("%s: tras crecer quedan %d libres; siguen sin caber %d",
				c.nombre, libre, puente+holguraRefresh)
		}
	}
}

func TestEspacioLibreMideElSistemaDeFicherosMontado(t *testing.T) {
	dir := t.TempDir()

	libre, err := espacioLibre(dir)
	if err != nil {
		t.Fatalf("espacioLibre(%s): %v", dir, err)
	}
	// Cazaria un Bsize leido con el tipo equivocado, que da 0 o un disparate.
	if libre <= 0 {
		t.Errorf("espacioLibre devolvio %d; un directorio existente tiene sitio", libre)
	}

	if _, err := espacioLibre(filepath.Join(dir, "no-existe")); err == nil {
		t.Error("espacioLibre no fallo sobre una ruta inexistente")
	}
}

// Medido en el laboratorio: se anadieron 12 MB a sequentialthinking.layer.ext4
// y el espacio libre dentro solo subio de 6,00 a 17,12 MB. Un 7% se lo quedaron
// los metadatos del ext4, y la primera pasada se quedo a 0,5 MB de caber.
func TestLaReservaDeMetadatosCubreLoQueElExt4SeQueda(t *testing.T) {
	const mib = 1 << 20
	const perdida = 0.08 // el 7% medido, con margen

	for _, falta := range []int64{4096, 512 * 1024, 1 * mib, 13 * mib, 100 * mib} {
		pedido := conReservaDeMetadatos(falta)
		entregado := int64(float64(pedido) * (1 - perdida))
		if entregado < falta {
			t.Errorf("faltaban %d B: se piden %d, el ext4 entregaria %d — seguiria sin caber",
				falta, pedido, entregado)
		}
	}
}
