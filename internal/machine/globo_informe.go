package machine

import (
	"context"
	"log"
	"os"

	"github.com/juan52878911/kindling/internal/fc"
)

// Informe de páginas libres del globo (free page reporting).
//
// Con el globo solo, lo que el invitado libera se queda en el host hasta que
// alguien aprieta (squeeze, el apretón al estar lista, el de antes de volcar).
// Con el informe, el driver virtio-balloon del invitado avisa de los bloques
// libres (de 2 MiB en x86 con páginas de 4 KiB, cada ~2 s) y Firecracker los
// suelta con madvise(DONTNEED), como al inflar: el RSS sigue a lo que el
// invitado usa sin que nadie tenga que pedirlo. No quita caché de página (eso
// sigue siendo cosa del apretón), solo lo que el invitado ya dio por libre.
//
// Hace falta en los dos lados: Firecracker 1.14 o posterior (el campo
// free_page_reporting de PUT /balloon; uno anterior lo rechaza con un 400, y
// se vuelve a pedir sin él) y un kernel de invitado con CONFIG_PAGE_REPORTING
// (VIRTIO_BALLOON lo selecciona desde Linux 5.7: el de kindling lo trae; uno
// sin él no negocia la función y todo sigue como antes). Va en la
// configuración del globo y viaja en el snapshot, así que lo heredan las
// copias de un dorado: en ellas, soltar una página la devuelve a la del
// mem.file compartido. Solo en Firecracker. KLING_FREE_PAGE_REPORTING=0 lo
// apaga.

// informePaginasLibres dice si se pide el informe de páginas libres.
func informePaginasLibres() bool {
	return !globoSinEstadisticas && os.Getenv("KLING_FREE_PAGE_REPORTING") != "0"
}

// configurarGlobo da a la microVM id, antes de arrancarla, su globo con amount
// MiB retenidos, deflate_on_oom, estadísticas y, si se puede, el informe de
// páginas libres. Si el VMM rechaza el informe, se repite sin él.
func (m *Manager) configurarGlobo(ctx context.Context, c *fc.Client, id string, amount int) error {
	if !informePaginasLibres() {
		return c.SetBalloon(ctx, amount, true, balloonStatsPollSec, false)
	}
	err := c.SetBalloon(ctx, amount, true, balloonStatsPollSec, true)
	if err == nil {
		return nil
	}
	if err2 := c.SetBalloon(ctx, amount, true, balloonStatsPollSec, false); err2 != nil {
		return err2
	}
	log.Printf("%s: the VMM does not take free_page_reporting (%v): balloon without it", shortID(id), err)
	return nil
}
