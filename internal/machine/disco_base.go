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
// huecos) y se cachea por ruta y huella (tamaño, fecha, inodo y ctime): se hashea
// una vez por imagen y arranque del daemon, aunque haya cien dorados sobre la
// misma base. Restaurar paga dos stat.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/juan52878911/kindling/internal/digest"
	"github.com/juan52878911/kindling/pkg/api"
)

// huellaDisco es lo que dice que un ext4 de imagen sigue siendo el que se
// hasheó: tamaño y fecha, y también dispositivo, inodo y ctime. La fecha la
// pone a mano quien escribe en el fichero (utimes): con solo tamaño y fecha,
// reescribir la imagen con otro contenido del mismo tamaño y devolverle su
// mtime pasaría por la de antes, y comprobarDiscoBase daría por bueno un
// disco que ya no es el del dorado. El ctime no se puede fijar desde fuera
// del kernel (cualquier escritura o utimes lo mueve) y un fichero puesto en
// su lugar con rename trae otro inodo.
type huellaDisco struct {
	tam, fecha, ctime int64
	dev, ino          uint64
}

func huellaDeDisco(st os.FileInfo) huellaDisco {
	dev, ino, ctime := identidadDisco(st)
	return huellaDisco{tam: st.Size(), fecha: st.ModTime().UnixNano(), ctime: ctime, dev: dev, ino: ino}
}

// digestCacheado es el digest de un ext4 junto con la huella que lo produjo.
type digestCacheado struct {
	huella huellaDisco
	hash   string
}

// digestDiscoBase es el digest en árbol del ext4 path, cacheado por su
// huella (huellaDisco). La huella se toma ANTES de hashear: si el fichero
// cambia mientras, la siguiente llamada no casa y lo vuelve a leer.
//
// Uno solo por ruta a la vez (P5): tras arrancar el daemon, N run -from
// simultáneos del mismo dorado hasheaban cada uno la imagen entera (con 8
// hilos cada uno). Ahora el primero la lee y los demás esperan su resultado,
// si es de la misma huella.
func (m *Manager) digestDiscoBase(path string) (string, error) {
	for {
		// Espera a un enlaceEstable en curso sobre esta ruta: su ctime a
		// medias no es un cambio del contenido.
		mu, _ := cerrojosEnlace.LoadOrStore(path, new(sync.Mutex))
		mu.(*sync.Mutex).Lock()
		st, err := os.Stat(path)
		var cacheado *digestCacheado
		if v, ok := m.discosBase.Load(path); ok {
			c := v.(digestCacheado)
			cacheado = &c
		}
		mu.(*sync.Mutex).Unlock()
		if err != nil {
			return "", err
		}
		huella := huellaDeDisco(st)
		if cacheado != nil && cacheado.huella == huella {
			return cacheado.hash, nil
		}
		v := &vueloDigest{hecho: make(chan struct{}), huella: huella}
		if otro, enCurso := m.discosEnVuelo.LoadOrStore(path, v); enCurso {
			o := otro.(*vueloDigest)
			<-o.hecho
			if o.huella == huella {
				return o.hash, o.err
			}
			continue // era de otra huella: el fichero cambió entre medias
		}
		// En un defer: aunque el digest entrara en pánico, los que esperan
		// no se quedan colgados.
		func() {
			defer func() {
				m.discosEnVuelo.CompareAndDelete(path, v)
				close(v.hecho)
			}()
			v.err = errors.New("digest interrupted")
			v.hash, v.err = digestArbol(path)
			if v.err == nil {
				m.discosBase.Store(path, digestCacheado{huella: huella, hash: v.hash})
			}
		}()
		return v.hash, v.err
	}
}

// vueloDigest es un digestDiscoBase en curso: hecho se cierra con hash y err
// ya puestos, de esa huella.
type vueloDigest struct {
	hecho  chan struct{}
	huella huellaDisco
	hash   string
	err    error
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

// huellaConfiable dice si el fichero (de st) solo lo puede modificar root, de
// modo que conservar su digest tras la operación es seguro.
var huellaConfiable = func(st os.FileInfo) bool { return true }

// cerrojosEnlace serializa, por ruta, las operaciones de enlaceEstable.
var cerrojosEnlace sync.Map // ruta → *sync.Mutex

// enlaceEstable ejecuta op, que hace enlaces duros (o los borra) sobre el
// fichero path, sin perder el digest cacheado de path. Un enlace duro cambia
// nlink y por tanto el ctime del inodo, y la huella (huellaDisco) lo mira: sin
// esto cada restauración con jailer y cada borrado de jail invalidaban el
// digest y la siguiente creación volvía a hashear la imagen entera.
//
// Bajo un cerrojo por ruta: si la huella de antes es la cacheada (la
// verificada), ejecuta op y reanota la de después solo si lo único que cambió
// es el ctime (mismos tamaño, fecha, dispositivo e inodo). Cualquier otro
// cambio, una huella previa distinta o un op que falla no anotan nada, y la
// siguiente comprobación rehashea. Un camino que no pase por aquí solo cuesta
// un rehash, nunca da por bueno un disco distinto.
func (m *Manager) enlaceEstable(path string, op func() error) error {
	if _, ok := m.discosBase.Load(path); !ok {
		return op()
	}
	mu, _ := cerrojosEnlace.LoadOrStore(path, new(sync.Mutex))
	l := mu.(*sync.Mutex)
	l.Lock()
	defer l.Unlock()
	antes, errAntes := os.Stat(path)
	if errAntes != nil {
		return op()
	}
	hAntes := huellaDeDisco(antes)
	v, ok := m.discosBase.Load(path)
	if !ok || v.(digestCacheado).huella != hAntes {
		return op()
	}
	c := v.(digestCacheado)
	if err := op(); err != nil {
		return err
	}
	despues, err := os.Stat(path)
	if err != nil {
		return nil
	}
	hDespues := huellaDeDisco(despues)
	hAntes.ctime = hDespues.ctime
	if hAntes == hDespues {
		// CompareAndSwap: si otro digest ya anotó algo, no se pisa.
		m.discosBase.CompareAndSwap(path, c, digestCacheado{huella: hDespues, hash: c.hash})
	}
	return nil
}
