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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/internal/api"
)

// errNoBridge marca una imagen que no lleva puente dentro: una base mínima, no
// una de servicio. No es un fallo, es una imagen que no aplica.
var errNoBridge = errors.New("has no bridge: not a service image")

// guestBridgePath es dónde vive el puente dentro de la imagen. Lo fija
// 80-mcp-image.sh al construirla, y el entrypoint lo invoca por esa ruta.
const guestBridgePath = "usr/local/bin/kling-bridge"

// Images lista las imágenes de rootfs disponibles.
func (m *Manager) Images() []string {
	entries, err := os.ReadDir(filepath.Join(m.root, "images"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".ext4")
		// overlay-template no es una imagen de rootfs: es el molde vacío del
		// disco de escritura de cada máquina, y no lleva puente dentro.
		if !ok || e.IsDir() || name == "overlay-template" {
			continue
		}
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
		path := m.imagePath(name)
		if _, err := os.Stat(path); err != nil {
			fila.Error = "does not exist"
			out = append(out, fila)
			continue
		}
		if users := enUso[name]; len(users) > 0 {
			fila.Skipped = true
			fila.Error = fmt.Sprintf("in use by %d machine(s): %s", len(users), strings.Join(users, ", "))
			out = append(out, fila)
			continue
		}
		actualizada, err := m.refreshOne(ctx, path, bridge, quiero)
		switch {
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
func (m *Manager) imageUsers() map[string][]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string][]string{}
	for _, mc := range m.byID {
		if mc.Image == "" || mc.State == api.StateStopped || mc.State == api.StateFailed {
			continue
		}
		out[mc.Image] = append(out[mc.Image], mc.Name)
	}
	return out
}

// refreshOne monta la imagen, cambia el puente si hace falta y la desmonta.
//
// Devuelve si hubo cambio. Una imagen que ya tenía el puente correcto se
// desmonta sin tocarla: reescribirla por gusto la ensuciaría y desharía su
// dispersión en disco.
func (m *Manager) refreshOne(ctx context.Context, image, bridge, quiero string) (bool, error) {
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

	dentro := filepath.Join(mnt, guestBridgePath)
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
// El error va aparte del booleano a propósito: "no lo sé" —debugfs ausente— no
// es lo mismo que "no lo lleva", y quien pregunta decide si falla abierto.
func imageHasBridge(ctx context.Context, image string) (bool, error) {
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
	out, err := exec.CommandContext(ctx, bin, "-R", "stat /"+guestBridgePath, image).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("debugfs on %s: %v: %s", image, err, strings.TrimSpace(string(out)))
	}
	// debugfs sale con 0 aunque el fichero no exista: la respuesta está en la
	// salida. Un stat con éxito imprime la línea "Inode: NNN Type: ...".
	return strings.Contains(string(out), "Inode:"), nil
}
