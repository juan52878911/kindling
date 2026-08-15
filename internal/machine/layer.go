package machine

// Imágenes por CAPAS: base compartida + delta del servicio.
//
// Una imagen monolítica ($NAME.ext4) es una copia entera de la base con unos
// pocos MB propios encima: 500 servicios eran 500 copias de los mismos ~130 MiB.
// Una imagen por capas ($NAME.layer.ext4) guarda SOLO el delta y se apoya en la
// base que ya está en disco, igual que las capas de OCI.
//
// El delta se construye como el upperdir de un overlay sobre la base
// (scripts/80-mcp-image.sh), así que dentro del ext4 de la capa el contenido
// cuelga de /upper — incluidos los whiteouts de los ficheros borrados. Al
// arrancar, esa capa se engancha como disco de solo lectura y el invitado la
// pone de lower por delante de la base: lowerdir=<capa>/upper:/.
//
// Las dos formas conviven: la presencia de $NAME.ext4 manda, y sin él la de
// $NAME.layer.ext4 marca la imagen como por capas. Nada que migrar.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/internal/api"
)

// layerUpperDir es el directorio dentro de la capa donde está el delta.
//
// Lo fija el build: overlayfs exige que upperdir y workdir compartan sistema de
// ficheros sin anidarse, así que la capa se construye con /upper y /work
// hermanos, y el /work se borra al terminar. Quien lea la capa —el invitado por
// el lowerdir, o debugfs desde el anfitrión— tiene que contar con este prefijo.
const layerUpperDir = "upper"

// defaultBaseImage es la base que asume 80-mcp-image.sh cuando no se le indica
// otra (BASE_IMAGE=min). Las recetas de esas construcciones dejan Base vacío, y
// aquí hay que resolverlo al mismo nombre o la capa se quedaría sin suelo.
const defaultBaseImage = "min"

// layerPath es el delta de una imagen por capas.
func (m *Manager) layerPath(image string) string {
	return filepath.Join(m.root, "images", image+".layer.ext4")
}

// recipePath es la receta que 80-mcp-image.sh dejó junto a la imagen. Es donde
// consta sobre qué base se construyó.
func (m *Manager) recipePath(image string) string {
	return filepath.Join(m.root, "images", image+".recipe.json")
}

// imageLayer resuelve los discos de solo lectura con los que arranca una imagen.
//
// Devuelve siempre el rootfs que va en vda; layer es "" para las imágenes
// monolíticas de siempre, y la ruta del delta para las que van por capas. El
// error es para lo que no se puede arrancar: ni imagen ni capa, o una capa
// huérfana cuya base no está en disco —que es peor que no tener nada, porque
// parece que la imagen existe—.
func (m *Manager) imageLayer(image string) (base, layer string, err error) {
	mono := m.imagePath(image)
	if _, err := os.Stat(mono); err == nil {
		// La imagen monolítica manda: si están las dos, arrancar por el camino
		// nuevo cambiaría lo que ve un servicio que hoy funciona.
		return mono, "", nil
	}

	layer = m.layerPath(image)
	if _, serr := os.Stat(layer); serr != nil {
		return "", "", fmt.Errorf("can't find image %q in %s", image, mono)
	}

	name := m.recipeBase(image)
	base = m.imagePath(name)
	if _, serr := os.Stat(base); serr != nil {
		return "", "", fmt.Errorf("layered image %q needs base image %q, which is not in %s: "+
			"build it (scripts/70-build-minimal-image.sh) or repackage the service",
			image, name, base)
	}
	return base, layer, nil
}

// recipeBase lee de la receta sobre qué base se construyó la imagen.
//
// Sin receta —o con una ilegible— se asume la base por defecto: es la que usó el
// build salvo que se le dijera otra cosa, así que acertar es lo normal y el caso
// contrario da un error claro en imageLayer, no un arranque raro.
func (m *Manager) recipeBase(image string) string {
	b, err := os.ReadFile(m.recipePath(image))
	if err != nil {
		return defaultBaseImage
	}
	var rec api.ImageRecipe
	if json.Unmarshal(b, &rec) != nil || rec.Base == "" {
		return defaultBaseImage
	}
	return rec.Base
}

// guestInitPath es el init del invitado, que vive DENTRO de la base.
const guestInitPath = "/sbin/overlay-init"

// baseSupportsLayers dice si el overlay-init de esa base entiende kling.layer.
//
// Hace falta preguntarlo porque el init viaja dentro de la imagen base, igual
// que el puente: actualizar kindling en el anfitrión NO actualiza el init de una
// base construida antes de las capas. Y ese fallo no es una carencia de
// funciones — el init viejo ignora el parámetro, monta solo la raíz, el invitado
// se queda sin /entrypoint y lo que se ve es un pánico del kernel. Deducir de ahí
// que hay que reconstruir la base es pedir demasiado.
//
// El error va aparte del booleano, como en imageHasBridge: sin debugfs no se
// puede saber, y "no lo sé" no es "no lo lleva".
func (m *Manager) baseSupportsLayers(ctx context.Context, base string) (bool, error) {
	// La respuesta solo cambia si cambia el fichero, y se pregunta en el camino
	// de arranque en frío: la clave lleva tamaño y mtime para no tener que volver
	// a lanzar debugfs por cada microVM.
	key := base
	if fi, err := os.Stat(base); err == nil {
		key = fmt.Sprintf("%s|%d|%d", base, fi.Size(), fi.ModTime().UnixNano())
	}
	if v, ok := m.layerOK.Load(key); ok {
		return v.(bool), nil
	}
	bin := debugfsBin()
	if bin == "" {
		return false, fmt.Errorf("cannot find debugfs (comes with e2fsprogs)")
	}
	out, err := exec.CommandContext(ctx, bin, "-R", "cat "+guestInitPath, base).Output()
	if err != nil {
		return false, fmt.Errorf("reading %s from %s: %w", guestInitPath, base, err)
	}
	ok := strings.Contains(string(out), api.LayerBootParam)
	m.layerOK.Store(key, ok)
	return ok, nil
}

// layerDriveID nombra el disco de la capa dentro del VMM. Como el de los
// volúmenes, queda grabado en el snapshot: cambiarlo rompería la restauración.
const layerDriveID = "svclayer"

// layerDevice dice en qué /dev/vdX cae la capa.
//
// El orden de enganche es el contrato: vda base, vdb overlay, vdc.. volúmenes, y
// la capa DETRÁS de todos. Va al final, y no en medio, para no correr las letras
// de los volúmenes: el puente los cuenta desde vdc por posición, y meter un
// disco antes montaría cada volumen en el sitio de otro.
func layerDevice(nvols int) (string, error) {
	i := 2 + nvols // 0=base, 1=overlay, luego los volúmenes
	if i > 25 {
		return "", fmt.Errorf("too many disks (%d) to attach the service layer", i)
	}
	return "/dev/vd" + string(rune('a'+i)), nil
}

// layerBootArg es lo que lee overlay-init dentro del invitado para saber qué
// disco poner de lower por delante de la base. Vacío = imagen monolítica.
func layerBootArg(dev string) string {
	if dev == "" {
		return ""
	}
	return " " + api.LayerBootParam + "=" + dev
}

// layerGuestPath traduce una ruta del invitado a su sitio DENTRO del ext4 de la
// capa, para leerla con debugfs desde el anfitrión.
func layerGuestPath(path string) string {
	return "/" + layerUpperDir + "/" + strings.TrimPrefix(path, "/")
}
