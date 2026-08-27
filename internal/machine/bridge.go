package machine

// Reemplazar el puente DENTRO de las imágenes ya construidas.
//
// El puente vive dentro de cada imagen, porque es el PID 1 del invitado. Eso
// significa que actualizar kindling en el anfitrión NO actualiza el puente de
// los servicios ya empaquetados: se quedan con el que tenían el día que se
// construyeron.
//
// La consecuencia no es una carencia de funciones, es un fallo desconcertante.
// Un puente antiguo no entiende los parámetros nuevos de la línea de comandos
// del kernel: interpretó el sufijo ":ro" como parte del nombre del directorio,
// intentó montar en escritura un disco que no lo admite, murió — y como es PID 1,
// el invitado entró en pánico. Deducir de un pánico del kernel que hay que
// reconstruir una imagen es pedir demasiado.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/juan52878911/kindling/internal/api"
)

// errNoBridge marca una imagen que no lleva puente dentro: una base mínima, no
// una de servicio. No es un fallo, es una imagen que no aplica.
var errNoBridge = errors.New("has no bridge: not a service image")

// holguraRefresh es el aire que se deja dentro de la imagen despues de meter el
// puente, para que el siguiente recambio no vuelva a tropezar por unos KB.
const holguraRefresh = 8 << 20

// errSinHueco dice que la imagen no tiene sitio DENTRO para el puente nuevo, y
// cuantos bytes le faltan. No es lo mismo que el disco del anfitrion lleno: el
// sintoma es identico ("no space left on device") y la causa, opuesta.
type errSinHueco struct{ faltan int64 }

// faltaParaElPuente dice cuantos bytes hay que anadir a la imagen para que el
// puente nuevo quepa AL LADO del viejo, mas la holgura. Cero significa que cabe.
func faltaParaElPuente(libre, puente int64) int64 {
	if falta := puente + holguraRefresh - libre; falta > 0 {
		return redondearMiB(falta)
	}
	return 0
}

// redondearMiB sube al MiB siguiente: resize2fs trabaja por bloques, y un
// tamano redondo deja ficheros legibles en un ls.
func redondearMiB(n int64) int64 {
	const mib = 1 << 20
	return (n + mib - 1) &^ (mib - 1)
}

func (e errSinHueco) Error() string {
	return fmt.Sprintf("no room inside the image: %d bytes short", e.faltan)
}

// guestBridgePath es dónde vive el puente dentro de la imagen. Lo fija
// 80-mcp-image.sh al construirla, y el entrypoint lo invoca por esa ruta.
const guestBridgePath = "usr/local/bin/kling-bridge"

// Images lista las imágenes de rootfs disponibles.
//
// Una imagen por capas se llama igual que una monolítica: lo que cambia es el
// fichero que la representa ($NAME.layer.ext4). Se recorta el sufijo largo
// PRIMERO, porque el corto también casa y dejaría "files.layer" como si fuera
// una imagen aparte.
func (m *Manager) Images() []string {
	entries, err := os.ReadDir(filepath.Join(m.root, "images"))
	if err != nil {
		return nil
	}
	vistas := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := strings.CutSuffix(e.Name(), ".layer.ext4")
		if !ok {
			if name, ok = strings.CutSuffix(e.Name(), ".ext4"); !ok {
				continue
			}
		}
		// overlay-template no es una imagen de rootfs: es el molde vacío del
		// disco de escritura de cada máquina, y no lleva puente dentro.
		if name == "overlay-template" {
			continue
		}
		vistas[name] = true
	}
	out := make([]string, 0, len(vistas))
	for name := range vistas {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// RefreshBridges pone el puente actual dentro de las imágenes indicadas.
//
// Si no se dan nombres, se hacen todas. Devuelve una fila por imagen contando
// qué pasó con ella, también las que no hicieron falta: saber que una imagen ya
// estaba al día vale tanto como saber que se actualizó.
func (m *Manager) RefreshBridges(ctx context.Context, bridge string, names []string) ([]api.BridgeRefresh, error) {
	if bridge == "" {
		return nil, fmt.Errorf("cannot find the bridge to inject: " +
			"it should be at /usr/local/lib/kindling/kling-bridge (put there by `make deploy`)")
	}
	quiero, err := fileDigest(bridge)
	if err != nil {
		return nil, fmt.Errorf("reading bridge %s: %w", bridge, err)
	}
	if len(names) == 0 {
		names = m.Images()
	}

	// Qué imágenes están en uso AHORA. Montar en escritura la imagen base de una
	// microVM viva le corrompe el sistema de ficheros por debajo — es de solo
	// lectura para ella, pero el ext4 no admite que otro lo modifique mientras
	// lo tiene montado.
	enUso := m.imageUsers()

	out := make([]api.BridgeRefresh, 0, len(names))
	for _, name := range names {
		fila := api.BridgeRefresh{Image: name}
		// Con una imagen por capas se toca la CAPA, no la base: el puente lo puso
		// ahí el build, y la base es de otros —reescribirla desde aquí cambiaría
		// bajo los pies de todos los servicios que se apoyan en ella—.
		base, layered := m.ImageBase(name)
		path, dentro := m.imagePath(name), "/"+guestBridgePath
		if layered {
			path, dentro = m.layerPath(name), layerGuestPath(guestBridgePath)
		}
		if _, err := os.Stat(path); err != nil {
			fila.Error = "does not exist"
			out = append(out, fila)
			continue
		}
		if users := enUso[name]; len(users) > 0 {
			fila.Skipped, fila.Busy = true, true
			fila.Error = fmt.Sprintf("in use by %d machine(s): %s", len(users), strings.Join(users, ", "))
			out = append(out, fila)
			continue
		}
		actualizada, err := m.refreshOne(ctx, path, dentro, bridge, quiero)
		switch {
		case errors.Is(err, errNoBridge) && layered:
			// Una capa sin puente NO es "no es una imagen de servicio": es un
			// servicio cuyo puente vive en la base, que es donde hay que
			// actualizarlo — y se actualiza una vez para todos.
			fila.Skipped = true
			fila.Error = fmt.Sprintf("its bridge comes from base %q; refresh that one", base)
		case errors.Is(err, errNoBridge):
			// Ni actualizada ni fallida: no aplica. Se informa igualmente para
			// que no parezca que se olvidó.
			fila.Skipped = true
			fila.Error = err.Error()
		case err != nil:
			fila.Error = err.Error()
		}
		fila.Updated = actualizada
		out = append(out, fila)
	}
	return out, nil
}

// imageUsers dice qué máquinas vivas usan cada imagen.
//
// Cuentan también las warm: al descongelarse vuelven a leer de la imagen base,
// que además está mapeada en el snapshot de memoria.
//
// Una máquina por capas cuenta como usuaria de DOS imágenes: su capa y la base
// sobre la que se apoya. Sin lo segundo, tocar la base mientras corre un servicio
// por capas le corrompería el sistema de ficheros por debajo — que es
// exactamente lo que esta cuenta existe para impedir.
func (m *Manager) imageUsers() map[string][]string {
	// Se copian los dos campos que hacen falta, no el puntero: sacar máquinas
	// vivas del candado es lo que hace List() y por el mismo motivo.
	type uso struct{ image, name string }
	m.mu.RLock()
	vivas := make([]uso, 0, len(m.byID))
	for _, mc := range m.byID {
		if mc.Image == "" || mc.State == api.StateStopped || mc.State == api.StateFailed {
			continue
		}
		vivas = append(vivas, uso{mc.Image, mc.Name})
	}
	m.mu.RUnlock()

	// La resolución toca disco (stat de la capa, lectura de la receta), así que
	// va FUERA del candado: m.mu protege el mapa de máquinas, y sostenerlo
	// mientras se lee el disco bloquea a List() y a persist().
	out := map[string][]string{}
	for _, u := range vivas {
		out[u.image] = append(out[u.image], u.name)
		if base, ok := m.ImageBase(u.image); ok {
			out[base] = append(out[base], u.name)
		}
	}
	return out
}

// refreshOne monta la imagen, cambia el puente si hace falta y la desmonta.
//
// dentro es la ruta del puente DENTRO de ese ext4: en una imagen monolítica la
// del invitado, y en una capa la misma bajo /upper, que es donde el build deja
// el delta.
//
// Devuelve si hubo cambio. Una imagen que ya tenía el puente correcto se
// desmonta sin tocarla: reescribirla por gusto la ensuciaría y desharía su
// dispersión en disco.
func (m *Manager) refreshOne(ctx context.Context, image, dentroPath, bridge, quiero string) (bool, error) {
	// Las imágenes se construyen ajustadas al byte, así que una que ya lleva
	// puente NO tiene sitio para su propio recambio: durante el renombrado
	// atómico conviven las dos copias. Crecer es la única salida que conserva
	// esa atomicidad; escribir encima dejaría, si algo falla a mitad, un PID 1
	// truncado — un invitado que no arranca en vez de un error.
	//
	// Se reintenta porque un ext4 NO entrega como espacio libre todo lo que se
	// le añade: parte se va en metadatos. Medido aquí: pedir 12 MB dejó 11,12
	// libres, un 7% menos, y la primera pasada se quedó a 0,5 MB de caber.
	// `reservaMetadatos` cubre ese margen; el bucle es la garantía de que un
	// anfitrión con otra geometría de bloques converge igual.
	const intentos = 3
	for i := 0; ; i++ {
		cambio, err := m.intentarRefresh(ctx, image, dentroPath, bridge, quiero)
		var sinHueco errSinHueco
		if !errors.As(err, &sinHueco) || i == intentos {
			return cambio, err
		}
		log.Printf("image %s: growing it by %d MiB; it was built with no room for a new bridge",
			filepath.Base(image), conReservaDeMetadatos(sinHueco.faltan)/(1<<20))
		if err := crecerImagen(ctx, image, sinHueco.faltan); err != nil {
			return false, err
		}
	}
}

// conReservaDeMetadatos infla lo que se pide para que el hueco que el ext4
// ENTREGA sea el que hacía falta.
//
// El 1/8 no es el 7% medido sino su inverso con margen: si el sistema de
// ficheros se queda una fracción p, hay que pedir 1/(1-p), no 1+p. Con 1/8 se
// tolera una pérdida de hasta el 11%. El MiB suelto cubre los déficits
// pequeños, donde el porcentaje no da ni para un grupo de bloques.
func conReservaDeMetadatos(falta int64) int64 {
	return redondearMiB(falta + falta/8 + (1 << 20))
}

// crecerImagen agranda el ext4 de una imagen. El fichero se estira y luego se
// le dice al sistema de ficheros que ocupe el hueco nuevo.
func crecerImagen(ctx context.Context, image string, extra int64) error {
	fi, err := os.Stat(image)
	if err != nil {
		return err
	}
	if err := os.Truncate(image, fi.Size()+conReservaDeMetadatos(extra)); err != nil {
		return fmt.Errorf("growing %s: %w", filepath.Base(image), err)
	}
	// resize2fs se niega a tocar un ext4 que no venga de un fsck reciente.
	repairVolume(ctx, image)
	if out, err := exec.CommandContext(ctx, "resize2fs", image).CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs %s: %v: %s", filepath.Base(image), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// espacioLibre devuelve los bytes disponibles DENTRO del sistema de ficheros
// montado en mnt.
func espacioLibre(mnt string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mnt, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (m *Manager) intentarRefresh(ctx context.Context, image, dentroPath, bridge, quiero string) (bool, error) {
	// Antes de montar en ESCRITURA. Montar así un ext4 sucio es como se corrompió
	// una imagen en este proyecto, y el síntoma fue un pánico del invitado.
	repairVolume(ctx, image)

	mnt, err := os.MkdirTemp("", "kling-refresh-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(mnt)

	if out, err := exec.CommandContext(ctx, "mount", "-o", "loop", image, mnt).CombinedOutput(); err != nil {
		return false, fmt.Errorf("mounting: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// El desmontaje va en defer y NO al final del cuerpo: cualquier retorno
	// intermedio dejaría la imagen montada, y una imagen montada en escritura es
	// exactamente lo que no debe pasar. Después, un e2fsck que deje constancia
	// de que quedó limpia.
	montada := true
	desmontar := func() {
		if !montada {
			return
		}
		montada = false
		_ = exec.Command("sync").Run()
		if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
			// Con -l al menos se desengancha; el sistema de ficheros se cierra
			// cuando se suelte la última referencia.
			_ = exec.Command("umount", "-l", mnt).Run()
			_ = out
		}
		repairVolume(context.WithoutCancel(ctx), image)
	}
	defer desmontar()

	dentro := filepath.Join(mnt, dentroPath)
	tengo, err := fileDigest(dentro)
	if os.IsNotExist(err) {
		// Una imagen SIN puente no es una imagen de servicio: es una base
		// mínima, cuyo entrypoint no invoca ningún puente. Inyectarle uno no
		// haría nada salvo engordarla, así que se deja como está.
		return false, errNoBridge
	}
	if err == nil && tengo == quiero {
		return false, nil
	}

	// Se escribe al lado y se renombra, en vez de sobre el fichero.
	//
	// El renombrado dentro de un mismo sistema de ficheros es atómico: o está el
	// puente viejo o el nuevo, nunca uno a medias. Escribir encima dejaría, si
	// algo falla a mitad, una imagen cuyo PID 1 es un binario truncado — y eso
	// no da un error, da un invitado que no arranca.
	// ¿Cabe? El renombrado atómico exige que quepan las dos copias a la vez, así
	// que el hueco se mide ANTES de escribir: un ENOSPC a media copia deja un
	// .nuevo trunco dentro de una imagen que ya no se puede arreglar sin crecer.
	if libre, err := espacioLibre(mnt); err == nil {
		if fi, err := os.Stat(bridge); err == nil {
			if falta := faltaParaElPuente(libre, fi.Size()); falta > 0 {
				desmontar()
				return false, errSinHueco{faltan: falta}
			}
		}
	}

	tmp := dentro + ".nuevo"
	if err := copyFile(bridge, tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("copying bridge: %w", err)
	}
	if err := os.Rename(tmp, dentro); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("replacing bridge: %w", err)
	}
	desmontar()
	return true, nil
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	// Sync antes de cerrar: el renombrado que viene después es atómico respecto
	// a los metadatos, pero no garantiza que los DATOS estén en el disco.
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// imageHasBridge dice si la imagen lleva puente dentro, SIN montarla.
//
// debugfs lee el ext4 en frío. Montar en escritura solo para mirar si existe un
// fichero es la operación que ya corrompió una imagen en este proyecto, y para
// esta pregunta ni siquiera hace falta.
//
// Con una imagen por capas hay dos sitios donde mirar, y se miran los dos: el
// puente lo pone el build en la capa, pero una base con el puente ya horneado
// también vale. Basta con que esté en uno.
//
// El error va aparte del booleano a propósito: "no lo sé" —debugfs ausente— no
// es lo mismo que "no lo lleva", y quien pregunta decide si falla abierto.
func imageHasBridge(ctx context.Context, base, layer string) (bool, error) {
	if layer != "" {
		has, err := hasFile(ctx, layer, layerGuestPath(guestBridgePath))
		if err != nil || has {
			return has, err
		}
	}
	return hasFile(ctx, base, "/"+guestBridgePath)
}

// hasFile mira con debugfs si un fichero existe dentro de un ext4 sin montarlo.
func hasFile(ctx context.Context, image, path string) (bool, error) {
	bin, err := exec.LookPath("debugfs")
	if err != nil {
		// En Debian vive en /sbin, que no siempre está en el PATH de un
		// servicio de systemd.
		for _, p := range []string{"/sbin/debugfs", "/usr/sbin/debugfs"} {
			if fi, serr := os.Stat(p); serr == nil && !fi.IsDir() {
				bin, err = p, nil
				break
			}
		}
	}
	if err != nil {
		return false, fmt.Errorf("cannot find debugfs (comes with e2fsprogs): %w", err)
	}
	out, err := exec.CommandContext(ctx, bin, "-R", "stat "+path, image).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("debugfs on %s: %v: %s", image, err, strings.TrimSpace(string(out)))
	}
	// debugfs sale con 0 aunque el fichero no exista: la respuesta está en la
	// salida. Un stat con éxito imprime la línea "Inode: NNN Type: ...".
	return strings.Contains(string(out), "Inode:"), nil
}
