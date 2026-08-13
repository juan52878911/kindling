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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/api"
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
)

// imageScript localiza scripts/80-mcp-image.sh.
//
// Se busca en varios sitios porque el daemon se despliega como un binario
// suelto: `make deploy` deja el script en /usr/local/lib/kindling, pero en
// desarrollo se trabaja desde el repositorio.
func (s *Server) imageScript() (string, error) {
	candidates := []string{
		os.Getenv("KLING_IMAGE_SCRIPT"),
		"/usr/local/lib/kindling/80-mcp-image.sh",
		"/usr/lib/kindling/80-mcp-image.sh",
		"scripts/80-mcp-image.sh",
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return filepath.Abs(p)
		}
	}
	return "", fmt.Errorf("no encuentro 80-mcp-image.sh; despliégalo con `make deploy` " +
		"o apunta a él con KLING_IMAGE_SCRIPT")
}

// validateBuild comprueba la petición antes de que nada llegue a un shell.
func validateBuild(r api.BuildImageRequest) error {
	if !reName.MatchString(r.Name) {
		return fmt.Errorf("nombre inválido %q: minúsculas, dígitos, guion y guion bajo, hasta 64", r.Name)
	}
	if r.Base != "" && !reName.MatchString(r.Base) {
		return fmt.Errorf("imagen base inválida %q", r.Base)
	}
	for _, p := range r.Packages {
		if !reAPK.MatchString(p) {
			return fmt.Errorf("paquete apk inválido %q", p)
		}
	}
	for _, p := range r.NPM {
		if !reNPM.MatchString(p) {
			return fmt.Errorf("paquete npm inválido %q", p)
		}
	}
	for _, p := range r.PIP {
		if !rePIP.MatchString(p) {
			return fmt.Errorf("paquete pip inválido %q", p)
		}
	}
	if len(r.Cmd) == 0 {
		return fmt.Errorf("falta el comando que arranca el servidor MCP")
	}
	for _, a := range r.Cmd {
		// El script mete cada argumento con `printf %q`, así que no hay
		// inyección de shell; pero un salto de línea partiría el entrypoint
		// generado en dos y un NUL lo truncaría.
		if strings.ContainsAny(a, "\x00\n\r") {
			return fmt.Errorf("el comando no puede llevar saltos de línea ni bytes nulos")
		}
	}
	if r.GrowMB < 0 || r.GrowMB > 8192 {
		return fmt.Errorf("crecimiento fuera de rango: %d MB", r.GrowMB)
	}
	return nil
}

// buildScriptArgs traduce un BuildImageRequest a los argumentos de 80-mcp-image.sh
// (sin el propio script como argv[0]). El daemon siempre construye en modo stdio.
// Los flags van ANTES de `--`, que separa el comando del servidor.
func buildScriptArgs(req api.BuildImageRequest) []string {
	args := []string{"stdio", req.Name}
	if len(req.Packages) > 0 {
		args = append(args, "-p", strings.Join(req.Packages, " "))
	}
	if len(req.NPM) > 0 {
		args = append(args, "-n", strings.Join(req.NPM, " "))
	}
	if len(req.PIP) > 0 {
		args = append(args, "-P", strings.Join(req.PIP, " "))
	}
	if req.Bundle {
		args = append(args, "-bundle")
	}
	args = append(args, "--")
	return append(args, req.Cmd...)
}

func (s *Server) handleBuildImage(w http.ResponseWriter, r *http.Request) {
	var req api.BuildImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := validateBuild(req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	script, err := s.imageScript()
	if err != nil {
		fail(w, http.StatusPreconditionFailed, err)
		return
	}

	argv := append([]string{script}, buildScriptArgs(req)...)

	// bash y no /bin/sh: en Debian /bin/sh es dash, y el script usa `set -o
	// pipefail`, arrays y `printf %q`. Con dash falla en la línea 26 con un
	// error que no se parece en nada a la causa.
	sh, err := exec.LookPath("bash")
	if err != nil {
		fail(w, http.StatusPreconditionFailed,
			fmt.Errorf("hace falta bash para empaquetar: 80-mcp-image.sh usa arrays y pipefail"))
		return
	}

	// El contexto de la PETICIÓN no manda aquí, a propósito.
	//
	// exec.CommandContext mata el proceso con SIGKILL cuando su contexto se
	// cancela, y este script monta un loopback, hace bind de /proc y ejecuta un
	// chroot. SIGKILL no dispara su `trap cleanup EXIT`, así que un cliente que
	// se rinde —un timeout, un Ctrl-C— deja la imagen MONTADA en escritura y a
	// medio construir. A partir de ahí, cada arranque de esa imagen la corrompe
	// más: el invitado ve un ext4 con needs_recovery y el kernel entra en
	// pánico con EUCLEAN. Pasó de verdad, y costó un rato entenderlo.
	//
	// Que el cliente cuelgue es molesto; dejar el host con un montaje huérfano
	// y una imagen inservible, no. Se acota solo por buildTimeout.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), buildTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, sh, argv...)
	cmd.Dir = s.root
	cmd.Env = append(os.Environ(), "KLING_ROOT="+s.root)
	if req.Base != "" {
		cmd.Env = append(cmd.Env, "BASE_IMAGE="+req.Base)
	}
	if req.GrowMB > 0 {
		cmd.Env = append(cmd.Env, fmt.Sprintf("GROW=%d", req.GrowMB))
	}
	if b := s.bridgePath(); b != "" {
		cmd.Env = append(cmd.Env, "BRIDGE="+b)
	}

	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		// La salida del script es lo único que explica POR QUÉ falló —un apk
		// que no existe, un npm sin red—, así que viaja entera al cliente.
		fail(w, http.StatusInternalServerError,
			fmt.Errorf("la construcción falló: %w\n%s", err, strings.TrimSpace(out.String())))
		return
	}

	// La receta se guarda JUNTO a la imagen, no dentro.
	//
	// Dentro haría falta montarla para leerla, que es una operación de root con
	// riesgo de corromper el ext4 — justo lo que hay que evitar para responder
	// a "¿cómo se construyó esto?". Al lado, es un fichero de texto.
	//
	// Un fallo al escribirla NO tumba la construcción: la imagen ya está hecha y
	// funciona; perder la receta es peor documentación, no un error.
	if err := s.saveRecipe(req); err != nil {
		log.Printf("imagen %s: construida, pero no pude guardar su receta: %v", req.Name, err)
	}

	writeJSON(w, http.StatusOK, api.BuildImageResult{
		Name:   req.Name,
		Path:   filepath.Join(s.root, "images", req.Name+".ext4"),
		Output: out.String(),
	})
}

// bridgePath localiza el puente que hay que inyectar en la imagen.
//
// El script lo busca en ./kling-bridge relativo a su directorio de trabajo, que
// aquí es la raíz de datos y no el repositorio.
func (s *Server) bridgePath() string {
	for _, p := range []string{
		os.Getenv("KLING_BRIDGE"),
		"/usr/local/lib/kindling/kling-bridge",
		"/usr/local/bin/kling-bridge",
	} {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

func (s *Server) handleRefreshBridges(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Images []string `json:"images,omitempty"`
	}
	// Cuerpo vacío = todas las imágenes. No es un error.
	_ = json.NewDecoder(r.Body).Decode(&req)

	// context.WithoutCancel: si el cliente corta a media faena, abandonar aquí
	// dejaría una imagen montada en escritura. Es exactamente la cadena que ya
	// corrompió una imagen en este proyecto y acabó en un pánico del invitado.
	res, err := s.mgr.RefreshBridges(context.WithoutCancel(r.Context()), s.bridgePath(), req.Images)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// recipePath es dónde vive la receta de una imagen.
func (s *Server) recipePath(name string) string {
	return filepath.Join(s.root, "images", name+".recipe.json")
}

// saveRecipe deja constancia de cómo se construyó la imagen.
//
// Se escribe en .tmp y se renombra: una receta a medias sería peor que ninguna,
// porque parece una respuesta.
func (s *Server) saveRecipe(r api.BuildImageRequest) error {
	rec := api.ImageRecipe{
		Name: r.Name, Base: r.Base, Packages: r.Packages, NPM: r.NPM,
		PIP: r.PIP, Cmd: r.Cmd, GrowMB: r.GrowMB, Bundle: r.Bundle, BuiltAt: time.Now(),
		KlingVer: Version,
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.recipePath(r.Name) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.recipePath(r.Name))
}

func (s *Server) handleImageRecipe(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !reName.MatchString(name) {
		fail(w, http.StatusBadRequest, fmt.Errorf("nombre inválido %q", name))
		return
	}
	b, err := os.ReadFile(s.recipePath(name))
	if err != nil {
		fail(w, http.StatusNotFound, fmt.Errorf(
			"no hay receta de %q. Las imágenes construidas antes de que se guardaran no la tienen; "+
				"el comando sigue estando dentro, en /entrypoint", name))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (s *Server) handleImageCapabilities(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !reName.MatchString(name) {
		fail(w, http.StatusBadRequest, fmt.Errorf("nombre inválido %q", name))
		return
	}
	caps, err := s.mgr.ImageCapabilities(r.Context(), name)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	// Sin capacidades declaradas se devuelve un objeto vacío, no un 404: quien
	// llama trata "no declara nada" como "sin necesidades especiales".
	if caps == nil {
		caps = &api.Capabilities{}
	}
	writeJSON(w, http.StatusOK, caps)
}
