package machine

// El disco de solo lectura de un dorado: la imagen (o su capa) y la base de esa
// capa, con su digest en meta.json (image_digest, base_digest) y firmado.
//
// Un dorado es un overlay y una RAM congelados SOBRE un ext4 concreto. Si ese
// ext4 cambia después (un `kling image refresh` de antes del 409 con dorados,
// una imagen reemplazada a mano, una base que se reconstruye con otro
// contenido), las copias restauran una caché de páginas y un overlay que no
// casan con el disco que ven, y el fallo sale dentro del invitado, lejos de
// la causa. La firma v2 cubría el NOMBRE de la imagen, no su contenido.
//
// Coste: el digest es el de internal/digest.Tree (en paralelo, sin leer los
// huecos) y se cachea por ruta, tamaño y fecha, como el del kernel: se hashea
// una vez por imagen y arranque del daemon, aunque haya cien dorados sobre la
// misma base. Restaurar paga dos stat.

import (
	"fmt"
	"os"

	"github.com/juan52878911/kindling/internal/digest"
	"github.com/juan52878911/kindling/pkg/api"
)

// digestDiscoBase es el digest en árbol del ext4 path, cacheado por tamaño y
// fecha. La huella se toma ANTES de hashear: si el fichero cambia mientras, la
// siguiente llamada no casa y lo vuelve a leer.
func (m *Manager) digestDiscoBase(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	tam, fecha := st.Size(), st.ModTime().UnixNano()
	if v, ok := m.discosBase.Load(path); ok {
		if h := v.(huellaKernel); h.tam == tam && h.fecha == fecha {
			return h.hash, nil
		}
	}
	hash, err := digest.Tree(path)
	if err != nil {
		return "", err
	}
	m.discosBase.Store(path, huellaKernel{tam: tam, fecha: fecha, hash: hash})
	return hash, nil
}

// digestsDeImagen son los digests del disco de solo lectura con que arranca
// image: el de su ext4 (o su capa) y, si va por capas, el de su base.
func (m *Manager) digestsDeImagen(image string) (img, base string, err error) {
	rootfs, capa, err := m.imageLayer(image)
	if err != nil {
		return "", "", err
	}
	if capa == "" {
		img, err = m.digestDiscoBase(rootfs)
		return img, "", err
	}
	if img, err = m.digestDiscoBase(capa); err != nil {
		return "", "", err
	}
	base, err = m.digestDiscoBase(rootfs)
	return img, base, err
}

// comprobarDiscoBase exige que la imagen de snap tenga el contenido sobre el
// que se congeló. Sin image_digest (un dorado de antes de meta.json v4, o el
// temporal de un fork) no hay con qué comparar y se salta.
func (m *Manager) comprobarDiscoBase(snap *api.Snapshot) error {
	if snap.ImageDigest == "" {
		return nil
	}
	img, base, err := m.digestsDeImagen(snap.Image)
	if err != nil {
		return fmt.Errorf("template %q: reading image %q to check it is the one it was frozen over: %w", snap.Name, snap.Image, err)
	}
	if img != snap.ImageDigest || base != snap.BaseDigest {
		cual := "image"
		if img == snap.ImageDigest {
			cual = "base image"
		}
		return marcado(errCorrupto, "template %q was frozen over a different %s than %q has now "+
			"(its disk changed after the commit), and its copies would restore over a disk that doesn't match. Recreate it:\n%s",
			snap.Name, cual, snap.Image, remedioDorado(snap.Name))
	}
	return nil
}
