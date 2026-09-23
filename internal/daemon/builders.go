package daemon

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
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/durable"
)

// CONSTRUCTORES DE IMÁGENES.
//
// Construir una imagen monta un loopback y hace chroot: es trabajo de root en el
// host del daemon. El núcleo no sabe cómo se empaqueta un servidor MCP, ni un
// entorno de sandbox, ni lo que venga después; sabe ejecutar, como root y con
// plazo, un constructor que el administrador instaló a propósito:
//
//	/usr/local/lib/kindling/builders/<nombre>   (o $KLING_BUILDERS_DIR)
//
// El protocolo:
//
//	<constructor> <dir de trabajo>
//
// En el directorio de trabajo el daemon deja request.json (la petición entera:
// name, base, grow_mb y el spec del constructor). El constructor tiene que dejar
// $KLING_ROOT/images/<name>.ext4 o <name>.layer.ext4 y salir con 0; lo que
// escriba por stdout/stderr vuelve a quien pidió la construcción. La receta la
// escribe el daemon.
//
// El constructor recibe en el entorno KLING_ROOT, KLING_IMAGE_NAME,
// KLING_BUILD_DIR y, si se pidieron, BASE_IMAGE y GROW.

var reBuilder = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func buildersDir() string {
	if d := os.Getenv("KLING_BUILDERS_DIR"); d != "" {
		return d
	}
	return "/usr/local/lib/kindling/builders"
}

// builderPath localiza el constructor y comprueba que se puede ejecutar como
// root sin regalar root: tiene que ser de root y nadie más puede escribirlo, ni
// él ni su directorio. Si no, cualquiera con escritura ahí tendría root al
// siguiente `kling add`.
func builderPath(name string) (string, error) {
	if !reBuilder.MatchString(name) {
		return "", fmt.Errorf("invalid builder name %q", name)
	}
	dir := buildersDir()
	p := filepath.Join(dir, name)
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("builder %q is not installed (looked in %s)", name, dir)
	}
	if !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		return "", fmt.Errorf("builder %s is not an executable file", p)
	}
	if os.Getenv("KLING_BUILDERS_INSECURE") == "1" {
		return p, nil // solo para pruebas y desarrollo
	}
	for _, path := range []string{p, dir} {
		fi, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return "", fmt.Errorf("%s is writable by group or others; the daemon runs builders as root", path)
		}
		if sys, ok := fi.Sys().(*syscall.Stat_t); ok && sys.Uid != 0 {
			return "", fmt.Errorf("%s is not owned by root; the daemon runs builders as root", path)
		}
	}
	return p, nil
}

func (s *Server) buildWithBuilder(w http.ResponseWriter, r *http.Request, req api.BuildImageRequest) {
	if !reName.MatchString(req.Name) {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid image name %q", req.Name))
		return
	}
	if req.Base != "" && !reName.MatchString(req.Base) {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid base image %q", req.Base))
		return
	}
	if len(req.Spec) > 0 && !json.Valid(req.Spec) {
		fail(w, http.StatusBadRequest, fmt.Errorf("spec is not valid JSON"))
		return
	}
	bin, err := builderPath(req.Builder)
	if err != nil {
		fail(w, http.StatusPreconditionFailed, err)
		return
	}

	work, err := os.MkdirTemp(filepath.Join(s.root, "build"), req.Name+".")
	if os.IsNotExist(err) {
		_ = os.MkdirAll(filepath.Join(s.root, "build"), 0o700)
		work, err = os.MkdirTemp(filepath.Join(s.root, "build"), req.Name+".")
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(work)
	b, _ := json.MarshalIndent(req, "", "  ")
	if err := os.WriteFile(filepath.Join(work, "request.json"), b, 0o600); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	// Sin cancelación del cliente, como el constructor de MCP: matar un chroot
	// con un loopback montado a medias deja el host peor que esperar.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, work)
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"KLING_ROOT="+s.root,
		"KLING_IMAGE_NAME="+req.Name,
		"KLING_BUILD_DIR="+work)
	if req.Base != "" {
		cmd.Env = append(cmd.Env, "BASE_IMAGE="+req.Base)
	}
	if req.GrowMB > 0 {
		cmd.Env = append(cmd.Env, fmt.Sprintf("GROW=%d", req.GrowMB))
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		fail(w, http.StatusInternalServerError,
			fmt.Errorf("builder %s failed: %w\n%s", req.Builder, err, strings.TrimSpace(out.String())))
		return
	}

	s.mgr.EnsureImageReadable(req.Name)
	// La base también: un constructor puede crearla la primera vez (el de
	// modelos, "llm", hace su base glibc), y sin esto la capa se construye bien
	// y la máquina no arranca porque el VMM no puede leer el suelo.
	if req.Base != "" {
		s.mgr.EnsureImageReadable(req.Base)
	}
	img := s.mgr.ImageFile(req.Name)
	if _, err := os.Stat(img); err != nil {
		if _, lerr := os.Stat(filepath.Join(s.root, "images", req.Name+".layer.ext4")); lerr != nil {
			fail(w, http.StatusInternalServerError,
				fmt.Errorf("builder %s finished but left no image %s\n%s", req.Builder, req.Name, strings.TrimSpace(out.String())))
			return
		}
	}

	rec := api.ImageRecipe{Name: req.Name, Base: req.Base, GrowMB: req.GrowMB,
		Builder: req.Builder, Spec: req.Spec, BuiltAt: time.Now(), KlingVer: Version}
	rb, _ := json.MarshalIndent(rec, "", "  ")
	// 0600: el spec de un constructor puede llevar secretos y el núcleo no sabe
	// cuáles son.
	if err := durable.Escribir(s.recipePath(req.Name), append(rb, '\n'), 0o600); err != nil {
		log.Printf("image %s: built, but couldn't save its recipe: %v", req.Name, err)
	}
	writeJSON(w, http.StatusOK, api.BuildImageResult{Name: req.Name, Path: img, Output: out.String()})
}
