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
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/juan52878911/kindling/internal/digest"
	"github.com/juan52878911/kindling/pkg/api"
)

// digestDiscoBase es el digest en árbol del ext4 path, cacheado por tamaño y
// fecha. La huella se toma ANTES de hashear: si el fichero cambia mientras, la
// siguiente llamada no casa y lo vuelve a leer.
//
// Uno solo por ruta a la vez (P5): tras arrancar el daemon, N run -from
// simultáneos del mismo dorado hasheaban cada uno la imagen entera (con 8
// hilos cada uno). Ahora el primero la lee y los demás esperan su resultado,
// si es de la misma huella.
func (m *Manager) digestDiscoBase(path string) (string, error) {
	for {
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
		v := &vueloDigest{hecho: make(chan struct{}), tam: tam, fecha: fecha}
		if otro, enCurso := m.discosEnVuelo.LoadOrStore(path, v); enCurso {
			o := otro.(*vueloDigest)
			<-o.hecho
			if o.tam == tam && o.fecha == fecha {
				return o.hash, o.err
			}
			continue // era de otra huella: el fichero cambió entre medias
		}
		v.hash, v.err = digestArbol(path)
		if v.err == nil {
			m.discosBase.Store(path, huellaKernel{tam: tam, fecha: fecha, hash: v.hash})
		}
		m.discosEnVuelo.CompareAndDelete(path, v)
		close(v.hecho)
		return v.hash, v.err
	}
}

// vueloDigest es un digestDiscoBase en curso: hecho se cierra con hash y err
// ya puestos, de la huella tam y fecha.
type vueloDigest struct {
	hecho      chan struct{}
	tam, fecha int64
	hash       string
	err        error
}

// digestArbol es digest.Tree; una variable para contar lecturas en las
// pruebas.
var digestArbol = digest.Tree

// precalentarDiscosBase calcula en segundo plano los digests de las imágenes
// sobre las que hay dorados (P5), de una en una (digest.Tree ya lee en
// paralelo): el primer run -from tras arrancar el daemon ya no paga leer la
// imagen entera, y si llega antes, espera a este en vez de leerla otra vez.
func (m *Manager) precalentarDiscosBase(ctx context.Context) {
	metas, _ := filepath.Glob(filepath.Join(m.root, "snapshots", "*", "meta.json"))
	vistas := map[string]bool{}
	for _, p := range metas {
		if ctx.Err() != nil {
			return
		}
		s, err := m.loadSnapshot(filepath.Base(filepath.Dir(p)))
		if err != nil || s.ImageDigest == "" || vistas[s.Image] {
			continue
		}
		vistas[s.Image] = true
		if _, _, err := m.digestsDeImagen(s.Image); err != nil {
			log.Printf("warning: precomputing the digest of image %q: %v", s.Image, err)
		}
	}
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
