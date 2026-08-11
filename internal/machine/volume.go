package machine

import (
	"context"
	"fmt"
	"io"
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

// defaultVolumeMount es dónde se monta un volumen si no se dice otra cosa.
const defaultVolumeMount = "/data"

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
		Name:      name,
		Path:      m.volumePath(name),
		SizeBytes: fi.Size(),
		// Lo asignado de verdad, que con un fichero disperso no tiene nada que
		// ver con el tamaño lógico: un volumen de 2 GiB recién creado ocupa
		// kilobytes.
		UsedBytes: allocatedBytes(m.volumePath(name)),
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
		if mc.State == api.StateStopped || mc.State == api.StateFailed {
			continue
		}
		for _, v := range mc.Volumes {
			u := out[v.Name]
			if v.ReadOnly {
				u.readers = append(u.readers, mc.Name)
			} else {
				u.writers = append(u.writers, mc.Name)
			}
			out[v.Name] = u
		}
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

// resolvedVolume es un volumen ya validado y listo para engancharse.
type resolvedVolume struct {
	name     string
	path     string // en el anfitrión
	mount    string // dentro del invitado
	readOnly bool
}

// resolveVolumes valida la petición de montaje y devuelve las rutas del host.
//
// Devuelve nil cuando no se pidió ninguno: no tener volumen es el caso normal,
// no un error.
//
// UN ESCRITOR, O MUCHOS LECTORES. Nunca las dos cosas.
//
// No es una política, es física. Un ext4 no admite dos sistemas montándolo en
// ESCRITURA: cada uno cachea metadatos que el otro no ve, y el resultado es
// corrupción — comprobado, y el síntoma es un EBADMSG al leer desde la siguiente
// microVM, que no se parece en nada a la causa.
//
// En LECTURA no hay tal problema: si nadie escribe, los bloques no cambian y
// cada invitado puede leerlos por su cuenta. Eso es lo que hace posible una
// biblioteca compartida —un volumen con paquetes que puebla una máquina y
// consumen muchas— sin duplicarla en cada imagen.
//
// Se comprueba aquí, en el camino de arranque, y no solo al borrar: quien monta
// es tan peligroso como quien borra.
func (m *Manager) resolveVolumes(req api.RunRequest) ([]resolvedVolume, error) {
	want := req.VolumeSet()
	if len(want) == 0 {
		return nil, nil
	}
	if len(want) > api.MaxVolumes {
		return nil, fmt.Errorf("son %d volúmenes y el máximo es %d: cada uno es un disco más",
			len(want), api.MaxVolumes)
	}

	inUse := m.volumeUsers()
	vistos := map[string]bool{}
	puntos := map[string]bool{}
	out := make([]resolvedVolume, 0, len(want))

	for _, v := range want {
		if !reVolume.MatchString(v.Name) {
			return nil, fmt.Errorf("nombre de volumen inválido %q", v.Name)
		}
		// El mismo volumen dos veces en la MISMA microVM es un doble montaje
		// tan malo como entre dos máquinas distintas, y además se le escaparía
		// a la comprobación de abajo: la máquina aún no está en byID.
		if vistos[v.Name] {
			return nil, fmt.Errorf("el volumen %q aparece dos veces: un disco no se monta dos veces", v.Name)
		}
		vistos[v.Name] = true

		p := m.volumePath(v.Name)
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("no existe el volumen %q: créalo con `kling volume create %s`", v.Name, v.Name)
		}

		u := inUse[v.Name]
		switch {
		case len(u.writers) > 0:
			// Hay escritor: no entra nadie más, ni siquiera a leer. Un lector
			// vería un sistema de ficheros cambiando bajo sus pies, con los
			// metadatos que leyó ya obsoletos.
			return nil, fmt.Errorf("el volumen %q lo tiene montado %s en ESCRITURA.\n"+
				"Mientras alguien escribe no puede entrar nadie más, ni a leer: párala antes",
				v.Name, strings.Join(u.writers, ", "))
		case !v.ReadOnly && len(u.readers) > 0:
			// El "cómo" va con la sintaxis exacta: un error que dice qué pasa
			// pero no qué escribir obliga a ir a buscarlo a la documentación.
			return nil, fmt.Errorf("el volumen %q lo están leyendo %d máquina(s): %s.\n"+
				"Para escribir hace falta tenerlo en exclusiva; o léelo tú también:  -volume %s:ro",
				v.Name, len(u.readers), strings.Join(u.readers, ", "), v.Name)
		}

		mp := v.Mount
		if mp == "" {
			mp = defaultVolumeMount
		}
		// Las rutas viajan por la línea de comandos del kernel separadas por
		// comas, y el separador de argumentos es el espacio: cualquiera de los
		// dos dentro de un punto de montaje partiría la lista y el invitado
		// montaría los volúmenes cruzados, o en ningún sitio.
		if !strings.HasPrefix(mp, "/") || strings.ContainsAny(mp, " \t\"',") {
			return nil, fmt.Errorf("punto de montaje inválido %q: ruta absoluta, sin espacios ni comas", mp)
		}
		// Dos volúmenes en el mismo punto: el segundo taparía al primero, que
		// quedaría montado y fuera de alcance.
		if puntos[mp] {
			return nil, fmt.Errorf("dos volúmenes montados en %s: el segundo taparía al primero", mp)
		}
		puntos[mp] = true

		out = append(out, resolvedVolume{name: v.Name, path: p, mount: mp, readOnly: v.ReadOnly})
	}
	return out, nil
}

// attachments convierte lo resuelto en lo que se graba en la máquina y en el
// snapshot. El orden se conserva: es el orden de los discos.
func attachments(vols []resolvedVolume) []api.VolumeAttachment {
	if len(vols) == 0 {
		return nil
	}
	out := make([]api.VolumeAttachment, len(vols))
	for i, v := range vols {
		// El DriveID se estampa aquí, en el nacimiento, y coincide con el que
		// boot() registra en SetDrive. A partir de este momento viaja con la
		// máquina y con cada snapshot que salga de ella.
		out[i] = api.VolumeAttachment{Name: v.name, Mount: v.mount,
			ReadOnly: v.readOnly, DriveID: volumeDriveID(i)}
	}
	return out
}

// execBootArg enciende /exec dentro del invitado.
//
// Solo lo pone el daemon, y solo para las microVMs de un solo uso que pueblan un
// volumen: el invitado no puede concedérselo a sí mismo porque la línea de
// comandos del kernel la escribe quien arranca la máquina.
func execBootArg(enabled bool) string {
	if !enabled {
		return ""
	}
	return " " + api.ExecBootParam + "=1"
}

// volumeBootArg es lo que lee el puente dentro del invitado para saber dónde
// montar cada disco. Vacío si no hay volúmenes.
//
// Formato: kling.volume=/data,/libs:ro — los puntos de montaje separados por
// comas, EN EL MISMO ORDEN en que se engancharon los discos, y el modo pegado a
// cada uno. El orden es todo el contrato: el puente cuenta desde /dev/vdc, así
// que la posición i de esta lista es el disco i. Y el modo va pegado, y no como
// parámetro aparte, para que sea imposible leer uno sin el otro: montar en
// escritura lo que se pidió de solo lectura corrompería lo que leen los demás.
func volumeBootArg(vols []api.VolumeAttachment) string {
	if len(vols) == 0 {
		return ""
	}
	specs := make([]string, len(vols))
	for i, v := range vols {
		specs[i] = v.Mount
		if v.ReadOnly {
			specs[i] += ":ro"
		}
	}
	return " " + api.VolumeBootParam + "=" + strings.Join(specs, ",")
}

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
// releaseVolumes le pide al invitado que DESMONTE sus volúmenes.
//
// Se llama antes de congelar un snapshot dorado. Un snapshot congela la memoria
// del invitado, y ahí dentro va la caché de ext4: superbloque, mapas de bloques,
// posición del journal. El fichero del volumen NO se copia al snapshot —sigue
// siendo el del anfitrión y sigue cambiando—, así que cada instancia restaurada
// arrancaría con metadatos de la época del import sobre un disco que ya divergió.
//
// El síntoma de no hacerlo aparece en la SEGUNDA generación de instancias y no
// señala jamás al commit.
func (m *Manager) releaseVolumes(mc *api.Machine) error {
	return m.guestVolumeOp(mc, "release", 10*time.Second)
}

// acquireVolumes le pide al invitado que MONTE sus volúmenes.
//
// Tras restaurar, cuando los discos ya apuntan a los ficheros de esta instancia.
// Un fallo aquí SÍ es un error: sin volumen, la herramienta escribe en un
// directorio del overlay que muere con la máquina, y eso no da ni un aviso hasta
// que alguien busca lo que guardó.
func (m *Manager) acquireVolumes(mc *api.Machine) error {
	if mc == nil || mc.IP == "" || len(mc.Volumes) == 0 {
		return nil
	}
	// Se espera a que conteste antes de pedírselo. Tras un Resume el invitado
	// está listo casi al instante, pero "casi" no es "siempre": sin esperar, la
	// primera petición se comería un connection refused y la máquina se marcaría
	// fallida por una carrera de milisegundos.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	base := "http://" + net.JoinHostPort(mc.IP, strconv.Itoa(api.GuestPort))
	if err := waitGuest(ctx, base, 20*time.Second); err != nil {
		return fmt.Errorf("el invitado no empezó a escuchar para montar sus volúmenes: %w", err)
	}
	return m.guestVolumeOp(mc, "acquire", 30*time.Second)
}

func (m *Manager) guestVolumeOp(mc *api.Machine, op string, limit time.Duration) error {
	if mc == nil || mc.IP == "" || len(mc.Volumes) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	url := "http://" + net.JoinHostPort(mc.IP, strconv.Itoa(api.GuestPort)) + "/volume/" + op
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("pidiendo al invitado que %s sus volúmenes: %w", op, err)
	}
	defer resp.Body.Close()
	// El código SÍ se mira, al contrario que en el vaciado: un puente antiguo no
	// conoce estas rutas y devuelve 404, y tomarlo por éxito es justo el fallo
	// silencioso que esto viene a cerrar.
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("el puente de esta imagen no sabe %s volúmenes: "+
				"es anterior a los snapshots sin montar. Ponlo al día con `kling images refresh`", op)
		}
		return fmt.Errorf("el invitado no pudo %s sus volúmenes: %s", op, strings.TrimSpace(string(b)))
	}
	return nil
}

func (m *Manager) flushVolume(mc *api.Machine) {
	if mc == nil || mc.IP == "" || mc.State != api.StateRunning {
		return
	}
	// Solo si hay algo que pueda haberse escrito. Un invitado con únicamente
	// volúmenes de solo lectura no tiene nada en caché que perder, y pedirle un
	// vaciado retrasaría su apagado a cambio de nada.
	escribible := false
	for _, v := range mc.Volumes {
		if !v.ReadOnly {
			escribible = true
			break
		}
	}
	if !escribible {
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
		log.Printf("%s: el invitado no vació sus volúmenes antes de morir: %v", mc.Name, err)
		return
	}
	resp.Body.Close()
}

// volumeDriveID nombra el disco i-ésimo de volumen.
//
// Los identificadores tienen que ser ESTABLES entre el arranque y la
// restauración: al restaurar un snapshot hay que reapuntar cada disco por su
// identificador, y uno que no coincida deja a la microVM leyendo el fichero de
// otra máquina, o ninguno.
func volumeDriveID(i int) string { return "volume" + strconv.Itoa(i) }

// legacyVolumeDriveID es como se llamaba el disco cuando solo podía haber uno.
//
// Los snapshots congelados con aquella versión lo llevan grabado dentro y no se
// pueden reescribir, así que restaurarlos exige usar este nombre. Es la única
// razón por la que existe.
const legacyVolumeDriveID = api.LegacyVolumeDriveID
