package machine

// PONER UN FICHERO EN UNA IMAGEN SIN MONTARLA (macOS).
//
// En Linux, intentarPut monta el ext4 por loop. En macOS no hay mount de ext4,
// pero sí e2fsprogs de Homebrew (lo exige ya el arranque: mkfs.ext4, e2fsck), y
// debugfs -w escribe dentro del sistema de ficheros sin montarlo. Es lo mismo,
// con las mismas garantías:
//
//   - Solo toca la imagen: debugfs no ve el sistema de ficheros del host, así
//     que ningún enlace simbólico de la imagen lleva fuera de ella. Los enlaces
//     de los directorios intermedios (/bin → usr/bin) se siguen a mano y
//     SIEMPRE dentro de la imagen: un destino absoluto se toma desde su raíz y
//     un ".." no pasa de ella (path.Clean).
//   - Se escribe al lado (<nombre>.nuevo) y se cambia la entrada del directorio
//     solo cuando la copia está entera y comprobada: un ENOSPC o un fallo a
//     medias deja el fichero viejo, nunca un PID 1 truncado.
//   - Modo el pedido, dueño root:root, como en Linux. Reemplazar con rm y ln
//     conserva los demás enlaces duros del viejo (su cuenta baja en uno), igual
//     que el rename de Linux.
//   - El hueco se mide antes (errSinHueco) y putOne crece la imagen si falta.
//   - e2fsck antes y después, como alrededor del montaje.
//
// debugfs sale con 0 aunque un comando falle: los errores se leen de su
// salida, y lo escrito se comprueba leyéndolo de vuelta.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// maxEnlacesRuta es cuántos enlaces simbólicos se siguen al resolver una ruta
// dentro de la imagen, como MAXSYMLINKS en Linux.
const maxEnlacesRuta = 40

// imagenDebugfs es un ext4 que se lee y se escribe con debugfs.
type imagenDebugfs struct {
	bin, file string
}

// entrada es lo que dice debugfs stat de una ruta.
type entrada struct {
	existe bool
	tipo   string // regular, directory, symlink, ...
	enlace string // destino, si es un enlace simbólico
	// blanqueo: un whiteout de overlayfs (dispositivo de caracteres 0:0). En
	// una capa dice "esto se BORRÓ": lo de la base con ese nombre no existe.
	blanqueo bool
	// opaco: directorio con trusted.overlay.opaque=y. En una capa tapa ENTERO
	// el directorio de la base con ese nombre.
	opaco bool
}

// nombreDebugfs valida una ruta que va entre comillas en un comando de
// debugfs: su intérprete no tiene forma de escapar comillas ni saltos de línea.
func nombreDebugfs(p string) error {
	for _, r := range p {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("path %q has characters debugfs cannot take (quotes, backslashes or control characters)", p)
		}
	}
	return nil
}

func comillas(p string) string { return `"` + p + `"` }

// debugfsCmd lanza debugfs con la salida en inglés: se analiza su texto.
func (im imagenDebugfs) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, im.bin, append(args, im.file)...)
	c.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return c
}

// leer ejecuta un comando de solo lectura.
//
// Con tope (maxSalidaDebugfs): lo que se lee así se analiza como texto, y un
// enlace lento o un stat no se acercan. Lo que sí puede ser grande (el
// contenido de un fichero) va por leerFichero o sha256De.
func (im imagenDebugfs) leer(ctx context.Context, orden string) ([]byte, error) {
	out, err := salidaAcotada(im.cmd(ctx, "-R", orden), maxSalidaDebugfs)
	if err != nil {
		return nil, fmt.Errorf("debugfs %s on %s: %w", orden, filepath.Base(im.file), err)
	}
	return out, nil
}

var (
	reTipo   = regexp.MustCompile(`Type:\s+(\S+)`)
	reFast   = regexp.MustCompile(`Fast link dest:\s+"(.*)"`)
	reBloque = regexp.MustCompile(`(?m)^Block size:\s+(\d+)`)
	reLibres = regexp.MustCompile(`(?m)^Free blocks:\s+(\d+)`)
	reReserv = regexp.MustCompile(`(?m)^Reserved block count:\s+(\d+)`)
	reDisp   = regexp.MustCompile(`Device major/minor number:\s+(\S+)`)
	reOpaco  = regexp.MustCompile(`trusted\.overlay\.opaque \(\d+\) = "y"`)
)

func (im imagenDebugfs) stat(ctx context.Context, p string) (entrada, error) {
	if err := nombreDebugfs(p); err != nil {
		return entrada{}, err
	}
	out, err := im.leer(ctx, "stat "+comillas(p))
	if err != nil {
		return entrada{}, err
	}
	s := string(out)
	if !strings.Contains(s, "Inode:") {
		return entrada{}, nil
	}
	e := entrada{existe: true}
	if m := reTipo.FindStringSubmatch(s); m != nil {
		e.tipo = m[1]
	}
	if e.tipo == "character" {
		if m := reDisp.FindStringSubmatch(s); m != nil && m[1] == "00:00" {
			e.blanqueo = true
		}
	}
	e.opaco = e.tipo == "directory" && reOpaco.MatchString(s)
	if e.tipo == "symlink" {
		if m := reFast.FindStringSubmatch(s); m != nil {
			e.enlace = m[1]
		} else {
			// Enlace lento (destino de más de 60 bytes): va en un bloque de
			// datos, y cat lo lee.
			b, err := im.leer(ctx, "cat "+comillas(p))
			if err != nil {
				return entrada{}, err
			}
			e.enlace = string(b)
		}
		if e.enlace == "" {
			return entrada{}, fmt.Errorf("cannot read the symlink %s in %s", p, filepath.Base(im.file))
		}
	}
	return e, nil
}

// resolverDir sigue dir dentro de la imagen: devuelve la parte que existe, ya
// sin enlaces simbólicos, y los componentes que faltan por crear debajo.
func (im imagenDebugfs) resolverDir(ctx context.Context, dir string) (string, []string, error) {
	pendientes := partes(dir)
	cur := "/"
	for saltos := 0; len(pendientes) > 0; {
		comp := pendientes[0]
		p := path.Join(cur, comp)
		e, err := im.stat(ctx, p)
		if err != nil {
			return "", nil, err
		}
		switch {
		case !e.existe:
			return cur, pendientes, nil
		case e.tipo == "directory":
			cur, pendientes = p, pendientes[1:]
		case e.tipo == "symlink":
			if saltos++; saltos > maxEnlacesRuta {
				return "", nil, fmt.Errorf("too many symlinks resolving %s in the image", dir)
			}
			base := cur
			if strings.HasPrefix(e.enlace, "/") {
				base = "/" // desde la raíz de la IMAGEN, nunca la del host
			}
			// Clean no deja que un ".." pase de la raíz.
			nuevo := path.Clean(path.Join(append([]string{base, e.enlace}, pendientes[1:]...)...))
			cur, pendientes = "/", partes(nuevo)
		default:
			return "", nil, fmt.Errorf("%s is not a directory in the image", p)
		}
	}
	return cur, nil, nil
}

func partes(p string) []string {
	var res []string
	for _, c := range strings.Split(path.Clean("/"+p), "/") {
		if c != "" {
			res = append(res, c)
		}
	}
	return res
}

// libre son los bytes que el ext4 puede dar a un fichero nuevo, sin contar los
// reservados (lo mismo que Bavail de statfs en el montaje).
func (im imagenDebugfs) libre(ctx context.Context) (int64, error) {
	out, err := im.leer(ctx, "stats")
	if err != nil {
		return 0, err
	}
	num := func(re *regexp.Regexp) int64 {
		if m := re.FindSubmatch(out); m != nil {
			n, _ := strconv.ParseInt(string(m[1]), 10, 64)
			return n
		}
		return -1
	}
	bs, libres, reserv := num(reBloque), num(reLibres), num(reReserv)
	if bs <= 0 || libres < 0 {
		return 0, fmt.Errorf("debugfs stats on %s: cannot read the free blocks", filepath.Base(im.file))
	}
	if reserv > 0 {
		libres -= reserv
	}
	if libres < 0 {
		libres = 0
	}
	return libres * bs, nil
}

// escribir ejecuta órdenes con -w y falla si debugfs se queja de alguna.
func (im imagenDebugfs) escribir(ctx context.Context, dir string, ordenes []string) error {
	f, err := os.CreateTemp(dir, "kling-debugfs-*.cmd")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(strings.Join(ordenes, "\n") + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	out, err := im.cmd(ctx, "-w", "-f", f.Name()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("debugfs -w on %s: %v: %s", filepath.Base(im.file), err, strings.TrimSpace(string(out)))
	}
	if quejas := quejasDebugfs(out); len(quejas) > 0 {
		return fmt.Errorf("debugfs -w on %s: %s", filepath.Base(im.file), strings.Join(quejas, "; "))
	}
	return nil
}

// quejasDebugfs saca de la salida de debugfs -f las líneas que no son el eco
// de una orden ni un aviso de éxito: son sus errores.
func quejasDebugfs(out []byte) []string {
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "",
			strings.HasPrefix(l, "debugfs "), // la cabecera con la versión
			strings.HasPrefix(l, "debugfs:"), // el eco de cada orden
			strings.HasPrefix(l, "Allocated inode:"):
			continue
		}
		res = append(res, l)
	}
	return res
}

// leerFichero lee p de la imagen, como mucho max bytes: si ocupa más, error
// sin haberlo cargado entero.
func (im imagenDebugfs) leerFichero(ctx context.Context, p string, max int64) ([]byte, error) {
	if err := nombreDebugfs(p); err != nil {
		return nil, err
	}
	out, err := salidaAcotada(im.cmd(ctx, "-R", "cat "+comillas(p)), max)
	if errors.Is(err, errSalidaGrande) {
		return nil, fmt.Errorf("%s is larger than the limit of %d bytes", p, max)
	}
	if err != nil {
		return nil, fmt.Errorf("debugfs cat %s on %s: %w", p, filepath.Base(im.file), err)
	}
	return out, nil
}

// sha256De lee p de la imagen y devuelve su digest, sin cargarlo entero.
func (im imagenDebugfs) sha256De(ctx context.Context, p string) (string, error) {
	c := im.cmd(ctx, "-R", "cat "+comillas(p))
	out, err := c.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := c.Start(); err != nil {
		return "", err
	}
	h := sha256.New()
	_, cerr := io.Copy(h, out)
	if err := c.Wait(); err != nil {
		return "", fmt.Errorf("debugfs cat %s on %s: %w", p, filepath.Base(im.file), err)
	}
	if cerr != nil {
		return "", cerr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// intentarPutDebugfs es intentarPut sin montar la imagen. Mismo contrato:
// (cambió, error), errNoBridge si el fichero no está y create es false, y
// errSinHueco si no cabe al lado del viejo.
func (m *Manager) intentarPutDebugfs(ctx context.Context, image, dentroPath, src, quiero string, mode os.FileMode, create bool) (bool, error) {
	bin := debugfsBin()
	if bin == "" {
		return false, ErrNoDebugfs
	}
	im := imagenDebugfs{bin: bin, file: image}
	limpio := path.Clean("/" + dentroPath)
	if limpio == "/" {
		return false, fmt.Errorf("invalid path %q", dentroPath)
	}
	if err := nombreDebugfs(limpio); err != nil {
		return false, err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", src)
	}

	// Como antes de montar en escritura: un ext4 sucio no se toca.
	repairVolume(ctx, image)

	dir, nombre := path.Split(limpio)
	real, faltan, err := im.resolverDir(ctx, dir)
	if err != nil {
		return false, err
	}
	if err := nombreDebugfs(real); err != nil {
		return false, err
	}
	destino := path.Join(append([]string{real}, append(faltan, nombre)...)...)
	var viejo entrada
	if len(faltan) == 0 {
		if viejo, err = im.stat(ctx, destino); err != nil {
			return false, err
		}
	}
	// Un whiteout de la capa en el destino: el fichero está BORRADO en la
	// imagen. Para quien solo reemplaza, no está; para quien crea, se
	// sustituye (rm + ln, como a un fichero viejo) y el nuevo lo tapa.
	blanqueo := viejo.blanqueo && strings.HasPrefix(destino, "/"+layerUpperDir+"/")
	if (!viejo.existe || blanqueo) && !create {
		return false, errNoBridge
	}
	switch {
	case blanqueo, viejo.tipo == "", viejo.tipo == "regular", viejo.tipo == "symlink":
	default:
		return false, fmt.Errorf("%s is a %s in the image, not a file", limpio, viejo.tipo)
	}
	if viejo.tipo == "regular" {
		tengo, err := im.sha256De(ctx, destino)
		if err != nil {
			return false, err
		}
		if tengo == quiero {
			return false, nil
		}
	}
	if libre, err := im.libre(ctx); err == nil {
		if falta := faltaParaElPuente(libre, fi.Size()); falta > 0 {
			return false, errSinHueco{faltan: falta}
		}
	}

	// Una ruta del host sin comillas ni barras invertidas; la de la raíz de
	// macOS lleva espacios ("Application Support"), que las comillas cubren.
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return false, err
	}
	if err := nombreDebugfs(srcAbs); err != nil {
		return false, err
	}
	dirDestino := path.Dir(destino)
	tmp := nombre + ".nuevo"
	tmpPath := path.Join(dirDestino, tmp)

	// 1. Los directorios que falten y la copia al lado, con su modo y dueño.
	var ordenes []string
	hecho := real
	for _, c := range faltan {
		hecho = path.Join(hecho, c)
		ordenes = append(ordenes, "mkdir "+comillas(hecho))
	}
	if len(faltan) == 0 {
		if e, err := im.stat(ctx, tmpPath); err != nil {
			return false, err
		} else if e.existe {
			ordenes = append(ordenes, "rm "+comillas(tmpPath)) // resto de un intento anterior
		}
	}
	ordenes = append(ordenes,
		"cd "+comillas(dirDestino),
		"write "+comillas(srcAbs)+" "+comillas(tmp),
		fmt.Sprintf("sif %s mode 0%o", comillas(tmp), 0o100000|uint32(mode.Perm())),
		"sif "+comillas(tmp)+" uid 0",
		"sif "+comillas(tmp)+" gid 0",
	)
	if err := im.escribir(ctx, m.dirTemporalPut(), ordenes); err != nil {
		return false, err
	}
	if got, err := im.sha256De(ctx, tmpPath); err != nil || got != quiero {
		_ = im.escribir(ctx, m.dirTemporalPut(), []string{"rm " + comillas(tmpPath)})
		repairVolume(context.WithoutCancel(ctx), image)
		if err == nil {
			err = errors.New("the copy inside the image does not match the source")
		}
		return false, fmt.Errorf("writing %s: %w", limpio, err)
	}

	// 2. El cambio de entrada: solo metadatos, con la copia ya comprobada.
	ordenes = []string{"cd " + comillas(dirDestino)}
	if viejo.existe {
		ordenes = append(ordenes, "rm "+comillas(nombre))
	}
	ordenes = append(ordenes, "ln "+comillas(tmp)+" "+comillas(nombre), "unlink "+comillas(tmp))
	err = im.escribir(ctx, m.dirTemporalPut(), ordenes)
	repairVolume(context.WithoutCancel(ctx), image)
	if err != nil {
		return false, fmt.Errorf("replacing %s: %w", limpio, err)
	}
	if got, err := im.sha256De(ctx, destino); err != nil || got != quiero {
		if err == nil {
			err = errors.New("the file inside the image does not match the source")
		}
		return false, fmt.Errorf("replacing %s: %w", limpio, err)
	}
	return true, nil
}

// dirTemporalPut es donde van los guiones de debugfs: en la raíz del daemon
// (build/), no en /tmp, que en macOS es de todos.
func (m *Manager) dirTemporalPut() string {
	if m.root == "" {
		return os.TempDir()
	}
	d := filepath.Join(m.root, "build")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return os.TempDir()
	}
	return d
}
