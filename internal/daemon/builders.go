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
	"reflect"
	"strings"
	"sync"
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
// ver builders_sinroot.go; y esos los hace el propio daemon, ver
// resolverConstructor):
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
// macOS, y el daemon se ejecuta a sí mismo como `kling builder <nombre>`
// (resolverConstructor).
var constructoresSinRoot = map[string]bool{"android": true, "debian": true, "oci": true}

// maxRecipeHints es el tamaño máximo del recipe.json que deja un constructor.
const maxRecipeHints = 256 << 10

// buildersDirPorDefecto es variable para los tests.
var buildersDirPorDefecto = "/usr/local/lib/kindling/builders"

func buildersDir() string {
	if d := os.Getenv("KLING_BUILDERS_DIR"); d != "" {
		return d
	}
	return buildersDirPorDefecto
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

// resolverConstructor dice qué ejecutar para el constructor name: un
// ejecutable y los argumentos que van antes del directorio de trabajo.
//
// Los del núcleo sin root (constructoresSinRoot) los hace el PROPIO binario
// del daemon (`<daemon> builder <name>`), aunque haya uno instalado en
// /usr/local/lib/kindling/builders: el instalado lanza el kling del sistema,
// y un daemon privado o recién compilado construiría con OTRO binario que el
// suyo. Solo un KLING_BUILDERS_DIR puesto a propósito manda sobre eso (para
// probar un constructor a mano); si no lo tiene, también el propio binario.
// El resto, siempre el instalado (builderPath).
func resolverConstructor(name string) (string, []string, error) {
	if constructoresSinRoot[name] && os.Getenv("KLING_BUILDERS_DIR") == "" {
		if self, err := os.Executable(); err == nil {
			return self, []string{"builder", name}, nil
		}
	}
	bin, err := builderPath(name)
	if err != nil && constructoresSinRoot[name] {
		if self, serr := os.Executable(); serr == nil {
			return self, []string{"builder", name}, nil
		}
	}
	return bin, nil, err
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
	bin, binArgs, err := resolverConstructor(req.Builder)
	if err != nil {
		fail(w, http.StatusPreconditionFailed, err)
		return
	}

	// En fila con las demás construcciones de este nombre: dos a la vez
	// escribirían el mismo images/<name> y la segunda lo renombraría bajo
	// las máquinas que ya arrancó la primera. Si mientras se esperaba otra
	// petición igual ya la hizo (dos `kling run -image` de la misma
	// referencia), se devuelve esa. Solo la hecha DURANTE la espera: pedir
	// otra vez lo mismo más tarde sigue siendo reconstruir (`-rebuild`, una
	// etiqueta que se movió).
	llegada := time.Now()
	soltarNombre := s.bloquearNombreImagen(req.Name)
	defer soltarNombre()
	if s.construidaDesde(req, llegada) {
		writeJSON(w, http.StatusOK, api.BuildImageResult{Name: req.Name, Path: s.mgr.ImageFile(req.Name),
			Output: fmt.Sprintf("image %s was built by a concurrent request with the same spec: reused\n", req.Name)})
		return
	}

	// Los aislados dejan la imagen en out/ y, con usuario de construcción,
	// corren con él (builders_sinroot.go).
	aislado := constructoresAislados[req.Builder]
	var u *usuarioConstructor
	if aislado {
		u = s.constructor
	}
	if u != nil && binArgs != nil && !atravesable(bin, u) {
		fail(w, http.StatusPreconditionFailed, fmt.Errorf("the builder user %q can't execute the daemon binary %s "+
			"(it or a parent directory doesn't let it through, e.g. /root): put kling under /usr/local or /srv, "+
			"or install the builder in KLING_BUILDERS_DIR", u.Nombre, bin))
		return
	}
	dueño := uint32(os.Geteuid())
	if u != nil {
		dueño = u.UID
		s.muConstructor.Lock()
		defer s.muConstructor.Unlock()
		// En fila también frente a otros daemons del host con el mismo
		// usuario (uno privado de pruebas junto al de systemd): el barrido
		// mataría el constructor en curso del otro.
		soltar, err := bloquearConstructorHost(u.UID)
		if err != nil {
			fail(w, http.StatusInternalServerError, fmt.Errorf("builder lock: %w", err))
			return
		}
		defer soltar()
		barrerProcesos(u.UID)
	}
	b, _ := json.MarshalIndent(req, "", "  ")
	work, err := prepararTrabajo(s.root, req.Name, b, u)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	defer func() {
		// Primero lo que el constructor dejara vivo, después su directorio:
		// al revés, sus hijos seguirían escribiendo mientras se borra.
		if u != nil {
			barrerProcesos(u.UID)
		}
		os.RemoveAll(work)
	}()

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

// cerrojoImagen es el cerrojo de construir un nombre de imagen; n, cuántos
// lo esperan o lo tienen (para soltar la entrada del mapa).
type cerrojoImagen struct {
	mu sync.Mutex
	n  int
}

// bloquearNombreImagen toma el cerrojo de construir name y devuelve con qué
// soltarlo.
func (s *Server) bloquearNombreImagen(name string) func() {
	s.muConstruyendo.Lock()
	if s.construyendo == nil {
		s.construyendo = map[string]*cerrojoImagen{}
	}
	c := s.construyendo[name]
	if c == nil {
		c = &cerrojoImagen{}
		s.construyendo[name] = c
	}
	c.n++
	s.muConstruyendo.Unlock()
	c.mu.Lock()
	return func() {
		c.mu.Unlock()
		s.muConstruyendo.Lock()
		if c.n--; c.n == 0 {
			delete(s.construyendo, name)
		}
		s.muConstruyendo.Unlock()
	}
}

// construidaDesde dice si la imagen de req ya está, construida desde desde
// con el mismo constructor, spec, base y tamaño: lo que hizo otra petición
// igual mientras ésta esperaba su turno.
func (s *Server) construidaDesde(req api.BuildImageRequest, desde time.Time) bool {
	b, err := os.ReadFile(s.recipePath(req.Name))
	if err != nil {
		return false
	}
	var rec api.ImageRecipe
	if json.Unmarshal(b, &rec) != nil || rec.BuiltAt.Before(desde) || rec.Builder != req.Builder ||
		rec.GrowMB != req.GrowMB || (req.Base != "" && rec.Base != req.Base) || !mismoJSON(rec.Spec, req.Spec) {
		return false
	}
	if _, err := os.Stat(s.mgr.ImageFile(req.Name)); err != nil {
		return false
	}
	return true
}

// mismoJSON compara dos JSON por su valor, no por sus bytes: la receta guarda
// el spec reindentado. Vacío cuenta como null.
func mismoJSON(a, b json.RawMessage) bool {
	var va, vb any
	if len(a) > 0 && json.Unmarshal(a, &va) != nil {
		return false
	}
	if len(b) > 0 && json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
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
