package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
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
func (m *Manager) ReadImageFile(ctx context.Context, image, p string, max int64) ([]byte, error) {
	p, err := cleanGuestPath(p)
	if err != nil {
		return nil, err
	}
	base, layer, err := m.imageLayer(image)
	if err != nil {
		return nil, err
	}
	bin := debugfsBin()
	if bin == "" {
		return nil, fmt.Errorf("cannot find debugfs (comes with e2fsprogs)")
	}
	type candidato struct{ img, inside string }
	var cs []candidato
	if layer != "" {
		cs = append(cs, candidato{layer, layerGuestPath(strings.TrimPrefix(p, "/"))})
	}
	cs = append(cs, candidato{base, p})
	for _, c := range cs {
		has, err := hasFile(ctx, c.img, c.inside)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		out, err := exec.CommandContext(ctx, bin, "-R", "cat "+c.inside, c.img).Output()
		if err != nil {
			return nil, fmt.Errorf("reading %s from %s: %w", p, image, err)
		}
		if int64(len(out)) > max {
			return nil, fmt.Errorf("%s is %d bytes; the limit is %d", p, len(out), max)
		}
		return out, nil
	}
	return nil, ErrNotInImage
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
	quiero, err := fileDigest(src)
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
