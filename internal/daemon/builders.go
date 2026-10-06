package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/durable"
	"github.com/juan52878911/kindling/pkg/lazyre"
)

// CONSTRUCTORES DE IMÁGENES.
//
// Construir una imagen monta un loopback y hace chroot: es trabajo de root en el
// host del daemon. El núcleo no sabe cómo se empaqueta un servidor MCP, ni un
// entorno de sandbox, ni lo que venga después; sabe ejecutar, como root y con
// plazo, un constructor que el administrador instaló a propósito (los del
// núcleo que no necesitan root, como "oci", con un usuario sin privilegios:
// ver builders_sinroot.go):
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
// escribe el daemon; el constructor puede dejar al lado de request.json un
// recipe.json (api.BuildRecipeHints: la base que hizo, techos de CPU, pila
// IPv6 del invitado y lo que apuntó de lo construido) que el daemon lleva a
// la receta.
//
// El constructor recibe en el entorno KLING_ROOT, KLING_IMAGE_NAME,
// KLING_BUILD_DIR y, si se pidieron, BASE_IMAGE y GROW. Los aislados
// (constructoresAislados) reciben además KLING_OUT_DIR, donde dejan la imagen
// en vez de en images/, y sin root KLING_CACHE_DIR, su caché.

var reBuilder = lazyre.New(`^[a-z][a-z0-9-]{0,31}$`)

// constructoresSinRoot son los constructores del núcleo escritos en Go de
// punta a punta (sin loop, chroot ni root): corren también en el daemon de
// macOS, y si no están instalados en el directorio de constructores el
// daemon se ejecuta a sí mismo como `kling builder <nombre>`.
var constructoresSinRoot = map[string]bool{"android": true, "debian": true, "oci": true}

// maxRecipeHints es el tamaño máximo del recipe.json que deja un constructor.
const maxRecipeHints = 256 << 10

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
	var binArgs []string
	if err != nil && constructoresSinRoot[req.Builder] {
		// Del núcleo y en Go: el propio binario del daemon lo lleva.
		if self, serr := os.Executable(); serr == nil {
			bin, binArgs, err = self, []string{"builder", req.Builder}, nil
		}
	}
	if err != nil {
		fail(w, http.StatusPreconditionFailed, err)
		return
	}

	// Los aislados dejan la imagen en out/ y, con usuario de construcción,
	// corren con él (builders_sinroot.go).
	aislado := constructoresAislados[req.Builder]
	var u *usuarioConstructor
	if aislado {
		u = s.constructor
	}
	dueño := uint32(os.Geteuid())
	if u != nil {
		dueño = u.UID
		s.muConstructor.Lock()
		defer s.muConstructor.Unlock()
		barrerProcesos(u.UID)
		defer barrerProcesos(u.UID)
	}
	b, _ := json.MarshalIndent(req, "", "  ")
	work, err := prepararTrabajo(s.root, req.Name, b, u)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(work)

	var cache string
	if u != nil {
		if cache, err = prepararCache(s.root, u); err != nil {
			fail(w, http.StatusInternalServerError, fmt.Errorf("builder cache: %w", err))
			return
		}
	}

	// Sin cancelación del cliente, como el constructor de MCP: matar un chroot
	// con un loopback montado a medias deja el host peor que esperar.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), buildTimeout)
	defer cancel()
	cmd := comandoConstructor(ctx, bin, binArgs, s.root, work, req, aislado, u, cache)
	out := filepath.Join(work, "out")
	var salida bytes.Buffer
	cmd.Stdout, cmd.Stderr = &salida, &salida
	if err := cmd.Run(); err != nil {
		fail(w, http.StatusInternalServerError,
			fmt.Errorf("builder %s failed: %w\n%s", req.Builder, err, strings.TrimSpace(salida.String())))
		return
	}
	if aislado {
		if u != nil {
			barrerProcesos(u.UID) // nada del constructor sigue vivo mientras se valida
		}
		if _, err := adoptarSalida(out, filepath.Join(s.root, "images"), req.Name, dueño); err != nil {
			fail(w, http.StatusInternalServerError, fmt.Errorf("builder %s: %w", req.Builder, err))
			return
		}
	}

	hints, err := readRecipeHints(filepath.Join(work, "recipe.json"))
	if err != nil {
		fail(w, http.StatusInternalServerError, fmt.Errorf("builder %s: %w\n%s", req.Builder, err, strings.TrimSpace(salida.String())))
		return
	}
	base := req.Base
	if hints.Base != "" {
		base = hints.Base
	}

	s.mgr.EnsureImageReadable(req.Name)
	// La base también: un constructor puede crearla la primera vez (el de
	// modelos, "llm", hace su base glibc), y sin esto la capa se construye bien
	// y la máquina no arranca porque el VMM no puede leer el suelo.
	if base != "" {
		s.mgr.EnsureImageReadable(base)
	}
	img := s.mgr.ImageFile(req.Name)
	if _, err := os.Stat(img); err != nil {
		if _, lerr := os.Stat(filepath.Join(s.root, "images", req.Name+".layer.ext4")); lerr != nil {
			fail(w, http.StatusInternalServerError,
				fmt.Errorf("builder %s finished but left no image %s\n%s", req.Builder, req.Name, strings.TrimSpace(salida.String())))
			return
		}
	}

	rec := api.ImageRecipe{Name: req.Name, Base: base, GrowMB: req.GrowMB,
		Builder: req.Builder, Spec: req.Spec, BuiltAt: time.Now(), KlingVer: Version,
		CPUPct: hints.CPUPct, CPUPctPerVCPU: hints.CPUPctPerVCPU, GuestIPv6Stack: hints.GuestIPv6Stack,
		Built: hints.Built}
	rb, _ := json.MarshalIndent(rec, "", "  ")
	// 0600: el spec de un constructor puede llevar secretos y el núcleo no sabe
	// cuáles son.
	if err := durable.Escribir(s.recipePath(req.Name), append(rb, '\n'), 0o600); err != nil {
		log.Printf("image %s: built, but couldn't save its recipe: %v", req.Name, err)
	}
	writeJSON(w, http.StatusOK, api.BuildImageResult{Name: req.Name, Path: img, Output: salida.String()})
}

// comandoConstructor prepara el proceso del constructor: su entorno y, con
// usuario de construcción, su identidad (u.credencial) y un entorno de lista
// blanca en vez del del daemon.
func comandoConstructor(ctx context.Context, bin string, binArgs []string, root, work string,
	req api.BuildImageRequest, aislado bool, u *usuarioConstructor, cache string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, append(append([]string{}, binArgs...), work)...)
	cmd.Dir = work
	cmd.Env = os.Environ()
	if u != nil {
		cmd.Env = append(entornoConstructor(work), "KLING_CACHE_DIR="+cache, "KLING_BUILD_LIMITS=1")
		cmd.SysProcAttr = u.credencial()
	}
	cmd.Env = append(cmd.Env,
		"KLING_ROOT="+root,
		"KLING_IMAGE_NAME="+req.Name,
		"KLING_BUILD_DIR="+work)
	if aislado {
		cmd.Env = append(cmd.Env, "KLING_OUT_DIR="+filepath.Join(work, "out"))
	}
	if req.Base != "" {
		cmd.Env = append(cmd.Env, "BASE_IMAGE="+req.Base)
	}
	if req.GrowMB > 0 {
		cmd.Env = append(cmd.Env, fmt.Sprintf("GROW=%d", req.GrowMB))
	}
	return cmd
}

// readRecipeHints lee el recipe.json que puede dejar un constructor
// (api.BuildRecipeHints). Sin fichero no es error: la receta es la de siempre.
// Sin seguir enlaces y solo un fichero regular: el directorio de trabajo puede
// ser de un constructor sin privilegios, y un recipe.json que apunte a la
// receta 0600 de otra imagen la colaría en ésta.
func readRecipeHints(p string) (api.BuildRecipeHints, error) {
	var h api.BuildRecipeHints
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return h, nil
	}
	if err != nil {
		return h, fmt.Errorf("recipe.json: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return h, fmt.Errorf("recipe.json is not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxRecipeHints+1))
	if err != nil {
		return h, err
	}
	if len(b) > maxRecipeHints {
		return h, fmt.Errorf("recipe.json over %d bytes", maxRecipeHints)
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, fmt.Errorf("recipe.json: %w", err)
	}
	if h.Base != "" && !reName.MatchString(h.Base) {
		return h, fmt.Errorf("recipe.json: invalid base %q", h.Base)
	}
	if h.CPUPct < 0 || h.CPUPct > 100*256 || h.CPUPctPerVCPU < 0 || h.CPUPctPerVCPU > 100 {
		return h, fmt.Errorf("recipe.json: cpu_pct out of range")
	}
	return h, nil
}
