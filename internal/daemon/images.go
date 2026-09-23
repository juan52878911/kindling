package daemon

// Construcción de imágenes MCP desde el daemon.
//
// Existe porque `scripts/80-mcp-image.sh` monta un loopback, hace chroot e
// instala paquetes: necesita root en el host con KVM. El CLI corre en el
// portátil, así que no puede ejecutarlo — tiene que pedírselo a quien ya es
// root aquí.
//
// SUPERFICIE PRIVILEGIADA. Esto amplía lo que se puede hacer a través del
// socket, y aunque el socket ya equivale a root en este host (se pueden arrancar
// kernels arbitrarios), eso no es excusa para no validar: los campos acaban en
// `apk add $PKGS` y `npm install -g $NPM` SIN comillas dentro del script, donde
// un valor con guiones por delante es inyección de argumentos, y el nombre
// acaba siendo la ruta `$ROOT/images/$NAME.ext4`, donde un `../` se sale del
// directorio. Se valida con lista blanca, que es la única que no se queda corta.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// buildTimeout: instalar node y un paquete npm en un chroot va lento, y en un
// host modesto con la imagen creciendo 768 MB puede pasar de un minuto.
const buildTimeout = 15 * time.Minute

var (
	// Nombre de imagen: es un componente de ruta y un nombre de servicio.
	reName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	// Paquete de apk.
	reAPK = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]*$`)
	// Paquete de npm, con ámbito y versión opcionales.
	// Los nombres de PyPI admiten punto, guion y guion bajo, y el
	// especificador de versión va con ==, >= o ~=. Nada de eso puede empezar
	// por guion: acabaría en `pip install $PIP` sin comillas, donde un valor
	// que empieza por guion es un flag.
	rePIP = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*((==|>=|<=|~=|!=)[a-zA-Z0-9._*+-]+)?$`)

	reNPM = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*(@[a-zA-Z0-9._~+-]+)?$`)
	// Variable de entorno horneada: KEY=value. La clave con la forma estricta
	// de un identificador de shell; el valor, cualquier cosa MENOS saltos de
	// línea y NUL — el script la escribe en el entrypoint con `printf %q`, así
	// que el resto de caracteres viaja inerte, pero un salto de línea antes de
	// llegar ahí partiría otras cosas y no hay ningún valor legítimo que lo
	// necesite.
	reEnv = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[^\x00\r\n]*$`)
)

func (s *Server) handleBuildImage(w http.ResponseWriter, r *http.Request) {
	if !construirImagenes {
		fail(w, http.StatusNotImplemented, errors.New("this daemon can't build images: building needs root, loop "+
			"devices and chroot on Linux. Build the image on a Linux host and copy it here:\n"+
			"  kling images copy <name> -from ssh://user@linux-host"))
		return
	}
	var req api.BuildImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if req.Builder == "" {
		// Hasta v0.5 el daemon empaquetaba servidores MCP por su cuenta. Ahora
		// lo hace el constructor "mcp" de kindling-mcp.
		fail(w, http.StatusBadRequest, fmt.Errorf("missing builder: images are built by an installed builder "+
			"(\"base\" from kindling, \"mcp\" from kindling-mcp); see docs/api.md"))
		return
	}
	s.buildWithBuilder(w, r, req)
}

// recipePath es dónde vive la receta de una imagen.
func (s *Server) recipePath(name string) string {
	return filepath.Join(s.root, "images", name+".recipe.json")
}

// handleImages enumera las imágenes de rootfs construidas: nombre, tamaño en
// disco, si se guardó su receta y cuántos snapshots dorados salieron de cada
// una. Ese último dato es el que dice qué imagen se puede retirar sin dejar
// servicios sin base.
//
// Con imágenes por capas hay una segunda forma de estar en uso: ser la BASE de
// otras. Se cuenta aparte de los snapshots porque significa otra cosa —quitar
// una base se lleva por delante servicios que solo guardan su delta— y porque el
// tamaño que se enseña de cada capa es el suyo, sin la base, que es compartida.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	usedBy := map[string]int{}
	for _, snap := range s.mgr.Snapshots() {
		usedBy[snap.Image]++
	}
	names := s.mgr.Images()

	// Primera pasada: quién se apoya en quién. Una base con capas encima no se
	// puede retirar aunque no tenga snapshots propios, y eso hay que poder verlo
	// ANTES de borrar nada.
	base := map[string]string{}
	layers := map[string]int{}
	for _, name := range names {
		if b, ok := s.mgr.ImageBase(name); ok {
			base[name] = b
			layers[b]++
		}
	}

	out := make([]api.Image, 0, len(names))
	for _, name := range names {
		img := api.Image{Name: name, UsedBy: usedBy[name],
			Base: base[name], Layers: layers[name]}
		// El fichero que se mide es el de la imagen: su capa si va por capas, el
		// ext4 entero si es monolítica.
		if fi, err := os.Stat(s.mgr.ImageFile(name)); err == nil {
			// Tamaño lógico y, aparte, el REALMENTE asignado en disco (bloques ×
			// 512): con ext4 disperso difieren, y solo el segundo dice cuánto se
			// recupera al borrar.
			img.SizeBytes = fi.Size()
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				img.DiskBytes = st.Blocks * 512
			} else {
				img.DiskBytes = fi.Size()
			}
		}
		if _, err := os.Stat(s.recipePath(name)); err == nil {
			img.HasRecipe = true
		}
		out = append(out, img)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleImageRecipe(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !reName.MatchString(name) {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid name %q", name))
		return
	}
	b, err := os.ReadFile(s.recipePath(name))
	if err != nil {
		fail(w, http.StatusNotFound, fmt.Errorf(
			"no recipe for %q. Images built before recipes were saved don't have one; "+
				"the command is still inside, in /entrypoint", name))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}
