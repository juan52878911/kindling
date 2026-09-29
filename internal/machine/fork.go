package machine

// Fork: ramificar una máquina viva en N copias (`kling sandbox fork`).
//
// No hay mecanismo nuevo: son las piezas que ya existían, en orden. Commit
// congela la máquina en un snapshot dorado TEMPORAL —la pausa, vuelca memoria y
// overlay, y la reanuda: el original sigue corriendo— y Run restaura N
// instancias desde él, cada una con su overlay propio y su red propia. Lo que
// añade este fichero es el ciclo de vida de ese snapshot temporal.
//
// DECISIÓN — qué pasa con el snapshot temporal:
//
// Se CONSERVA mientras quede alguna copia no parada, y el vigilante lo borra
// solo en cuanto ya no queda ninguna (barrerForks). Borrarlo nada más restaurar
// parece más limpio, y en Linux hasta funcionaría (el mem.file sigue mapeado
// aunque se desenlace), pero rompe tres cosas que el resto del daemon da por
// hechas:
//
//   - RemoveSnapshot se niega a borrar un snapshot con instancias vivas. Es la
//     regla que protege el mem.file que todas mapean; saltársela aquí sería la
//     primera excepción, justo en el camino que más instancias crea.
//   - La contabilidad de memoria (hotMemFilesMiBLocked) mide la caché caliente
//     por el mem.file del snapshot de cada instancia. Desenlazado, mediría cero
//     y dejaría pasar arranques que no caben: el error sería del lado
//     peligroso.
//   - En macOS (vz) la restauración no mapea el fichero igual, y un fichero que
//     desaparece bajo una instancia viva no es algo que se haya probado allí.
//
// El precio es disco: el mem.file (perforado: solo lo que el invitado había
// tocado) y el overlay dorado siguen ocupando mientras viva alguna copia, y el
// snapshot aparece en `kling snapshots` con sus instancias. Es visible y se
// recupera solo.
//
// El snapshot se reconoce como temporal por una MARCA en su directorio
// (forkMarca), no por su nombre ni por sus etiquetas: un nombre lo puede elegir
// cualquiera con `kling commit`, y las etiquetas del snapshot se heredan a las
// copias y de ahí a lo que se haga con ellas —un `kling commit` de una copia
// daría un snapshot del usuario marcado como temporal, y el barrido se lo
// llevaría—. La marca es un fichero que Commit no copia.
//
// Lo que NO se puede evitar, y es por diseño de los snapshots: todas las copias
// despiertan con la MISMA memoria. Comparten la disposición de ASLR del kernel
// y de los procesos que ya corrían, y el estado de los generadores de números
// aleatorios que ya estaban sembrados. El reloj y el CSPRNG del invitado se
// resiembran al restaurar (resyncGuest, VMGENID), pero un proceso que ya tenía
// su semilla en memoria la conserva: es lo mismo que pasa con cualquier
// instancia de un snapshot dorado.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// ErrFork es una petición de fork que no se puede atender por cómo es la
// máquina de origen (o por la propia petición), no por un fallo del host.
var ErrFork = errors.New("cannot fork")

// forkMarca es el fichero que marca un snapshot como el temporal de un fork.
// Guarda el id de la máquina de origen, solo para diagnóstico.
const forkMarca = "fork-of"

// ForkOptions es lo que se puede decidir de las copias. El resto —imagen,
// memoria, CPU, red, volúmenes de solo lectura, etiquetas— sale del original,
// congelado en el snapshot.
type ForkOptions struct {
	// Count copias; 0 es 1. Como mucho api.ForkMax.
	Count int
	// TTLSeconds y OnTTL: 0 y "" heredan los del original.
	TTLSeconds int
	OnTTL      string
	// Lista, si no es nil, se llama con cada copia recién restaurada. Un error
	// deshace el fork entero. El daemon espera aquí al agente del invitado.
	Lista func(ctx context.Context, mc *api.Machine) error
	// Labels se suman a las de cada copia, ya en su nacimiento (sin ventana en
	// la que exista sin ellas). Las valida api.ValidateForkLabels.
	Labels map[string]string
}

// restaurarFork restaura una copia. Es m.Run —el tope de máquinas, la
// normalización de on_ttl y runFrom—; sustituible en tests, porque restaurar de
// verdad pide KVM y el binario de firecracker.
var restaurarFork = func(ctx context.Context, m *Manager, req api.RunRequest) (*api.Machine, error) {
	return m.Run(ctx, req)
}

// nombreSnapshotFork da un nombre único y válido (validName) al snapshot
// temporal: fork-<12 del id de origen>-<8 al azar>.
func nombreSnapshotFork(srcID string) string {
	if len(srcID) > 12 {
		srcID = srcID[:12]
	}
	return "fork-" + srcID + "-" + newID()[:8]
}

// nombreCopia es el nombre de una copia: el del original y un sufijo al azar.
// No se numeran (-1, -2...): dos forks del mismo original repetirían nombres, y
// Get resuelve por nombre.
func nombreCopia(src string) string {
	return src + "-" + newID()[:6]
}

// Fork ramifica la máquina ref en opt.Count copias y devuelve el snapshot
// temporal del que salieron y las copias, en marcha.
//
// Todo o nada: si una sola copia no se puede restaurar (o Lista la rechaza), se
// eliminan las ya creadas y el snapshot temporal, y el original sigue como
// estaba —Commit ya garantiza que lo reanuda pase lo que pase—.
func (m *Manager) Fork(ctx context.Context, ref string, opt ForkOptions) (snapName string, out []*api.Machine, errOut error) {
	n := opt.Count
	if n == 0 {
		n = 1
	}
	if n < 0 || n > api.ForkMax {
		return "", nil, fmt.Errorf("%w: count must be between 1 and %d", ErrFork, api.ForkMax)
	}
	if err := api.ValidateForkLabels(opt.Labels); err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrFork, err)
	}
	src, ok := m.Get(ref)
	if !ok {
		return "", nil, fmt.Errorf("%w: %q", ErrNoMachine, ref)
	}
	if err := puedeRamificarse(src); err != nil {
		return "", nil, err
	}
	if err := m.forkSinCredenciales(src); err != nil {
		return "", nil, err
	}

	name := nombreSnapshotFork(src.ID)
	// Reservado de principio a fin: entre que Commit termina y la primera
	// restauración reserva por su cuenta, el snapshot no tiene marca, ni
	// instancias, ni reserva de nadie. Sin esto, el barrido de forks o un
	// `kling snapshots rm` podían llevárselo en ese hueco.
	soltar := m.reserveDir(reservaSnapshot(name))
	defer soltar()
	// Con la reserva aún tomada (los defer van al revés): deshacerFork pasa por
	// removeSnapshot(propio=true), que descuenta justo esta reserva.
	defer func() {
		if errOut != nil {
			m.deshacerFork(name)
			snapName, out = "", nil
		}
	}()

	// La comprobación de credenciales se repite con el cerrojo de la máquina:
	// entre la de arriba y la pausa, un SetCredentials pudo darle alguna.
	if _, err := m.commit(ctx, src.ID, name, false, m.forkSinCredenciales); err != nil {
		return "", nil, fmt.Errorf("forking %s: %w", src.Name, err)
	}
	if err := os.WriteFile(filepath.Join(m.snapDir(name), forkMarca), []byte(src.ID+"\n"), 0o644); err != nil {
		return "", nil, fmt.Errorf("marking fork snapshot %s: %w", name, err)
	}

	ttl, onTTL := opt.TTLSeconds, opt.OnTTL
	if ttl == 0 {
		ttl = src.TTLSeconds
	}
	if onTTL == "" {
		onTTL = src.OnTTL
	}
	// Una a una y no en paralelo: la puerta de arranque (enterLaunch) las
	// serializaría igual, y así la reserva de memoria de cada una ve ya
	// anclado el mem.file de la anterior en vez de N reservas a ciegas.
	for i := 0; i < n; i++ {
		mc, err := restaurarFork(ctx, m, api.RunRequest{
			Name:       nombreCopia(src.Name),
			From:       name,
			AllowExec:  src.AllowExec,
			TTLSeconds: ttl,
			OnTTL:      onTTL,
			Labels:     api.MergeLabels(opt.Labels, map[string]string{api.LabelForkOf: src.ID}),
		})
		if err != nil {
			return "", nil, fmt.Errorf("fork %d of %d from %s: %w", i+1, n, src.Name, err)
		}
		out = append(out, mc)
		if opt.Lista != nil {
			if err := opt.Lista(ctx, mc); err != nil {
				return "", nil, fmt.Errorf("fork %d of %d from %s: %w", i+1, n, src.Name, err)
			}
		}
	}
	log.Printf("fork: %s branched into %d copies from snapshot %s", src.Name, n, name)
	return name, out, nil
}

// puedeRamificarse descarta, antes de pausar nada, las máquinas que no se
// pueden copiar.
func puedeRamificarse(src *api.Machine) error {
	if src.State != api.StateRunning {
		return fmt.Errorf("%w: only a running machine can be forked (%s is %s); thaw it first",
			ErrNotRunning, src.Name, src.State)
	}
	// Igual que Freeze, y por lo mismo: el secreto vive en la RAM, y el
	// volcado acabaría en un mem.file en disco que además comparten todas las
	// copias.
	if src.HasSecrets {
		return fmt.Errorf("%w: %s has session secrets injected via MMDS, and a fork would write them "+
			"to disk and hand them to every copy", ErrFork, src.Name)
	}
	// Commit también lo rechaza, pero con un consejo que aquí no vale.
	if len(src.Shares) > 0 {
		return fmt.Errorf("%w: %s has %d shared folder(s) mounted, and every copy would wake up "+
			"with them; fork a sandbox created without -share", ErrFork, src.Name, len(src.Shares))
	}
	// Un volumen en escritura no puede tenerlo más de una máquina a la vez:
	// la primera copia ya chocaría con el original. Mejor decirlo antes de
	// pausar y volcar para nada.
	for _, v := range src.Volumes {
		if !v.ReadOnly {
			return fmt.Errorf("%w: %s has volume %q mounted read-write, and only one machine at a time "+
				"can write to a volume; mount it read-only (:ro) to fork it", ErrFork, src.Name, v.Name)
		}
	}
	return nil
}

// forkSinCredenciales rechaza el fork de una máquina con credenciales del
// proxy (credenciales.go).
//
// DECISIÓN — rechazar y no reentregar: las copias despiertan con la memoria
// del original, y con ella los MARCADORES del original en el entorno de sus
// procesos; el proxy de cada copia es nuevo y no los conoce, así que fallan
// cerrado (bien) pero rotas y sin decir por qué. Reentregar con marcadores
// nuevos no lo arregla: van a MMDS, pero un proceso que ya leyó su entorno
// sigue con el viejo. Reentregar los MISMOS marcadores sí funcionaría, pero
// multiplicaría la clave por N máquinas sin que nadie lo haya pedido de
// forma explícita. Lo correcto ya existe: credenciales de plantilla y
// `run -from`, que da a cada instancia su marcador antes de que arranque nada.
//
// Vale igual para las que vinieron de una plantilla: el snapshot temporal del
// fork no lleva el almacén de la plantilla, así que las copias no recibirían
// nada. Se mira el almacén además de CredentialDomains: es la fuente de
// verdad, y state.json podría ir por detrás.
func (m *Manager) forkSinCredenciales(src *api.Machine) error {
	_, err := os.Stat(m.credPath(src.ID))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Falla cerrado: si no se puede mirar el almacén, no se sabe que no
		// haya credenciales.
		return fmt.Errorf("%w: couldn't check whether %s has proxy credentials: %v", ErrFork, src.Name, err)
	}
	if len(src.CredentialDomains) == 0 && err != nil {
		return nil
	}
	dominios := "proxy credentials"
	if len(src.CredentialDomains) > 0 {
		dominios = "proxy credentials for " + strings.Join(src.CredentialDomains, ", ")
	}
	return fmt.Errorf("%w: %s has %s, and every copy would wake up with placeholders its own proxy doesn't know; "+
		"attach the credentials to a template instead (kling template credential <template> ...) and "+
		"start another instance with run -from <template> instead of forking this one; every instance then gets its own placeholder", ErrFork, src.Name, dominios)
}

// deshacerFork elimina las copias que llegó a crear un fork fallido (también
// las que quedaron failed: runFrom deja la entrada para diagnosticarla, y
// mientras exista retiene el snapshot) y después el snapshot temporal. Se
// llama con la reserva del fork tomada.
func (m *Manager) deshacerFork(name string) {
	m.mu.RLock()
	var ids []string
	for id, mc := range m.byID {
		if mc.From == name {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	for _, id := range ids {
		if err := m.Remove(id); err != nil {
			log.Printf("fork %s: couldn't remove copy %s: %v", name, id, err)
		}
	}
	if _, err := os.Stat(m.snapDir(name)); err != nil {
		return // Commit no llegó a dejarlo (y ya limpió lo suyo)
	}
	if err := m.removeSnapshot(name, true); err != nil {
		log.Printf("fork %s: couldn't remove its snapshot: %v", name, err)
	}
}

// esSnapshotDeFork dice si dir es el snapshot temporal de un fork.
func esSnapshotDeFork(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, forkMarca))
	return err == nil
}

// forkEnUso dice si alguien retiene aún el snapshot de fork name: una reserva
// (el propio fork, o una restauración en curso) o una instancia que no esté
// parada. Es la misma regla que aplica removeSnapshot, que es quien decide de
// verdad; esto solo evita llamarlo (y registrar su negativa) en cada vuelta.
func (m *Manager) forkEnUso(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.reserved[reservaSnapshot(name)] > 0 {
		return true
	}
	for _, mc := range m.byID {
		if mc.From == name && mc.State != api.StateStopped {
			return true
		}
	}
	return false
}

// barrerForks borra los snapshots temporales de fork de los que ya no queda
// ninguna copia. Lo llama el vigilante en cada vuelta, sin m.mu tomado.
//
// Una copia dormida (warm) lo retiene igual que una viva: aunque se descongele
// desde su propio volcado, sigue figurando como instancia del snapshot, y
// removeSnapshot la cuenta.
func (m *Manager) barrerForks() {
	base := filepath.Join(m.root, "snapshots")
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || !esSnapshotDeFork(filepath.Join(base, name)) {
			continue
		}
		if m.forkEnUso(name) {
			continue
		}
		if err := m.removeSnapshot(name, false); err != nil {
			log.Printf("fork snapshot %q: no copies left, but couldn't remove it: %v", name, err)
			continue
		}
		log.Printf("fork snapshot %q: no copies left, removed", name)
	}
}
