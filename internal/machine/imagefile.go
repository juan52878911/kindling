package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/digest"
)

// FICHEROS DENTRO DE LAS IMÁGENES.
//
// Generaliza lo que antes solo se hacía con el puente (RefreshBridges) y con
// /etc/kling/capabilities.json (ImageCapabilities): leer o reemplazar un fichero
// dentro de una imagen ya construida, sin reconstruirla. Una extensión lo usa
// para lo suyo —kindling-mcp para poner al día su puente— sin que el núcleo sepa
// qué fichero es.

// ErrNotInImage dice que el fichero no está en la imagen.
var ErrNotInImage = errors.New("file is not in the image")

// cleanGuestPath valida una ruta absoluta dentro del invitado.
func cleanGuestPath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("path must be absolute: %q", p)
	}
	c := path.Clean(p)
	if c == "/" || strings.Contains(c, "..") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	return c, nil
}

// ReadImageFile devuelve el contenido de p dentro de la imagen (primero la capa
// del servicio, después su base). Falla con ErrNotInImage si no está y si pasa
// de max bytes.
//
// La capa es el upperdir de un overlay (ver layer.go), y se lee como lo vería
// el invitado: un whiteout de p (o de un directorio que lo contiene) es que se
// BORRÓ, y un directorio opaco de la capa tapa entero el de la base. Antes se
// caía a la base en los dos casos, y `images cat` enseñaba un fichero que el
// servicio no tiene.
//
// Las rutas van entre comillas y validadas (nombreDebugfs), y lo leído, con
// tope: sin comillas, "cat /a b" leía /a; sin tope, un fichero de gigas se
// cargaba entero en el daemon antes de comprobar max.
func (m *Manager) ReadImageFile(ctx context.Context, image, p string, max int64) ([]byte, error) {
	p, err := cleanGuestPath(p)
	if err != nil {
		return nil, err
	}
	if err := nombreDebugfs(p); err != nil {
		return nil, err
	}
	base, layer, err := m.imageLayer(image)
	if err != nil {
		return nil, err
	}
	bin := debugfsBin()
	if bin == "" {
		return nil, ErrNoDebugfs
	}
	leerDe := func(img, inside string) ([]byte, error) {
		b, err := imagenDebugfs{bin: bin, file: img}.leerFichero(ctx, inside, max)
		if err != nil {
			return nil, fmt.Errorf("reading %s from %s: %w", p, image, err)
		}
		return b, nil
	}
	if layer != "" {
		capa := imagenDebugfs{bin: bin, file: layer}
		dentro := layerGuestPath(strings.TrimPrefix(p, "/"))
		visto, err := capa.enCapa(ctx, dentro)
		if err != nil {
			return nil, err
		}
		switch visto {
		case capaLoTiene:
			return leerDe(layer, dentro)
		case capaLoTapa:
			return nil, ErrNotInImage
		}
	}
	e, err := imagenDebugfs{bin: bin, file: base}.stat(ctx, p)
	if err != nil {
		return nil, err
	}
	if !e.existe {
		return nil, ErrNotInImage
	}
	return leerDe(base, p)
}

// Lo que dice una capa de una ruta (ver enCapa).
const (
	capaNoDice  = iota // no la tiene ni la tapa: manda la base
	capaLoTiene        // la tiene: se lee de la capa
	capaLoTapa         // whiteout o directorio opaco: no está en la imagen
)

// enCapa recorre dentro (una ruta bajo /upper) componente a componente: un
// whiteout en cualquiera de ellos la borra, y un directorio opaco por encima
// hace que lo que la capa no tenga tampoco venga de la base.
func (im imagenDebugfs) enCapa(ctx context.Context, dentro string) (int, error) {
	comps := partes(dentro)
	cur := "/"
	opaco := false
	for i, c := range comps {
		cur = path.Join(cur, c)
		e, err := im.stat(ctx, cur)
		if err != nil {
			return 0, err
		}
		if !e.existe {
			break
		}
		if e.blanqueo {
			return capaLoTapa, nil
		}
		if i == len(comps)-1 {
			if e.tipo == "directory" {
				break // un directorio no se lee
			}
			return capaLoTiene, nil
		}
		if e.tipo != "directory" {
			// Algo que no es un directorio tapa el de la base entero.
			return capaLoTapa, nil
		}
		if e.opaco {
			opaco = true
		}
	}
	if opaco {
		return capaLoTapa, nil
	}
	return capaNoDice, nil
}

// StatImageFile dice si p está en la imagen, cuánto ocupa y su sha256.
func (m *Manager) StatImageFile(ctx context.Context, image, p string) (*api.ImageFileStat, error) {
	b, err := m.ReadImageFile(ctx, image, p, 256<<20)
	if errors.Is(err, ErrNotInImage) {
		return &api.ImageFileStat{Path: p}, nil
	}
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	return &api.ImageFileStat{Path: p, Exists: true, Size: int64(len(b)), SHA256: hex.EncodeToString(h[:])}, nil
}

// PutImageFile pone el fichero del anfitrión src en p dentro de la imagen. En
// una imagen por capas se escribe en la capa, que tapa a la base. Con
// create=false solo reemplaza lo que ya está, como el recambio del puente. Se
// niega si la imagen está en uso: cambiarla por debajo de una máquina viva
// corrompería lo que ésta tiene montado.
func (m *Manager) PutImageFile(ctx context.Context, image, p, src string, mode os.FileMode, create bool) (api.ImageFileResult, error) {
	res := api.ImageFileResult{Image: image, Path: p}
	p, err := cleanGuestPath(p)
	if err != nil {
		return res, err
	}
	quiero, err := digest.File(src)
	if err != nil {
		return res, fmt.Errorf("reading %s: %w", src, err)
	}
	if !validName.MatchString(image) {
		return res, fmt.Errorf("invalid image name %q", image)
	}
	_, layered := m.ImageBase(image)
	file, dentro := m.imagePath(image), p
	if layered {
		file, dentro = m.layerPath(image), layerGuestPath(strings.TrimPrefix(p, "/"))
	}
	if _, err := os.Stat(file); err != nil {
		return res, fmt.Errorf("image %q does not exist", image)
	}
	if users := m.imageUsers()[image]; len(users) > 0 {
		res.Busy = true
		return res, fmt.Errorf("image %q is in use by %d machine(s): %s", image, len(users), strings.Join(users, ", "))
	}
	updated, err := m.putOne(ctx, file, dentro, src, quiero, mode, create)
	if errors.Is(err, errNoBridge) {
		res.Skipped = true
		return res, ErrNotInImage
	}
	res.Updated = updated
	return res, err
}
