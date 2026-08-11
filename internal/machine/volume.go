package machine

// VOLÚMENES: almacenamiento que sobrevive a la microVM.
//
// El README dice, con razón, que kindling da persistencia de SESIÓN y no
// almacenamiento duradero: el overlay de cada máquina muere con ella. Esto es lo
// segundo.
//
// POR QUÉ UN DISCO Y NO UN DIRECTORIO DEL HOST. La petición natural es "monta
// /home/juan/notas dentro". No se hace, y no por pereza:
//
//   - Firecracker no tiene virtio-fs. Compartir un directorio exigiría 9p o un
//     servidor de ficheros dentro del invitado, que es superficie nueva.
//   - Y sobre todo: el invitado se considera HOSTIL. Un directorio del host
//     montado en escritura le da un canal directo al sistema de ficheros del
//     anfitrión, que es exactamente la frontera que justifica usar microVMs en
//     vez de contenedores. Sería tirar por tierra la premisa del proyecto para
//     ahorrar un `cp`.
//
// Un volumen es un fichero-imagen ext4 en el host que se expone como TERCER
// DISCO (vdc). El host no lo monta mientras la microVM lo use, así que nunca hay
// dos sistemas escribiendo el mismo ext4 — que es corrupción garantizada.
//
// QUIÉN LO MONTA DENTRO. El puente (kling-bridge), no el init de la imagen.
// Ponerlo en el init obligaría a reconstruir todas las imágenes base; el puente
// ya viaja en cada imagen de modo stdio y es PID 1, así que puede montar antes
// de lanzar el servidor MCP. El punto de montaje viaja en la línea de comandos
// del kernel, así que es dinámico por arranque y no queda grabado en la imagen.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// reVolume acota el nombre: es un componente de ruta.
var reVolume = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// defaultVolumeMiB es el tamaño lógico por defecto. Al ser disperso, lo que
// ocupa de verdad es solo lo que se escriba.
const defaultVolumeMiB = 1024

func (m *Manager) volumesDir() string         { return filepath.Join(m.root, "volumes") }
func (m *Manager) volumePath(n string) string { return filepath.Join(m.volumesDir(), n+".ext4") }

// CreateVolume crea un volumen vacío formateado en ext4.
func (m *Manager) CreateVolume(ctx context.Context, name string, sizeMiB int) (*api.Volume, error) {
	if !reVolume.MatchString(name) {
		return nil, fmt.Errorf("nombre de volumen inválido %q: minúsculas, dígitos, guion y guion bajo", name)
	}
	if sizeMiB <= 0 {
		sizeMiB = defaultVolumeMiB
	}
	if sizeMiB > 64<<10 {
		return nil, fmt.Errorf("tamaño excesivo: %d MiB", sizeMiB)
	}
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		return nil, err
	}
	path := m.volumePath(name)
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("el volumen %q ya existe", name)
	}

	// Se construye en .tmp y se renombra: existir tiene que implicar estar
	// formateado. Un fichero a medias se montaría mal dentro del invitado y el
	// fallo aparecería allí, no aquí.
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := createOverlay(ctx, tmp, sizeMiB); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("formateando el volumen: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	m.priv.EnsureReadable(m.volumesDir())
	// El VMM corre sin privilegios y tiene que poder ESCRIBIR aquí: un volumen
	// de solo lectura no serviría de nada.
	if m.priv.UID > 0 {
		_ = os.Chown(path, m.priv.UID, -1)
	}
	return m.statVolume(name)
}

// Volumes lista los volúmenes con su ocupación real y quién los usa.
func (m *Manager) Volumes() []*api.Volume {
	entries, err := os.ReadDir(m.volumesDir())
	if err != nil {
		return nil
	}
	inUse := m.volumeUsers()

	var out []*api.Volume
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".ext4")
		if !ok || e.IsDir() {
			continue
		}
		v, err := m.statVolume(name)
		if err != nil {
			continue
		}
		v.UsedBy = inUse[name]
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Manager) statVolume(name string) (*api.Volume, error) {
	fi, err := os.Stat(m.volumePath(name))
	if err != nil {
		return nil, fmt.Errorf("no existe el volumen %q", name)
	}
	return &api.Volume{
		Name:         name,
		Path:         m.volumePath(name),
		SizeBytes:    fi.Size(),
		UsedBytes:    allocatedBytes(m.volumePath(name)),
		CreatedAt:    fi.ModTime(),
		LastModified: fi.ModTime(),
	}, nil
}

// volumeUsers dice qué máquinas tienen montado cada volumen.
func (m *Manager) volumeUsers() map[string][]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string][]string{}
	for _, mc := range m.byID {
		if mc.Volume == "" || mc.State == api.StateStopped || mc.State == api.StateFailed {
			continue
		}
		out[mc.Volume] = append(out[mc.Volume], mc.Name)
	}
	return out
}

// RemoveVolume borra un volumen, salvo que alguien lo esté usando.
//
// La comprobación no es cortesía: borrar el fichero bajo una microVM que lo
// tiene montado le corrompe el sistema de ficheros sin avisar.
func (m *Manager) RemoveVolume(name string) error {
	if users := m.volumeUsers()[name]; len(users) > 0 {
		return fmt.Errorf("el volumen %q lo usan %d máquina(s): %s",
			name, len(users), strings.Join(users, ", "))
	}
	if err := os.Remove(m.volumePath(name)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no existe el volumen %q", name)
		}
		return err
	}
	return nil
}

// resolveVolume valida la petición de montaje y devuelve la ruta en el host.
//
// Devuelve ("", "", nil) cuando no se pidió ninguno: no tener volumen es el caso
// normal, no un error.
func (m *Manager) resolveVolume(req api.RunRequest) (path, mountpoint string, err error) {
	if req.Volume == "" {
		return "", "", nil
	}
	if !reVolume.MatchString(req.Volume) {
		return "", "", fmt.Errorf("nombre de volumen inválido %q", req.Volume)
	}
	p := m.volumePath(req.Volume)
	if _, err := os.Stat(p); err != nil {
		return "", "", fmt.Errorf("no existe el volumen %q: créalo con `kling volume create %s`", req.Volume, req.Volume)
	}

	mp := req.VolumeMount
	if mp == "" {
		mp = "/data"
	}
	// La ruta viaja por la línea de comandos del kernel, donde el separador es
	// el espacio: un punto de montaje con espacios partiría el argumento y el
	// invitado montaría en otro sitio.
	if !strings.HasPrefix(mp, "/") || strings.ContainsAny(mp, " \t\"'") {
		return "", "", fmt.Errorf("punto de montaje inválido %q: ruta absoluta y sin espacios", mp)
	}
	return p, mp, nil
}

// volumeBootArg es lo que lee el puente dentro del invitado para saber dónde
// montar /dev/vdc. Vacío si no hay volumen.
func volumeBootArg(mountpoint string) string {
	if mountpoint == "" {
		return ""
	}
	return " " + api.VolumeBootParam + "=" + mountpoint
}

// touchVolume actualiza la marca de tiempo para que `kling volume ls` muestre
// cuándo se usó por última vez.
func (m *Manager) touchVolume(name string) {
	if name == "" {
		return
	}
	now := time.Now()
	_ = os.Chtimes(m.volumePath(name), now, now)
}

var _ = exec.Command // se usa desde createOverlay
