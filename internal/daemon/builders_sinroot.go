package daemon

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CONSTRUCTORES SIN ROOT.
//
// El constructor "oci" baja de internet y parsea tars hostiles (internal/oci,
// internal/ext4). No necesita root para nada, así que en Linux, con el daemon
// como root, corre con un usuario propio sin privilegios (-build-as,
// KLING_BUILD_AS, por defecto "kindling-build"): un bug del parser aterriza en
// ese usuario, no en root.
//
// Es un usuario aparte del de Firecracker (-run-as) a propósito. Con el mismo
// uid, un VMM comprometido podría reescribir la caché de blobs y los builds en
// curso (y meter así su código en las imágenes de los demás), y un constructor
// comprometido podría mandar señales o ptrace a los VMM vivos y escribir en los
// volúmenes, que son del usuario del VMM. Cada uno con el suyo, ninguno toca lo
// del otro.
//
// Lo que el constructor puede escribir, y nada más:
//
//	<root>/build/<name>.XXXX/   su directorio de trabajo (de root 0711 build/)
//	<root>/cache/builder/       su caché (la de blobs OCI en oci/), 0700
//
// La imagen la deja en <trabajo>/out/ (KLING_OUT_DIR); el daemon la valida (sin
// seguir enlaces, fichero regular, del usuario de construcción, un solo enlace
// duro), le quita el dueño y la mueve a images/. El recipe.json se lee igual,
// sin seguir enlaces. El entorno es una lista blanca: el del daemon puede llevar
// secretos. Las construcciones sin root van de una en una y, al acabar, el
// daemon mata cualquier proceso que haya quedado con ese uid: lo que un
// constructor comprometido deje en segundo plano no llega a la siguiente.
//
// Sin usuario (macOS, daemon sin root, usuario inexistente) corre como hasta
// ahora, con el uid del daemon.

// constructoresAislados son los constructores del núcleo que dejan su salida
// en KLING_OUT_DIR y corren con el usuario de construcción. android y debian
// también son Go puro, pero reutilizan y escriben bases y recetas de images/
// (CheckBaseOwner lee recetas 0600 de root): siguen con root por ahora.
var constructoresAislados = map[string]bool{"oci": true}

// usuarioConstructor es el usuario sin privilegios de los constructores.
type usuarioConstructor struct {
	Nombre   string
	UID, GID uint32
}

// lookupUser es variable para los tests.
var lookupUser = user.Lookup

// resolverUsuarioConstructor busca el usuario de construcción. nil y un aviso
// si no se puede usar: entonces el constructor corre como el daemon, como
// siempre.
func resolverUsuarioConstructor(nombre string, uidVMM int, hayVMM bool) (*usuarioConstructor, string) {
	if nombre == "" || runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return nil, ""
	}
	u, err := lookupUser(nombre)
	if err != nil {
		return nil, fmt.Sprintf("the builder user %q doesn't exist, so the oci builder runs as root "+
			"(sudo useradd --system --no-create-home --shell /usr/sbin/nologin %s, or pass -build-as/KLING_BUILD_AS)",
			nombre, nombre)
	}
	uid, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil || uid == 0 || gid == 0 {
		return nil, fmt.Sprintf("the builder user %q is root or has no numeric uid/gid: the oci builder runs as root", nombre)
	}
	if hayVMM && int(uid) == uidVMM {
		return nil, fmt.Sprintf("the builder user %q is the Firecracker user: give the builders their own "+
			"(sudo useradd --system --no-create-home --shell /usr/sbin/nologin kindling-build); the oci builder runs as root", nombre)
	}
	return &usuarioConstructor{Nombre: nombre, UID: uint32(uid), GID: uint32(gid)}, ""
}

// SetBuildUser fija el usuario de los constructores sin root. Se llama antes
// de Listen.
func (s *Server) SetBuildUser(nombre string) {
	uidVMM, hay := s.mgr.UIDVMM()
	u, aviso := resolverUsuarioConstructor(nombre, uidVMM, hay)
	if aviso != "" {
		log.Printf("SECURITY WARNING: %s", aviso)
	}
	s.constructor = u
}

// credencial es la identidad con la que nace el proceso del constructor: su
// uid y su gid, sin grupos suplementarios (el daemon tiene los de root). Al
// cambiar de uid el núcleo le quita todas las capacidades.
func (u *usuarioConstructor) credencial() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: u.UID, Gid: u.GID, Groups: []uint32{}}}
}

// entornoPermitido es lo único del entorno del daemon que llega a un
// constructor sin root: lo que necesita para bajar (proxy, certificados), dónde
// está el agente y la fecha reproducible.
var entornoPermitido = []string{
	"PATH", "TZ", "LANG", "LC_ALL", "SOURCE_DATE_EPOCH",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"KLING_LIB_DIR", "KLING_GUEST_AGENT", "KLING_GUEST_AGENT_amd64", "KLING_GUEST_AGENT_arm64",
}

func entornoConstructor(work string) []string {
	env := []string{"HOME=" + work, "TMPDIR=" + work}
	for _, k := range entornoPermitido {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// prepararTrabajo crea el directorio de trabajo de una construcción con su
// request.json. Con usuario de construcción, build/ queda de root 0711 (se
// atraviesa, no se lista ni se escribe) y el directorio de trabajo es suyo.
func prepararTrabajo(root, name string, req []byte, u *usuarioConstructor) (string, error) {
	build := filepath.Join(root, "build")
	if err := os.MkdirAll(build, 0o700); err != nil {
		return "", err
	}
	if u != nil {
		fi, err := os.Lstat(build)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%s is not a directory", build)
		}
		if os.Geteuid() == 0 {
			if err := os.Lchown(build, 0, 0); err != nil {
				return "", err
			}
		}
		if err := os.Chmod(build, 0o711); err != nil {
			return "", err
		}
	}
	work, err := os.MkdirTemp(build, name+".")
	if err != nil {
		return "", err
	}
	// Mientras el directorio es de root nadie más puede tocarlo: las rutas son
	// seguras. El chown del directorio va el último.
	rq := filepath.Join(work, "request.json")
	if err := os.WriteFile(rq, req, 0o600); err != nil {
		os.RemoveAll(work)
		return "", err
	}
	if u != nil {
		for _, p := range []string{rq, work} {
			if err := os.Lchown(p, int(u.UID), int(u.GID)); err != nil {
				os.RemoveAll(work)
				return "", err
			}
		}
	}
	return work, nil
}

// prepararCache deja <root>/cache/builder, la caché del usuario de
// construcción: suya y 0700. Aparte de <root>/cache/oci, que siguen usando
// los constructores que corren como root (debian, android): root no debe
// escribir en un directorio de un usuario sin privilegios (le plantaría
// enlaces), ni fiarse de lo que haya dejado. La primera vez enlaza (hard
// link) los blobs de la caché de root: siguen siendo de root y de solo lectura
// para él, el cliente OCI los comprueba por sha256 al usarlos, y reimportar
// lo que ya se bajó como root no vuelve a bajar nada.
func prepararCache(root string, u *usuarioConstructor) (string, error) {
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", err
	}
	if fi, err := os.Lstat(cache); err != nil {
		return "", err
	} else if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", cache)
	} else if fi.Mode().Perm()&0o001 == 0 {
		// Que se pueda atravesar, sin listar.
		if err := os.Chmod(cache, fi.Mode().Perm()|0o001); err != nil {
			return "", err
		}
	}
	d := filepath.Join(cache, "builder")
	fi, err := os.Lstat(d)
	switch {
	case os.IsNotExist(err):
		if err := os.Mkdir(d, 0o700); err != nil {
			return "", err
		}
		migrarCacheOCI(filepath.Join(cache, "oci"), filepath.Join(d, "oci"), u)
	case err != nil:
		return "", err
	case !fi.IsDir():
		return "", fmt.Errorf("%s is not a directory", d)
	}
	if err := os.Lchown(d, int(u.UID), int(u.GID)); err != nil {
		return "", err
	}
	return d, os.Chmod(d, 0o700)
}

// migrarCacheOCI enlaza los blobs de la caché OCI de root en la nueva. Corre
// con el directorio nuevo aún de root (nadie más puede tocarlo); lo que falle
// se baja otra vez, sin más.
func migrarCacheOCI(vieja, nueva string, u *usuarioConstructor) {
	entradas, err := os.ReadDir(filepath.Join(vieja, "sha256"))
	if err != nil {
		return
	}
	dst := filepath.Join(nueva, "sha256")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return
	}
	n := 0
	for _, e := range entradas {
		if !e.Type().IsRegular() || strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		if os.Link(filepath.Join(vieja, "sha256", e.Name()), filepath.Join(dst, e.Name())) == nil {
			n++
		}
	}
	for _, p := range []string{nueva, dst} {
		_ = os.Lchown(p, int(u.UID), int(u.GID))
	}
	if n > 0 {
		log.Printf("builder cache: linked %d blob(s) from %s", n, vieja)
	}
}

// adoptarSalida mueve a images/ la imagen que dejó un constructor en out/:
// <name>.ext4 o <name>.layer.ext4, lo que haya. Cada una tiene que ser un
// fichero regular (sin seguir enlaces), de uid y con un solo enlace duro; sale
// de root (si el daemon lo es) y 0644, como la dejaban los constructores; el
// grupo y los permisos del VMM los pone luego EnsureImageReadable. Devuelve
// cuántas movió.
func adoptarSalida(out, images, name string, uid uint32) (int, error) {
	fi, err := os.Lstat(out)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !fi.IsDir() {
		return 0, fmt.Errorf("the builder's output %s is not a directory", out)
	}
	n := 0
	for _, f := range []string{name + ".ext4", name + ".layer.ext4"} {
		ok, err := adoptarFichero(filepath.Join(out, f), filepath.Join(images, f), uid)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

func adoptarFichero(src, dst string, uid uint32) (bool, error) {
	// O_NONBLOCK: abrir una FIFO plantada no se queda esperando.
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("builder output %s: %w", filepath.Base(src), err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return false, fmt.Errorf("builder output %s is not a regular file", fi.Name())
	case !ok || st.Uid != uid:
		return false, fmt.Errorf("builder output %s is not owned by the builder (uid %d)", fi.Name(), uid)
	case uint64(st.Nlink) != 1:
		return false, fmt.Errorf("builder output %s has other hard links", fi.Name())
	}
	if os.Geteuid() == 0 && uid != 0 {
		if err := f.Chown(0, 0); err != nil {
			return false, err
		}
	}
	if err := f.Chmod(0o644); err != nil {
		return false, err
	}
	if err := os.Rename(src, dst); err != nil {
		return false, err
	}
	// Lo que se movió tiene que ser lo que se comprobó.
	if got, err := os.Lstat(dst); err != nil || !os.SameFile(got, fi) {
		os.Remove(dst)
		return false, fmt.Errorf("builder output %s changed while it was being moved", fi.Name())
	}
	return true, nil
}

// barrerProcesos mata los procesos que queden con el uid del constructor. Va
// por /proc y repite hasta una pasada limpia: un proceso que se bifurca sin
// parar no se escapa entre dos lecturas. Sin /proc (macOS) no hace nada.
func barrerProcesos(uid uint32) {
	for pasada := 0; pasada < 50; pasada++ {
		entradas, err := os.ReadDir("/proc")
		if err != nil {
			return
		}
		vivos := 0
		for _, e := range entradas {
			pid, err := strconv.Atoi(e.Name())
			if err != nil || pid <= 1 || pid == os.Getpid() {
				continue
			}
			if procesoDeUID(filepath.Join("/proc", e.Name(), "status"), uid) {
				vivos++
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		if vivos == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	log.Printf("WARNING: processes of builder uid %d keep coming back", uid)
}

// procesoDeUID dice si alguno de los uid (real, efectivo, guardado, de
// ficheros) de un /proc/<pid>/status es uid.
func procesoDeUID(status string, uid uint32) bool {
	f, err := os.Open(status)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if campos, ok := strings.CutPrefix(sc.Text(), "Uid:"); ok {
			for _, c := range strings.Fields(campos) {
				if v, err := strconv.ParseUint(c, 10, 32); err == nil && uint32(v) == uid {
					return true
				}
			}
			return false
		}
	}
	return false
}
