package machine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/juan52878911/kindling/internal/digest"
	"github.com/juan52878911/kindling/pkg/api"
)

// paseUnico perfora las páginas a cero del volcado de memoria de un dorado y
// devuelve su digest en árbol, en UN pase de lectura (C9-02).
//
// Antes eran dos tras el volcado: `fallocate --dig-holes` releía los MemMiB
// enteros para perforar, y digest.Tree releía lo asignado para hashear. Tras
// el volcado casi nada queda en la caché (manager.go, en el congelado), así
// que cada pase iba al disco: con un modelo de 1,6 GiB, ~11-13 s cada uno.
// Aquí cada trozo de 4 MiB se lee una vez (con los lectores de digest.Tree),
// se perforan sus páginas a cero y se hashea lo leído.
//
// El digest es el de siempre: el de digest.Tree para el mismo contenido,
// porque perforar ceros no cambia lo que se lee (un hueco se lee como ceros,
// y Tree ya trataba un trozo hueco como 4 MiB de ceros). Los dorados de antes
// verifican igual y los nuevos verifican con digest.Matches sin cambios.
//
// Donde perforarRango es nil (macOS: el mem.file de kling-vz/kling-hv no es
// RAM cruda; ver perforarHuecos) no se perfora nada y es digest.Tree.
func paseUnico(ctx context.Context, path string) (string, error) {
	perforar := perforarRango
	modo := os.O_RDONLY
	if perforar != nil {
		modo = os.O_RDWR // perforar es escribir
	}
	f, err := os.OpenFile(path, modo, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if perforar == nil {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return digest.TreeFile(f, nil)
	}
	return digest.TreeFile(f, func(off int64, b []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return perforarCeros(f, off, b, perforar)
	})
}

// cronoCommit es el cronómetro por fases del commit: escribe lo mismo que
// antes en el registro ("commit X -> Y: overlay…, dump…") y además lo guarda
// por campo, para devolverlo en la respuesta (api.CommitTimings, C9-12).
type cronoCommit struct {
	t       *tiempos
	apreton time.Duration
	ct      api.CommitTimings
}

// nuevoCronoCommit empieza a contar ya; apreton es lo que tardó el apretón,
// que va antes de la pausa (0 si no hubo).
func nuevoCronoCommit(apreton time.Duration) *cronoCommit {
	return &cronoCommit{t: nuevosTiempos(), apreton: apreton, ct: api.CommitTimings{SqueezeMS: apreton.Milliseconds()}}
}

func (c *cronoCommit) marca(fase string) {
	ms := time.Since(c.t.ultima).Milliseconds()
	c.t.marca(fase)
	switch fase {
	case "overlay":
		c.ct.OverlayMS += ms
	case "dump":
		c.ct.DumpMS += ms
	case "holes":
		c.ct.HolesMS += ms
	case "sync":
		c.ct.SyncMS += ms
	case "pass", "digest":
		// El pase único perfora y hashea a la vez: va entero al digest
		// (api.CommitTimings).
		c.ct.DigestMS += ms
	}
}

// String es la línea del registro, con el apretón si lo hubo.
func (c *cronoCommit) String() string {
	if c.apreton > 0 {
		return fmt.Sprintf("squeeze %d ms, %s", c.apreton.Milliseconds(), c.t)
	}
	return c.t.String()
}

// desglose devuelve lo medido hasta ahora, con el total desde el apretón.
func (c *cronoCommit) desglose() *api.CommitTimings {
	out := c.ct
	out.TotalMS = (c.apreton + time.Since(c.t.t0)).Milliseconds()
	return &out
}

// paginaCeros es una página a cero, para comparar.
var paginaCeros = make([]byte, paginaVolcado)

// paginaVolcado es el grano del perforado: la página del invitado. fallocate
// --dig-holes va por el bloque del sistema de ficheros, que en ext4/XFS del
// host es también 4 KiB.
const paginaVolcado = 4096

// perforarCeros perfora las tiradas de páginas a cero de b (el contenido del
// trozo que empieza en off): una llamada por tirada, no por página. Una
// página con un solo byte distinto de cero se queda. La cola de menos de una
// página no se perfora (fallocate tampoco: no llena un bloque).
func perforarCeros(f *os.File, off int64, b []byte, perforar func(*os.File, int64, int64) error) error {
	ini := -1
	cerrar := func(fin int) error {
		if ini < 0 {
			return nil
		}
		desde, n := off+int64(ini), int64(fin-ini)
		ini = -1
		if err := perforar(f, desde, n); err != nil {
			return fmt.Errorf("punching %d bytes at %d: %w", n, desde, err)
		}
		return nil
	}
	p := 0
	for ; p+paginaVolcado <= len(b); p += paginaVolcado {
		if bytes.Equal(b[p:p+paginaVolcado], paginaCeros) {
			if ini < 0 {
				ini = p
			}
			continue
		}
		if err := cerrar(p); err != nil {
			return err
		}
	}
	return cerrar(p)
}
