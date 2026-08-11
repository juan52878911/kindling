package machine

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	if err := createVolumeImage(ctx, tmp, sizeMiB); err != nil {
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
		v.UsedBy = inUse[name].all()
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

// volumeUsers dice qué máquinas tienen montado cada volumen, y cómo.
//
// Distinguir lectores de escritores no es un adorno: es lo que decide si una
// máquina más puede montarlo. Ver resolveVolume.
type volumeUse struct {
	readers []string
	// writers es una lista y no un solo nombre aunque solo pueda haber uno: si
	// alguna vez hay dos, es una anomalía que hay que VER, no una que colapsar
	// en un nombre. Reportar de menos convierte un estado imposible en un
	// informe tranquilizador.
	writers []string
}

func (u volumeUse) all() []string {
	out := make([]string, 0, len(u.writers)+len(u.readers))
	for _, w := range u.writers {
		out = append(out, w+" (escritura)")
	}
	return append(out, u.readers...)
}

func (m *Manager) volumeUsers() map[string]volumeUse {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]volumeUse{}
	for _, mc := range m.byID {
		if mc.Volume == "" || mc.State == api.StateStopped || mc.State == api.StateFailed {
			continue
		}
		u := out[mc.Volume]
		if mc.VolumeReadOnly {
			u.readers = append(u.readers, mc.Name)
		} else {
			u.writers = append(u.writers, mc.Name)
		}
		out[mc.Volume] = u
	}
	return out
}

// RemoveVolume borra un volumen, salvo que alguien lo esté usando.
//
// La comprobación no es cortesía: borrar el fichero bajo una microVM que lo
// tiene montado le corrompe el sistema de ficheros sin avisar.
func (m *Manager) RemoveVolume(name string) error {
	if users := m.volumeUsers()[name].all(); len(users) > 0 {
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
func (m *Manager) resolveVolume(req api.RunRequest) (path, mountpoint string, readOnly bool, err error) {
	if req.Volume == "" {
		return "", "", false, nil
	}
	if !reVolume.MatchString(req.Volume) {
		return "", "", false, fmt.Errorf("nombre de volumen inválido %q", req.Volume)
	}
	p := m.volumePath(req.Volume)
	if _, err := os.Stat(p); err != nil {
		return "", "", false, fmt.Errorf("no existe el volumen %q: créalo con `kling volume create %s`", req.Volume, req.Volume)
	}

	// UN ESCRITOR, O MUCHOS LECTORES. Nunca las dos cosas.
	//
	// No es una política, es física. Un ext4 no admite dos sistemas montándolo
	// en ESCRITURA: cada uno cachea metadatos que el otro no ve, y el resultado
	// es corrupción — comprobado, y el síntoma es un EBADMSG al leer desde la
	// siguiente microVM, que no se parece en nada a la causa.
	//
	// En LECTURA no hay tal problema: si nadie escribe, los bloques no cambian
	// y cada invitado puede leerlos por su cuenta. Eso es lo que hace posible
	// una biblioteca compartida —un volumen con paquetes que puebla una máquina
	// y consumen muchas— sin duplicarla en cada imagen.
	//
	// Se comprueba aquí, en el camino de arranque, y no solo al borrar: quien
	// monta es tan peligroso como quien borra.
	u := m.volumeUsers()[req.Volume]
	switch {
	case len(u.writers) > 0:
		// Hay escritor: no entra nadie más, ni siquiera a leer. Un lector vería
		// un sistema de ficheros cambiando bajo sus pies, con los metadatos que
		// leyó ya obsoletos.
		return "", "", false, fmt.Errorf("el volumen %q lo tiene montado %s en ESCRITURA.\n"+
			"Mientras alguien escribe no puede entrar nadie más, ni a leer: párala antes",
			req.Volume, strings.Join(u.writers, ", "))
	case !req.VolumeReadOnly && len(u.readers) > 0:
		// Quiere escribir y ya hay lectores: tampoco, por la misma razón.
		return "", "", false, fmt.Errorf("el volumen %q lo están leyendo %d máquina(s): %s.\n"+
			"Para escribir hace falta tenerlo en exclusiva; o móntalo con -volume-ro",
			req.Volume, len(u.readers), strings.Join(u.readers, ", "))
	}

	mp := req.VolumeMount
	if mp == "" {
		mp = "/data"
	}
	// La ruta viaja por la línea de comandos del kernel, donde el separador es
	// el espacio: un punto de montaje con espacios partiría el argumento y el
	// invitado montaría en otro sitio.
	if !strings.HasPrefix(mp, "/") || strings.ContainsAny(mp, " \t\"'") {
		return "", "", false, fmt.Errorf("punto de montaje inválido %q: ruta absoluta y sin espacios", mp)
	}
	return p, mp, req.VolumeReadOnly, nil
}

// volumeBootArg es lo que lee el puente dentro del invitado para saber dónde
// montar /dev/vdc. Vacío si no hay volumen.
func volumeBootArg(mountpoint string, readOnly bool) string {
	if mountpoint == "" {
		return ""
	}
	// El modo va PEGADO al punto de montaje con dos puntos, y no como parámetro
	// aparte, para que el puente no pueda leer uno sin el otro: montar en
	// escritura algo que se pidió de solo lectura corrompe lo que comparten los
	// demás lectores.
	if readOnly {
		return " " + api.VolumeBootParam + "=" + mountpoint + ":ro"
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

// createVolumeImage formatea un volumen en ext4 CON journal.
//
// Esta es la única diferencia con createOverlay, y no es un detalle: un overlay
// es desechable —muere con la máquina, y por eso ahorrarse el journal es gratis—
// mientras que un volumen tiene que sobrevivir a que maten el VMM con SIGKILL,
// que para el invitado es indistinguible de un corte de corriente.
//
// Sin journal, ese corte deja el sistema de ficheros incoherente y sin nada que
// reproducir. El síntoma no se parece en nada a la causa: el siguiente arranque
// monta sin quejarse y el primer read devuelve EBADMSG. Comprobado.
func createVolumeImage(ctx context.Context, path string, sizeMiB int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(sizeMiB) << 20); err != nil {
		f.Close()
		return err
	}
	f.Close()

	// -E nodiscard preserva la dispersión: un volumen de 1 GiB recién creado
	// ocupa kilobytes en el anfitrión y crece según se usa.
	out, err := exec.CommandContext(ctx, "mkfs.ext4",
		"-q", "-F", "-E", "nodiscard", "-L", "kling-vol", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// repairVolume repasa el sistema de ficheros antes de entregárselo al invitado.
//
// El journal cubre los cortes normales, pero no cubre dos cosas: los volúmenes
// creados antes de que los volúmenes llevasen journal, y el caso de un anfitrión
// que se apagó a la brava a media escritura. Un e2fsck aquí cuesta milisegundos
// sobre un volumen sano y es la diferencia entre arrancar y un pánico del kernel
// invitado en los demás casos.
//
// No devuelve error a propósito: si e2fsck no está instalado, o el volumen está
// tan dañado que no se puede arreglar solo, seguir adelante y dejar que falle el
// montaje dentro del invitado da un mensaje más útil que negarse a arrancar.
func repairVolume(ctx context.Context, path string) {
	// -p arregla solo lo que no admite ambigüedad. Sin -p, e2fsck haría
	// preguntas a un stdin que no existe y se quedaría colgado.
	cmd := exec.CommandContext(ctx, "e2fsck", "-p", "-f", path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	// e2fsck sale con 1 cuando ARREGLÓ algo: es un éxito, no un fallo.
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		log.Printf("volumen %s: reparado antes de montar: %s",
			filepath.Base(path), strings.TrimSpace(string(out)))
		return
	}
	log.Printf("volumen %s: e2fsck no pudo repasarlo (%v); lo monto igualmente",
		filepath.Base(path), err)
}

// flushVolume le pide al invitado que vacíe su caché al disco.
//
// Se llama justo antes de matar el VMM. El daemon mata con SIGKILL —no hay otra
// forma de garantizar que un invitado hostil se pare— y eso significa que lo que
// el servidor MCP acabase de escribir sigue en la caché de páginas del invitado
// y no ha tocado el fichero del anfitrión. Esta llamada es la que hace que un
// volumen persistente persista de verdad.
//
// El plazo es corto y los fallos se ignoran: si el invitado no contesta, ya
// estaba roto, y retrasar el apagado por él no arregla nada.
func (m *Manager) flushVolume(mc *api.Machine) {
	if mc == nil || mc.Volume == "" || mc.IP == "" || mc.State != api.StateRunning {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := "http://" + net.JoinHostPort(mc.IP, strconv.Itoa(api.GuestPort)) + "/volume/sync"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("volumen %s: el invitado no vació la caché antes de morir: %v", mc.Volume, err)
		return
	}
	resp.Body.Close()
}
