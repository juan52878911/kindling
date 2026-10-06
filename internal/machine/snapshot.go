package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"log"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/digest"
	"github.com/juan52878911/kindling/pkg/esquema"
	"github.com/juan52878911/kindling/pkg/lazyre"

	"github.com/juan52878911/kindling/pkg/durable"
)

var validName = lazyre.New(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func (m *Manager) snapDir(name string) string {
	return filepath.Join(m.root, "snapshots", name)
}

// Commit congela una máquina en marcha como snapshot dorado reutilizable, y la
// deja corriendo.
//
// El snapshot guarda tres cosas: el estado de la VM, su memoria, y una copia del
// overlay tal y como estaba en ese instante. Las tres son necesarias: al
// restaurar, el invitado despierta con su estado de montaje en memoria, así que
// el disco que le demos debe tener exactamente el contenido que tenía al
// congelarse.
func (m *Manager) Commit(ctx context.Context, ref, name string, replace bool) (snapOut *api.Snapshot, errOut error) {
	return m.CommitWith(ctx, ref, name, CommitOptions{Replace: replace})
}

// CommitOptions es cómo se hace un Commit.
type CommitOptions struct {
	Replace bool
	// SkipReady no espera a que el invitado esté listo según su imagen
	// (listo.go); ReadyWait es cuánto se espera (0 = DefaultReadyWait).
	SkipReady bool
	ReadyWait time.Duration
}

// CommitWith es Commit con opciones. La espera a "listo" va ANTES del cerrojo
// de la máquina: puede durar minutos, y mientras nadie más podría pararla ni
// borrarla.
func (m *Manager) CommitWith(ctx context.Context, ref, name string, o CommitOptions) (*api.Snapshot, error) {
	if !o.SkipReady {
		if err := m.listoParaCongelar(ctx, ref, o.ReadyWait); err != nil {
			return nil, err
		}
	}
	return m.commit(ctx, ref, name, o.Replace, nil, false, false)
}

// commitPausada es Commit de una máquina que YA está pausada (Pause) y que se
// deja pausada: el snapshot de un grafo pausa todos sus nodos, vuelca uno a
// uno y los reanuda al final, para que todos los volcados sean del mismo
// instante (grafo_snapshot.go). Los volúmenes los suelta quien llama ANTES de
// pausarla (soltarlos pide hablar con el agente, y un invitado pausado no
// contesta), y es también quien se los devuelve: aquí no se tocan.
func (m *Manager) commitPausada(ctx context.Context, ref, name string) (*api.Snapshot, error) {
	return m.commit(ctx, ref, name, false, nil, true, false)
}

// commit es Commit con una comprobación opcional que se ejecuta con el cerrojo
// de la máquina tomado y la máquina releída, justo antes de pausarla. La usa
// Fork para repetir ahí lo que ya miró sin cerrojo (TOCTOU con un
// SetCredentials concurrente, que toma el mismo cerrojo). Con yaPausada, la
// máquina tiene que estar pausada, no se pausa ni se reanuda (commitPausada).
// Con deFork es el snapshot temporal de un fork: no se hashea su overlay (ver
// la DECISIÓN junto a los digests).
func (m *Manager) commit(ctx context.Context, ref, name string, replace bool, comprobar func(*api.Machine) error, yaPausada, deFork bool) (snapOut *api.Snapshot, errOut error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("invalid snapshot name: %q", name)
	}
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q does not exist", ref)
	}
	// El cerrojo de ciclo de vida de la plantilla, como Freeze, Stop o Remove
	// (M-04). Commit la pausa, le cambia el disco y la vuelca: sin él, un
	// Remove concurrente borraba su directorio bajo un VMM pausado, un Freeze
	// volcaba una máquina cuyo disco era el overlay dorado, y el reapuntado
	// final fallaba dejándola pausada y listada como running.
	defer m.lock(mc.ID)()

	// Se vuelve a leer con el cerrojo tomado: mientras lo esperábamos pudo
	// congelarla, pausarla o borrarla otro.
	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, fmt.Errorf("machine %q does not exist", ref)
	}
	mc = cur
	// Como Freeze, y con más razón: el volcado de Commit ES el mem.file del
	// dorado, el que mapean todas las instancias futuras. Un secreto de sesión
	// inyectado por MMDS acabaría en todas. Se mira aquí, con el cerrojo
	// tomado y la máquina releída: un PutMMDS concurrente toma el mismo
	// cerrojo, así que o ya marcó HasSecrets o espera a que acabe el commit.
	// Cubre también fork, que pasa por aquí.
	if mc.HasSecrets {
		return nil, fmt.Errorf("machine %s has session secrets injected via MMDS and can't become "+
			"a snapshot: its RAM would be shared by every instance. Snapshot a machine that never "+
			"received secrets", mc.Name)
	}
	switch {
	case yaPausada && mc.State != api.StatePaused:
		return nil, fmt.Errorf("machine %s should be paused for this snapshot (is %s)", mc.Name, mc.State)
	case !yaPausada && mc.State != api.StateRunning:
		return nil, fmt.Errorf("only a running machine can be committed (is %s)", mc.State)
	}
	if comprobar != nil {
		if err := comprobar(mc); err != nil {
			return nil, err
		}
	}
	// El dorado es un volcado de la RAM entera más una copia del overlay: si
	// no cabe, mejor saberlo antes de soltar volúmenes y pausar (ver
	// checkDiskParaVolcado).
	if err := m.checkDiskParaVolcado(max(mc.MemMiB, mc.MemMaxMiB)+int(allocatedBytes(filepath.Join(m.dir(mc.ID), "overlay.ext4"))>>20), "save"); err != nil {
		return nil, err
	}
	// La memoria volcada llevaría montada una carpeta de ESTE host (las vivas)
	// o un disco que no viaja con el snapshot (las copias): cada instancia
	// restaurada despertaría con un montaje que no le corresponde.
	if len(mc.Shares) > 0 {
		return nil, fmt.Errorf("%w: %s has %d shared folder(s) mounted, and a snapshot would carry them to "+
			"every instance restored from it. Commit a machine without -share (freeze/thaw of this one works)",
			ErrSharesCommit, mc.Name, len(mc.Shares))
	}

	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("no socket for %s", mc.ID)
	}

	// Reservado mientras dure el commit: sin meta.json todavía, este directorio
	// es indistinguible de los restos de un commit interrumpido, y el barrido
	// (sweepSnapshotLeftovers) o un RemoveSnapshot lo borrarían bajo nuestros pies.
	soltar := m.reserveDir(reservaSnapshot(name))
	defer soltar()

	dir := m.snapDir(name)
	var anterior string // el dorado viejo apartado por -replace
	defer func() {
		if anterior == "" {
			return
		}
		if errOut == nil {
			// El nuevo está entero (meta.json escrito): el viejo sobra.
			if err := os.RemoveAll(anterior); err != nil {
				log.Printf("commit %s: couldn't remove the replaced snapshot at %s: %v", name, anterior, err)
			}
			return
		}
		// La limpieza de abajo (que corre antes: los defer van al revés) ya
		// quitó lo a medias; el viejo vuelve a su sitio.
		if err := os.Rename(anterior, dir); err != nil {
			log.Printf("commit %s: couldn't restore the previous snapshot from %s: %v", name, anterior, err)
			return
		}
		m.invalidateSnapCache(name)
	}()
	if _, err := os.Stat(dir); err == nil {
		if !replace {
			return nil, fmt.Errorf("snapshot %q already exists (use `kling commit -replace` to replace it)", name)
		}
		// Reemplazar pasa por las comprobaciones de RemoveSnapshot y no por un
		// RemoveAll directo: son las que saben negarse si el snapshot tiene
		// instancias vivas, que seguirían mapeando un mem.file que estaríamos
		// pisando debajo de ellas.
		//
		// Pero el viejo NO se borra hasta que el nuevo esté entero: se aparta
		// al lado y vuelve a su sitio si el commit falla (o, si el daemon muere
		// a mitad, al arrancar: recuperarReemplazos). Antes se borraba primero,
		// y un volcado que fallaba por disco lleno dejaba sin ninguno de los dos.
		// El nuevo se escribe en el nombre definitivo, no en un temporal: el
		// volcado graba la ruta de su overlay, y restaurar la abre tal cual.
		var err error
		if anterior, err = m.apartarParaReemplazo(name); err != nil {
			return nil, fmt.Errorf("replacing snapshot %q: %w", name, err)
		}
	}

	snapPath := filepath.Join(dir, "snap.file")
	memPath := filepath.Join(dir, "mem.file")
	goldOverlay := filepath.Join(dir, "overlay.ext4")
	ownOverlay := filepath.Join(m.dir(mc.ID), "overlay.ext4")

	// Una plantilla jailed corre chrooteada: no ve snapDir. El overlay dorado y
	// el volcado se escriben en el jail (en su path absoluto) y se recuperan al
	// host después. goldDst es dónde se copia el overlay para que firecracker lo
	// abra; en el host es goldOverlay, en el jail su réplica dentro del chroot
	// (se fija al crear ese directorio, más abajo).
	jailed := m.jailerJailed && strings.HasPrefix(sock, m.jailRoot(mc.ID))
	goldDst := goldOverlay

	c := fc.New(sock)
	// El volcado escribe la memoria entera: con varios GiB no cabe en los 30 s
	// por defecto del cliente (F-01). Las llamadas de la limpieza usan el mismo
	// plazo porque pueden llegar con un volcado aún en marcha, y la API de
	// Firecracker las atiende de una en una: esperan a que acabe.
	lento := c.ConPlazo(plazoVolcado(max(mc.MemMiB, mc.MemMaxMiB)))

	// LIMPIEZA. Una sola, diferida, para TODAS las salidas de error (M-05).
	// Antes cada camino deshacía su parte a mano y varios se olvidaban de algo:
	// un Pause fallido no devolvía los volúmenes, y un fallo al reapuntar el
	// disco de vuelta o al perforar la memoria salía sin reanudar y sin borrar
	// el directorio a medias. La plantilla quedaba pausada figurando como
	// running —el vigilante no lo ve porque el proceso vive, y el gateway le
	// sigue enrutando peticiones que nadie contesta—.
	//
	// Los flags dicen qué hay que deshacer. Cada uno se marca ANTES de pedir lo
	// que deshace: una petición que falla, o cuyo ctx se cancela, pudo llegar a
	// aplicarse, y deshacer algo que no pasó es inocuo (reapuntar al disco
	// propio, montar un volumen ya montado). El camino feliz los baja al
	// devolver la plantilla a su estado (restaurarPlantilla), así que la
	// limpieza solo hace lo que quede pendiente.
	//
	// Con context.WithoutCancel: si quien pidió el commit se desconecta a
	// mitad, su ctx se cancela, y con él se saltaba la reanudación. Deshacer
	// no es opcional por mucho que ya nadie espere la respuesta.
	var (
		creado      bool // el directorio del snapshot es nuestro: borrarlo si no hay meta
		soltados    bool // se pidió al invitado soltar los volúmenes: devolvérselos
		pausaPedida bool // se pidió la pausa, aunque fallara: puede estar pausada
		pausada     bool // la pausa se confirmó: reanudar sí o sí
		reapuntado  bool // el overlay puede apuntar al dorado: devolverle el suyo
		hecho       bool // meta.json escrito: el snapshot vale y no se toca
	)
	limpio := context.WithoutCancel(ctx)

	// restaurarPlantilla devuelve la plantilla a como estaba: su disco, en
	// marcha, con sus volúmenes. La llaman el camino feliz, en cuanto termina
	// el volcado (para que la plantilla esté parada lo menos posible), y la
	// limpieza. Si devuelve error, la plantilla no era recuperable y ya quedó
	// marcada fallida.
	restaurarPlantilla := func() error {
		if reapuntado {
			if err := lento.PatchDrive(limpio, "overlay", ownOverlay); err != nil {
				// No se reanuda: correría escribiendo en el overlay dorado, que
				// es de otras instancias, y su propio disco se quedaría atrás
				// para siempre. Mejor muerta y dicho que viva y corrupta.
				err = fmt.Errorf("returning overlay to machine: %w", err)
				m.fail(mc, fmt.Errorf("commit could not give the template its own disk back: %w", err))
				reapuntado, pausaPedida, pausada, soltados = false, false, false, false
				return err
			}
			reapuntado = false
		}
		if pausaPedida {
			if err := lento.Resume(limpio); err != nil {
				if pausada {
					// Pausada y sin poder reanudarla: dejarla como running
					// sería mentir. Igual que Freeze.
					err = fmt.Errorf("resuming template after commit: %w", err)
					m.fail(mc, fmt.Errorf("commit paused the template and could not resume it: %w", err))
					pausaPedida, pausada, soltados = false, false, false
					return err
				}
				// La pausa nunca se confirmó: lo normal es que siga en marcha
				// y que Firecracker no tenga nada que reanudar.
				log.Printf("commit %s: resume after a failed pause: %v", mc.Name, err)
			}
			pausaPedida, pausada = false, false
		}
		if soltados {
			soltados = false
			// La plantilla sigue viva y se quedó sin volúmenes al soltarlos: hay
			// que devolvérselos, o seguirá corriendo escribiendo en su overlay.
			if err := m.acquireVolumes(mc); err != nil {
				log.Printf("warning: template %s ended up without its volumes after commit: %v", mc.Name, err)
			}
		}
		return nil
	}
	defer func() {
		if errOut == nil {
			return
		}
		_ = restaurarPlantilla()
		if creado && !hecho {
			os.RemoveAll(dir)
			m.invalidateSnapCache(name)
			if jailed {
				// Las réplicas en el chroot de la plantilla: el volcado y el
				// overlay dorado que no llegaron a recuperarse. Sin seguir
				// enlaces: el chroot es del VMM (ver enjaulado.go).
				if err := borrarEnJail(m.jailRoot(mc.ID), dir); err != nil {
					log.Printf("commit %s: cleaning up the jail: %v", mc.Name, err)
				}
			}
		}
	}()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	creado = true

	// Quien escribe el snapshot es Firecracker, que corre sin privilegios: el
	// directorio tiene que ser suyo antes de pedírselo.
	if err := m.priv.Own(dir); err != nil {
		return nil, err
	}
	if jailed {
		// La réplica del directorio dentro del chroot se crea y se cede por
		// descriptor, sin seguir enlaces: la raíz del chroot es del VMM, que
		// pudo cambiar cualquier componente de esa ruta por un enlace al host,
		// y un MkdirAll y un Chown por ruta, como root, lo seguirían. El overlay
		// dorado se crea después dentro de ESTE directorio (rutaEnDir), no en
		// lo que la ruta resuelva entonces.
		dj, err := abrirDirSinEnlaces(m.jailRoot(mc.ID), dir, true)
		if err != nil {
			return nil, err
		}
		defer dj.Close()
		if m.priv.Enabled {
			if err := dj.Chown(m.priv.UID, m.priv.GID); err != nil {
				return nil, fmt.Errorf("granting the snapshot directory in the jail: %w", err)
			}
		}
		goldDst = rutaEnDir(dj, "overlay.ext4")
	}

	// Los volúmenes se DESMONTAN antes de congelar, y con la máquina aún
	// corriendo: un invitado pausado no atiende HTTP.
	//
	// Sin esto, la memoria volcada lleva dentro la caché de ext4 —superbloque,
	// mapas de bloques, posición del journal— de un disco que después seguirá
	// cambiando, porque el fichero del volumen NO se copia al snapshot. Cada
	// instancia restaurada arrancaría creyendo un estado que ya no existe.
	if !yaPausada {
		soltados = true
		if err := m.releaseVolumes(mc); err != nil {
			return nil, fmt.Errorf("preparing volumes for freeze: %w", err)
		}

		// Con la plantilla aún en marcha: que suelte lo que no usa, y el
		// dorado lleve solo lo que está en uso (apreton_volcado.go). Es el
		// mem.file que mapearán todas las copias.
		if n := apretarAntesDeVolcar(ctx, c, mc); n > 0 {
			log.Printf("commit %s: the guest handed back ~%d MiB before the dump", mc.Name, n)
		}

		pausaPedida = true
		if err := c.Pause(ctx); err != nil {
			return nil, err
		}
		pausada = true
	}
	t := nuevosTiempos()

	// El overlay se copia con la máquina pausada, para que sea coherente con la
	// memoria que se va a volcar.
	srcOverlay := m.overlayParaLeer(mc.ID)
	// El overlay lo escribe el VMM: se abre sin seguir enlaces, se comprueba
	// sobre el descriptor que es un fichero regular y se copia DESDE ese
	// descriptor, sin volver a abrir la ruta (ver fijarOverlayParaLeer). El
	// destino se crea y se cede también por descriptor: en el jail está en un
	// directorio del VMM.
	origen, tras, err := fijarOverlayParaLeer(srcOverlay)
	if err != nil {
		return nil, fmt.Errorf("copying overlay: %w", err)
	}
	goldFI, err := m.copiarOverlayDesde(ctx, origen, goldDst, m.priv.OwnFile)
	t.marca("overlay")
	origen.Close()
	if err != nil {
		return nil, fmt.Errorf("copying overlay: %w", err)
	}
	if err := tras(); err != nil {
		_ = os.Remove(goldDst)
		return nil, fmt.Errorf("copying overlay: %w", err)
	}

	// CLAVE: se reapunta el disco a la copia dorada ANTES de volcar, para que el
	// snapshot grabe esa ruta y no la de esta máquina.
	//
	// Sin esto, el snapshot queda atado al directorio de la plantilla: en cuanto
	// se elimina la plantilla, restaurar falla con "No such file or directory"
	// sobre un overlay que ya no existe. El snapshot dorado tiene que ser
	// autocontenido, porque su razón de ser es sobrevivir a la máquina que lo creó.
	reapuntado = true
	if err := c.PatchDrive(ctx, "overlay", goldOverlay); err != nil {
		return nil, fmt.Errorf("repointing overlay to golden copy: %w", err)
	}
	// Un volcado completo deja a cero el mapa de páginas sucias del VMM: si la
	// plantilla era una copia con seguimiento, su siguiente freeze ya no puede
	// ser "desde el dorado" (diff_volcado.go). Se olvida ANTES de pedirlo, y
	// no solo si el commit sale bien: falle lo que falle a partir de aquí, la
	// plantilla se reanuda con el mapa ya reiniciado.
	m.olvidarDiffBase(mc.ID)
	if err := lento.Snapshot(ctx, snapPath, memPath); err != nil {
		return nil, err
	}
	t.marca("dump")
	if jailed {
		// Recuperar del chroot al host: snapDir es donde runFrom los busca (y
		// los replica de vuelta en el próximo jail). Rename, mismo filesystem,
		// sin seguir enlaces y solo si es el fichero que escribió el VMM (ver
		// recuperarDelJail).
		if err := recuperarDelJail(m.jailRoot(mc.ID), dir, dir, m.uidJail(), "snap.file", "mem.file", "overlay.ext4"); err != nil {
			return nil, err
		}
		// El overlay dorado estuvo en un directorio del VMM: lo que se recupera
		// tiene que ser el fichero que copió el daemon, no un enlace ni otro
		// fichero puesto en su lugar (de él se clonarán todas las instancias).
		if fi, err := os.Lstat(goldOverlay); err != nil || !fi.Mode().IsRegular() || !os.SameFile(fi, goldFI) {
			return nil, fmt.Errorf("recovering overlay.ext4 from jail: it is not the golden copy the daemon wrote")
		}
	}
	// Se devuelve el disco propio y se reanuda YA: la plantilla no debe escribir
	// en el overlay dorado, que a partir de ahora es plantilla de otras
	// instancias, y lo que queda (perforar, hashear) no la necesita parada.
	// Vuelve a montar sus volúmenes: sigue viva hasta que la importación la
	// destruya, y con -keep puede quedarse.
	if err := restaurarPlantilla(); err != nil {
		return nil, err
	}
	t.marca("resume")

	if out, err := perforarHuecos(ctx, memPath); err != nil {
		return nil, fmt.Errorf("punching holes in memory file: %v: %s", err, out)
	}
	t.marca("holes")

	// Digests de integridad del rootfs dorado y del volcado de estado.
	//
	// Se calculan aquí, con los ficheros ya en su forma final (el overlay copiado,
	// el snap volcado, la memoria perforada). El mem.file se deja fuera a
	// propósito: es el grande y volver a leerlo entero en cada restauración mataría
	// los ~30 ms del thaw. Ver verifyIntegrity para el porqué completo.
	//
	// DECISIÓN — el snapshot temporal de un fork no graba el digest de su
	// overlay. Son 512 MiB nominales que sha256 lee enteros (los huecos pasan
	// como ceros): medido en fc-test, 1,5 s de un fork de 5 s, con la
	// plantilla ya reanudada pero con quien pidió el fork esperando. Ese digest
	// detecta que un dorado se corrompió en disco entre que se congeló y una
	// restauración futura; el temporal de un fork se restaura en esta misma
	// llamada, segundos después de escribirlo este daemon, y se borra cuando no
	// quedan copias (barrerForks). Sin digest se trata como un dorado antiguo
	// para ese fichero (verifyIntegrity lo salta); el de snap.file, que es
	// pequeño, se sigue grabando y comprobando.
	var rootfsSHA string
	if !deFork {
		if rootfsSHA, err = digest.File(goldOverlay); err != nil {
			return nil, fmt.Errorf("computing digest of golden overlay: %w", err)
		}
	}
	snapSHA, err := digest.File(snapPath)
	if err != nil {
		return nil, fmt.Errorf("computing digest of state dump: %w", err)
	}
	// El kernel con el que se congela (K2): sin él, reconstruirlo con otra
	// configuración (K1) rompería restauraciones de este dorado sin ninguna
	// señal de por qué. Cacheado por tamaño+fecha (kernelHash): no es otro
	// fichero grande que hashear entero en cada commit.
	kernelSHA, err := m.kernelHash()
	if err != nil {
		return nil, fmt.Errorf("computing digest of the kernel: %w", err)
	}
	t.marca("digest")
	// F2: si la plantilla arrancó en frío (mc.From == ""), este binario ya le
	// puso `ipv6.disable=1` en la línea de arranque (knet.BootArg), así que su
	// kernel congelado no lo tiene cargado. Si en cambio es una instancia de
	// otro dorado (fork), la línea de arranque no se repite: la que tiene es
	// la que quedó grabada en la memoria de AQUEL, así que se hereda su marca
	// y no se vuelve a suponer "sí" a ciegas.
	// Una imagen que pide la pila IPv6 (ipv6DeReceta) arrancó con el módulo
	// cargado: su dorado no lleva la marca.
	guestIPv6Stack := mc.From == "" && m.ipv6DeReceta(mc.Image)
	guestIPv6Off := mc.From == "" && !guestIPv6Stack
	if mc.From != "" {
		if origen, _, oerr := m.loadSnapshotCached(mc.From); oerr == nil {
			guestIPv6Off = origen.GuestIPv6Off
			guestIPv6Stack = origen.GuestIPv6Stack
		}
		// Si no se puede leer el dorado de origen (se borró entre tanto), se
		// deja en false: "no consta" es la lectura segura, igual que un
		// dorado antiguo sin el campo.
	}

	snap := &api.Snapshot{
		Name: name, Image: mc.Image, CreatedAt: time.Now(),
		// Sin las etiquetas de grafo: una plantilla no es de ningún grafo, y
		// una máquina nacida de ella tampoco (ver sinEtiquetasGrafo).
		VCPUs: mc.VCPUs, MemMiB: mc.MemMiB, MemMaxMiB: mc.MemMaxMiB, Labels: sinEtiquetasGrafo(mc.Labels),
		Egress:       mc.Egress,
		CPUPct:       mc.CPUPct,
		CPUPctFixed:  mc.CPUPctFixed,
		AllowDomains: mc.AllowDomains,
		// La puerta de exec se congela con la memoria: las instancias la tendrán
		// quiera quien las cree o no, y el snapshot tiene que decirlo.
		AllowExec:      mc.AllowExec,
		RootfsSHA256:   rootfsSHA,
		SnapSHA256:     snapSHA,
		KernelSHA256:   kernelSHA,
		GuestIPv6Off:   guestIPv6Off,
		GuestIPv6Stack: guestIPv6Stack,
		// El volumen se graba en el snapshot porque el conjunto de discos de una
		// microVM queda FIJADO al congelarla: a una restaurada no se le puede
		// añadir un disco que no tuviera. Sin esto, el gateway despierta el
		// servicio sin volumen y la herramienta escribe en un overlay que muere
		// con la máquina — sin un solo error por ningún lado.
		Volumes:   mc.Volumes,
		MemBytes:  allocatedBytes(memPath),
		DiskBytes: diskUsage(dir),
	}
	// Con qué VMM, kling y macOS se hizo: lo que permite marcarlo obsoleto
	// cuando cambien en vez de fallar al despertar (meta.go).
	m.grabarOrigen(snap)
	if err := m.firmar(snap); err != nil {
		return nil, err
	}
	cerrarVolcado(dir)
	m.priv.EnsureReadable(dir)
	m.overlayDoradoSinJailer(dir)

	b, err := codificarMeta(snap, nil)
	if err != nil {
		return nil, err
	}
	if err := writeMeta(dir, b); err != nil {
		return nil, err
	}
	// El directorio (mem.file, overlay.ext4...) ya estaba en su forma final
	// antes de este writeMeta; lo que hace falta olvidar es lo que -replace
	// pudo dejar cacheado con el mismo nombre (M-08, M-12).
	m.invalidateSnapCache(name)
	hecho = true
	// Los digests se acaban de calcular sobre estos mismos ficheros: la
	// primera restauración no tiene que volver a leerlos (1,4 s medidos en la
	// de un fork). Tras reiniciar el daemon se verifican otra vez, como
	// cualquier dorado (integridadYaVista).
	m.anotarIntegridad(name, dir)
	t.marca("meta")
	log.Printf("commit %s -> %s: %s", mc.Name, name, t)

	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvCommitted, ID: mc.ID, Name: name,
		Message: fmt.Sprintf("golden snapshot from %s (%d MiB)", mc.Name, snap.MemBytes>>20)})
	return snap, nil
}

// Snapshots lista los snapshots dorados y cuántas instancias vivas tiene cada uno.
func (m *Manager) Snapshots() []*api.Snapshot {
	entries, err := os.ReadDir(filepath.Join(m.root, "snapshots"))
	if err != nil {
		return nil
	}

	live := map[string]int{}
	m.mu.RLock()
	for _, mc := range m.byID {
		if mc.From != "" && mc.State == api.StateRunning {
			live[mc.From]++
		}
	}
	m.mu.RUnlock()

	out := make([]*api.Snapshot, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, disco, err := m.loadSnapshotCached(e.Name())
		if err != nil {
			continue
		}
		s.DiskBytes = disco
		s.Instances = live[e.Name()]
		m.anotarCredencialesPlantilla(s)
		m.marcarObsoleto(s)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// snapCacheEntry es lo que loadSnapshotCached recuerda de un dorado: el
// snapshot ya parseado y su ocupación en disco, con la huella (mtime de
// meta.json y del directorio) con la que se calcularon. snap NUNCA se entrega
// a quien llama: se clona (ver cloneSnapshot), para que nadie pueda pisar lo
// cacheado escribiendo en DiskBytes/Instances o anotando el que recibió.
type snapCacheEntry struct {
	metaMod int64
	dirMod  int64
	snap    *api.Snapshot
	disco   int64
}

// loadSnapshotCached es loadSnapshot + diskUsage con caché por mtime de
// meta.json y del directorio (M-08).
//
// Antes, Snapshots() releía y parseaba el meta.json y recorría el directorio
// entero de CADA dorado en CADA llamada: el gateway llama a Snapshots() en
// cada tick del reaper y en cada arranque en frío desde un snapshot (runFrom),
// así que con unas pocas decenas de servicios eso es E/S de fondo constante.
// Un dorado no cambia salvo por una anotación o un `commit -replace` con el
// mismo nombre — los dos pasan por writeMeta/removeSnapshot, que invalidan
// esta entrada (ver invalidateSnapCache) — así que memorizar el resultado
// hasta que eso ocurra no pierde ninguna actualización real.
//
// La huella (mtime de meta.json + mtime del directorio) es un cinturón además
// del tirante de la invalidación explícita: cubre cambios que no pasan por
// esas dos funciones, como restaurar un directorio desde la papelera a mano.
func (m *Manager) loadSnapshotCached(name string) (*api.Snapshot, int64, error) {
	dir := m.snapDir(name)
	metaFi, err := os.Stat(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, 0, fmt.Errorf("snapshot %q does not exist", name)
	}
	dirFi, err := os.Stat(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("snapshot %q does not exist", name)
	}
	metaMod, dirMod := metaFi.ModTime().UnixNano(), dirFi.ModTime().UnixNano()

	m.mu.RLock()
	entry, hay := m.snapCache[name]
	m.mu.RUnlock()
	if hay && entry.metaMod == metaMod && entry.dirMod == dirMod {
		return cloneSnapshot(entry.snap), entry.disco, nil
	}

	snap, err := m.loadSnapshot(name)
	if err != nil {
		return nil, 0, err
	}
	disco := diskUsage(dir)

	m.mu.Lock()
	if m.snapCache == nil {
		m.snapCache = map[string]snapCacheEntry{}
	}
	m.snapCache[name] = snapCacheEntry{metaMod: metaMod, dirMod: dirMod, snap: snap, disco: disco}
	m.mu.Unlock()
	return cloneSnapshot(snap), disco, nil
}

// invalidateSnapCache olvida todo lo cacheado sobre un dorado: el snapshot
// parseado y su disco (loadSnapshotCached), y el tamaño asignado de su
// mem.file (hotMemFilesMiBLocked, M-12). Se llama justo después de escribir su
// meta.json (writeMeta, en Commit y en editMeta) o de apartar su directorio
// (removeSnapshot): la próxima lectura vuelve a mirar el disco en vez de
// servir un dorado que ya no es el que hay ahí.
func (m *Manager) invalidateSnapCache(name string) {
	m.mu.Lock()
	delete(m.snapCache, name)
	delete(m.memAllocCache, name)
	m.mu.Unlock()
}

// cloneSnapshot copia un *api.Snapshot para que quien lo recibe pueda pisar
// DiskBytes o Instances —o anotarlo— sin tocar al que vive en snapCache ni al
// que puede estar leyendo otro goroutine a la vez (mismo motivo que M-03 con
// api.Machine: compartir el slice/map de otro y escribir en el sitio es una
// carrera de datos, no solo un bug de lógica).
func cloneSnapshot(s *api.Snapshot) *api.Snapshot {
	c := *s
	c.Volumes = append([]api.VolumeAttachment(nil), s.Volumes...)
	c.AllowDomains = append([]string(nil), s.AllowDomains...)
	if s.Labels != nil {
		c.Labels = make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			c.Labels[k] = v
		}
	}
	if s.Annotations != nil {
		c.Annotations = make(map[string]json.RawMessage, len(s.Annotations))
		for k, v := range s.Annotations {
			c.Annotations[k] = v
		}
	}
	return &c
}

func (m *Manager) loadSnapshot(name string) (*api.Snapshot, error) {
	s, _, _, err := m.loadSnapshotMeta(name)
	return s, err
}

// loadSnapshotMeta es loadSnapshot devolviendo también el meta.json tal cual
// estaba y su versión: lo que editMeta necesita para migrarlo con copia y
// conservar lo que no conoce (ver meta.go).
func (m *Manager) loadSnapshotMeta(name string) (*api.Snapshot, []byte, int, error) {
	// El nombre llega de la URL. Sin validarlo, un "../../etc" saldría del
	// directorio de datos: recorrido de rutas de manual.
	if !validName.MatchString(name) {
		return nil, nil, 0, fmt.Errorf("invalid snapshot name: %q", name)
	}
	ruta := filepath.Join(m.snapDir(name), "meta.json")
	b, err := os.ReadFile(ruta)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("snapshot %q does not exist", name)
	}
	// Uno de un kling más nuevo (schema mayor) es un error y no se toca: ni
	// se restaura ni se anota ni se aparta (esquema.ErrMasNuevo).
	s, v, err := decodificarMeta(ruta, b)
	if err != nil {
		return nil, nil, v, err
	}
	// El DIRECTORIO manda sobre lo que diga el meta.
	//
	// `runFrom` resuelve el snapshot por `snapDir(nombre)`, o sea por directorio.
	// Si el meta declara otro nombre —un meta copiado a mano, un renombrado a
	// medias— el listado enseñaría un servicio que no se puede instanciar, y dos
	// directorios que declaren el mismo nombre saldrían como DUPLICADOS
	// indistinguibles. Observado: `sequentialthinking` apareció dos veces, con
	// tamaños distintos y un solo directorio en disco.
	if s.Name != name {
		log.Printf("snapshot %q: its meta claims the name %q; going with the directory",
			name, s.Name)
		s.Name = name
	}
	liftV04(b, s)
	return s, b, v, nil
}

// verifyIntegrity comprueba que el rootfs dorado y el volcado de estado no se
// corrompieron desde que se congelaron.
//
// DECISIÓN — qué se hashea y qué no:
//
//	overlay.ext4 (rootfs) y snap.file  -> SÍ, la PRIMERA vez.
//	mem.file                           -> NO aquí.
//
// El fichero de memoria es el grande —cientos de MiB— y restaurar promete ~30 ms;
// leerlo entero por sha256 en cada thaw tiraría esa cifra por tierra.
//
// CORRECCIÓN, medida: la versión anterior de esta nota daba por hecho que "el
// rootfs y el snap.file son pequeños". No lo son. El overlay dorado son 512 MiB
// nominales, y sha256 los lee ENTEROS —los huecos de un fichero disperso se leen
// como ceros, y por el hash pasan igual—. Medido en fc-test: hashearlo cuesta
// 2.866 ms de los 4.280 que tarda una instanciación, el 67%.
//
// El razonamiento era bueno; el supuesto estaba mal por dos órdenes de magnitud.
//
// Lo que arregla el desfase no es hashear menos sino hashear MENOS VECES: un
// dorado es INMUTABLE desde que se congela, así que verificarlo en cada una de
// las 142 instanciaciones no aporta nada sobre verificarlo en la primera. Se
// recuerda el veredicto y se reusa mientras el fichero no cambie de tamaño ni de
// fecha; si cambia, se vuelve a verificar. Un dorado corrupto se sigue detectando,
// y se detecta igual de pronto.
//
// Los snapshots anteriores a esta comprobación no tienen digests grabados: se
// saltan en vez de fallar, o reimportar dejaría de ser opcional para todos.
func (m *Manager) verifyIntegrity(snap *api.Snapshot, snapDir string) error {
	if snap.RootfsSHA256 == "" && snap.SnapSHA256 == "" {
		return nil // snapshot legacy: no hay digests que comprobar
	}
	if m.integridadYaVista(snap.Name, snapDir) {
		return nil
	}
	for _, chk := range []struct{ file, want string }{
		{"overlay.ext4", snap.RootfsSHA256},
		{"snap.file", snap.SnapSHA256},
	} {
		if chk.want == "" {
			continue
		}
		got, err := digest.File(filepath.Join(snapDir, chk.file))
		if err != nil {
			return fmt.Errorf("couldn't read %s to verify its integrity: %w", chk.file, err)
		}
		if got != chk.want {
			return fmt.Errorf("snapshot %q is corrupt: %s doesn't match what was frozen "+
				"(sha256 expected %s…, found %s…). Reimport it with `kling mcp import %s -force`",
				snap.Name, chk.file, chk.want[:12], got[:12], snap.Name)
		}
	}
	m.anotarIntegridad(snap.Name, snapDir)
	return nil
}

// huellaSnapshot describe los ficheros verificados sin leerlos: tamaño y fecha de
// los dos que se hashean. Basta para saber si el dorado sigue siendo el mismo, y
// cuesta dos stat en vez de 512 MiB de sha256.
type huellaSnapshot struct {
	tam   [2]int64
	fecha [2]int64
}

func huellaDe(snapDir string) (huellaSnapshot, bool) {
	var h huellaSnapshot
	for i, f := range [2]string{"overlay.ext4", "snap.file"} {
		st, err := os.Stat(filepath.Join(snapDir, f))
		if err != nil {
			return h, false
		}
		h.tam[i] = st.Size()
		h.fecha[i] = st.ModTime().UnixNano()
	}
	return h, true
}

// integridadYaVista dice si este dorado, EXACTAMENTE como está ahora en disco, ya
// pasó la verificación. El veredicto vive en memoria y no en disco a propósito:
// tras reiniciar el daemon se vuelve a verificar una vez, que es barato y cubre
// una corrupción ocurrida mientras estaba parado.
func (m *Manager) integridadYaVista(name, snapDir string) bool {
	h, ok := huellaDe(snapDir)
	if !ok {
		return false
	}
	m.mu.RLock()
	visto, hay := m.integridad[name]
	m.mu.RUnlock()
	return hay && visto == h
}

func (m *Manager) anotarIntegridad(name, snapDir string) {
	h, ok := huellaDe(snapDir)
	if !ok {
		return
	}
	m.mu.Lock()
	if m.integridad == nil {
		m.integridad = map[string]huellaSnapshot{}
	}
	m.integridad[name] = h
	m.mu.Unlock()
}

// huellaKernel es el kernelSHA256 cacheado de Manager.kernelSHA, junto con el
// tamaño y la fecha del vmlinux que lo produjeron (K2). Igual que huellaSnapshot:
// basta un stat para saber si sigue valiendo, en vez de volver a hashear.
type huellaKernel struct {
	tam   int64
	fecha int64
	hash  string
}

// kernelHash devuelve el sha256 del kernel en uso (KernelPath), cacheado por
// tamaño+fecha del fichero (K2).
//
// Se pide en cada Commit y en cada runFrom, y rehashear un vmlinux de varias
// decenas de MiB en cada restauración sería justo el tipo de coste que este
// proyecto existe para evitar. El vmlinux es un fichero compartido por todas
// las microVMs y solo cambia cuando alguien reconstruye el kernel (K1) o lo
// reemplaza a mano; entre esos dos momentos, tamaño y fecha no se mueven.
func (m *Manager) kernelHash() (string, error) {
	path := m.KernelPath()
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	tam, fecha := st.Size(), st.ModTime().UnixNano()

	m.mu.RLock()
	h := m.kernelSHA
	m.mu.RUnlock()
	if h.hash != "" && h.tam == tam && h.fecha == fecha {
		return h.hash, nil
	}

	hash, err := digest.File(path)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.kernelSHA = huellaKernel{tam: tam, fecha: fecha, hash: hash}
	m.mu.Unlock()
	return hash, nil
}

// overlayDoradoSinJailer deja escribible por el grupo del VMM el overlay de un
// dorado, SOLO sin jailer (KLING_JAILER=0): ahí Firecracker abre la ruta del
// host tal cual al cargar el snapshot, en lectura y escritura, y no hay jail
// donde darle su propia copia bajo esa ruta. Con jailer (el modo por defecto)
// el dorado queda de solo lectura: ver runFrom.
func (m *Manager) overlayDoradoSinJailer(snapDir string) {
	if !m.priv.Enabled || m.jailerJailed {
		return
	}
	_ = os.Chmod(filepath.Join(snapDir, "overlay.ext4"), 0o660)
}

// avisoKernel dice, para el log, si el kernel instalado ahora no es el mismo
// con el que se congeló `que` (K2), o "" si lo es o no se sabe.
//
// Es un aviso y no un error: ni restaurar un dorado ni descongelar usan
// vmlinux. El kernel del invitado viaja dentro de mem.file con el resto de su
// memoria, y Firecracker no vuelve a leer el fichero (runFrom y Thaw ni
// siquiera lo enlazan en el jail). Negarse dejaba sin servicio todos los
// dorados y las congeladas tras reconstruir el kernel (K1), y funcionarían
// perfectamente. Lo que sí usaría el kernel nuevo es el siguiente ARRANQUE EN
// FRÍO de esa plantilla o máquina: para eso queda el aviso.
//
// recordedSHA vacío es anterior a este campo: nada que comparar.
func (m *Manager) avisoKernel(recordedSHA, que string) string {
	err := m.kernelIgual(recordedSHA)
	if !errors.Is(err, errKernelCambiado) {
		return ""
	}
	return fmt.Sprintf("%s was frozen with a different kernel than the one installed now; "+
		"restoring doesn't use it (the guest kernel lives in its memory), but a cold boot would", que)
}

// errKernelCambiado es que el kernel instalado no es el grabado al congelar.
var errKernelCambiado = errors.New("the kernel changed")

// avisoIPv6Invitado dice, para el log y el bus de eventos, si las instancias
// del dorado `name` arrancan con el módulo IPv6 del kernel todavía cargado
// (F2): "" si el dorado ya lleva GuestIPv6Off, o si a este dorado ya se le
// avisó antes en la vida de este daemon (sync.Map, igual que resyncAvisado:
// un aviso por dorado, no uno por instancia).
//
// Es un aviso y no un bloqueo: `applyIPv6Barrier` cierra el paso en el
// namespace del host lo mismo con o sin este campo (defensa en profundidad,
// no la única capa) — ver SECURITY.md, "IPv6: cerrado, no solo ausente". Lo
// que falta en estos dorados es la capa de dentro: el invitado, si el módulo
// sigue cargado, aún podría auto-asignarse una link-local en su propia pila.
func (m *Manager) avisoIPv6Invitado(name string, guestIPv6Off bool) string {
	if guestIPv6Off {
		return ""
	}
	if _, yaAvisado := m.ipv6Avisado.LoadOrStore(name, true); yaAvisado {
		return ""
	}
	return fmt.Sprintf("snapshot %q was frozen before the IPv6 barrier: its guest kernel still has "+
		"the IPv6 module loaded (the host namespace blocks it either way, see SECURITY.md). "+
		"To fix this instance's lineage, commit a fresh golden from a machine booted with the "+
		"current kindling (`kling commit -replace <machine> %s`)", name, name)
}

// kernelIgual compara recordedSHA con el kernel instalado ahora; vacío se
// acepta (anterior al campo). Es la parte común de avisoKernel (dorados)
// y del Thaw de una warm (sello del volcado, ver kernelDelVolcado).
func (m *Manager) kernelIgual(recordedSHA string) error {
	if recordedSHA == "" {
		return nil
	}
	actual, err := m.kernelHash()
	if err != nil {
		return fmt.Errorf("hashing the current kernel: %w", err)
	}
	if actual != recordedSHA {
		return errKernelCambiado
	}
	return nil
}

// RemoveSnapshot borra un snapshot dorado, salvo que tenga instancias vivas.
func (m *Manager) RemoveSnapshot(name string) error { return m.removeSnapshot(name, false) }

// removeSnapshot es RemoveSnapshot; propio=true lo llama el commit que TIENE la
// reserva de ese nombre (el camino de -replace), que no debe toparse con ella.
//
// Se niega mientras alguien más tenga reservado el snapshot: un commit que lo
// está escribiendo o una restauración (runFrom) que lo está leyendo. La
// restauración reserva ANTES de leer nada y no aparece en byID hasta mucho
// después —copiar el overlay dorado, montar la red—, así que mirar solo las
// instancias vivas dejaba borrar el mem.file bajo un LoadSnapshot en curso, que
// fallaba con un ENOENT crudo (M-15).
//
// La comprobación y la retirada del directorio van bajo el MISMO m.mu, y la
// retirada es un rename a la papelera (instantáneo, como en el barrido): entre
// "nadie lo usa" y "ya no está" no cabe una reserva nueva. El borrado de verdad,
// que con un mem.file de GiB tarda, va después y sin cerrojo.
func (m *Manager) removeSnapshot(name string, propio bool) error {
	destino, restos, rerr, err := m.retirarSnapshot(name, propio, m.apartarSnapshot)
	if err != nil {
		return err
	}
	if restos {
		log.Printf("snapshot %q: removing the leftovers of an interrupted commit", name)
	}
	if rerr != nil {
		// Sin papelera (otro sistema de ficheros, permisos): se borra en su
		// sitio, como siempre. La ventana vuelve a existir, pero solo aquí.
		log.Printf("snapshot %q: couldn't move it to the trash (%v); deleting it in place", name, rerr)
		return os.RemoveAll(m.snapDir(name))
	}
	return os.RemoveAll(destino)
}

// retirarSnapshot comprueba que nadie usa el snapshot name y lo quita de su
// sitio con apartar, todo bajo el mismo m.mu (ver removeSnapshot). err es que
// no se puede retirar; rerr, que apartar falló (y el directorio sigue ahí).
func (m *Manager) retirarSnapshot(name string, propio bool, apartar func(string) (string, error)) (destino string, restos bool, rerr, err error) {
	if _, err := m.loadSnapshot(name); err != nil {
		// Sin meta.json pero con directorio: son los restos de un commit que se
		// interrumpió antes de escribirlo (el meta es lo último). Antes esto
		// abortaba aquí, así que esos GiB solo se recuperaban con un rm -rf a
		// mano, y `commit -replace` con el mismo nombre fallaba igual.
		if !restosDeCommit(m.snapDir(name)) {
			return "", false, nil, err
		}
		restos = true
	}

	m.mu.Lock()
	enUso := m.reserved[reservaSnapshot(name)]
	if propio {
		enUso-- // la reserva del propio commit que reemplaza
	}
	if enUso > 0 {
		m.mu.Unlock()
		if restos {
			return "", restos, nil, fmt.Errorf("snapshot %q is being committed right now", name)
		}
		return "", restos, nil, fmt.Errorf("snapshot %q is in use right now (an instance is being restored from it, "+
			"or it is being committed); retry in a moment", name)
	}
	if !restos {
		var users []string
		for _, mc := range m.byID {
			if mc.From == name && mc.State != api.StateStopped {
				users = append(users, mc.Name)
			}
		}
		if len(users) > 0 {
			m.mu.Unlock()
			return "", restos, nil, fmt.Errorf("snapshot %q has %d live instance(s) (%v)", name, len(users), users)
		}
	}
	destino, rerr = apartar(name)
	m.mu.Unlock()
	// El directorio ya no está donde estaba (o está a punto de dejar de
	// estarlo): lo que loadSnapshotCached/hotMemFilesMiBLocked recordaban de
	// este nombre ya no vale (M-08, M-12).
	m.invalidateSnapCache(name)
	return destino, restos, rerr, nil
}

// sufijoAnterior marca el dorado viejo que un `commit -replace` apartó
// mientras escribe el nuevo: snapshots/.<nombre>.anterior-<ns>. Empieza por
// punto: ni Snapshots ni los barridos lo toman por un snapshot.
const sufijoAnterior = ".anterior-"

// apartarAnterior es el apartar de un commit -replace: el dorado viejo no va
// a la papelera (que se vacía sola), va al lado, para poder devolverlo si el
// nuevo no llega a completarse.
func (m *Manager) apartarAnterior(name string) (string, error) {
	destino := filepath.Join(m.root, "snapshots", "."+name+sufijoAnterior+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Rename(m.snapDir(name), destino); err != nil {
		return "", err
	}
	return destino, nil
}

// recuperarReemplazos resuelve, al arrancar, los `commit -replace` que un
// daemon anterior dejó a medias: si el nuevo dorado llegó a su meta.json, el
// viejo sobra; si no, el viejo vuelve a su sitio y lo a medias se aparta.
func (m *Manager) recuperarReemplazos() {
	base := filepath.Join(m.root, "snapshots")
	entradas, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entradas {
		nombre, _, ok := strings.Cut(strings.TrimPrefix(e.Name(), "."), sufijoAnterior)
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".") || !ok || !validName.MatchString(nombre) {
			continue
		}
		anterior := filepath.Join(base, e.Name())
		_, err := m.loadSnapshot(nombre)
		if esquema.EsMasNuevo(err) {
			// El nuevo lo escribió un kling más nuevo: está entero, pero este
			// binario no lo entiende. No se decide nada; los dos se quedan.
			log.Printf("snapshot %q: %v; leaving the old one at %s", nombre, err, anterior)
			continue
		}
		if err == nil {
			log.Printf("snapshot %q: the replacement finished before the daemon stopped; removing the old one", nombre)
			_ = os.RemoveAll(anterior)
			continue
		}
		if _, err := os.Stat(m.snapDir(nombre)); err == nil {
			if _, err := m.apartarSnapshot(nombre); err != nil {
				log.Printf("snapshot %q: couldn't set the unfinished replacement aside (%v); the old one stays at %s",
					nombre, err, anterior)
				continue
			}
		}
		if err := os.Rename(anterior, m.snapDir(nombre)); err != nil {
			log.Printf("snapshot %q: couldn't restore the old one from %s: %v", nombre, anterior, err)
			continue
		}
		m.invalidateSnapCache(nombre)
		log.Printf("snapshot %q: the replacement didn't finish; the previous snapshot is back", nombre)
	}
}

// apartarSnapshot mueve el directorio del snapshot a la papelera y devuelve
// dónde quedó. Solo renombra: se puede (y se debe) llamar con m.mu tomado.
func (m *Manager) apartarSnapshot(name string) (string, error) {
	papelera := filepath.Join(m.root, "machines", papeleraDir)
	if err := os.MkdirAll(papelera, 0o700); err != nil {
		return "", err
	}
	destino := filepath.Join(papelera, fmt.Sprintf("snap-%s-%d", name, time.Now().UnixNano()))
	if err := os.Rename(m.snapDir(name), destino); err != nil {
		return "", err
	}
	return destino, nil
}

// apartarParaReemplazo retira el snapshot name para que un commit -replace
// escriba el nuevo, sin borrarlo: devuelve dónde quedó (ver apartarAnterior).
func (m *Manager) apartarParaReemplazo(name string) (string, error) {
	destino, _, rerr, err := m.retirarSnapshot(name, true, m.apartarAnterior)
	if err != nil {
		return "", err
	}
	if rerr != nil {
		return "", fmt.Errorf("setting the old snapshot aside: %w", rerr)
	}
	return destino, nil
}

// explainRestoreErr traduce los fallos de restauración de Firecracker con causa
// conocida a un error que dice qué pasó y qué hacer. Los que no reconoce pasan
// intactos: adornar un error sin entenderlo es peor que dejarlo crudo.
//
// El caso que motiva esto: el snapshot graba la frecuencia del TSC del host, y
// esa frecuencia se mide de nuevo en cada arranque — tras reiniciar el host,
// TODOS los snapshots anteriores dejan de restaurar a la vez, con un
// "Could not set TSC scaling ... Invalid argument (os error 22)" que no apunta
// a nada. La recuperación es siempre la misma: rehacer el snapshot.
func explainRestoreErr(err error, what, remedy string) error {
	if !api.EsFalloTSC(err) {
		return err
	}
	return fmt.Errorf("%s can't be restored on this host: the snapshot records the CPU's TSC "+
		"frequency, which changes on every host boot, so a host reboot invalidates every "+
		"snapshot taken before it (this is a Firecracker limitation, not corruption).\n"+
		"Recover by recreating it:\n%s\nUnderlying error: %v", what, remedy, err)
}

// runFrom instancia una microVM desde un snapshot dorado.
//
// Todas las instancias mapean el MISMO fichero de memoria: Firecracker lo mapea
// en privado, así que comparten las páginas que no escriben y solo divergen las
// que tocan. La segunda instancia y las siguientes salen casi gratis en RAM.
func (m *Manager) runFrom(ctx context.Context, req api.RunRequest) (*api.Machine, error) {
	// Antes de reservar el snapshot y de publicar nada (ver Run).
	if m.JailerBlocked != "" {
		return nil, errors.New(m.JailerBlocked)
	}
	// El snapshot queda RESERVADO mientras dure la restauración, desde antes de
	// leer su meta.json: RemoveSnapshot se niega a borrarlo mientras tanto. Sin
	// esto, un `kling snapshots rm` durante la copia del overlay o el montaje de
	// la red —la ventana más ancha del daemon, antes de aparecer en byID— le
	// quitaba el mem.file a LoadSnapshot (M-15). Se suelta al volver: para
	// entonces la instancia, si arrancó, ya cuenta como viva en byID. El
	// nombre se valida antes: no se reserva "snap:../../etc".
	if !validName.MatchString(req.From) {
		return nil, fmt.Errorf("invalid snapshot name: %q", req.From)
	}
	defer m.reserveDir(reservaSnapshot(req.From))()

	// Cacheado (M-08): esto se llama en cada instanciación desde este dorado, y
	// el meta.json no cambia entre una y la siguiente.
	snap, _, err := m.loadSnapshotCached(req.From)
	if err != nil {
		return nil, err
	}
	// OBSOLETO: hecho con un VMM que el de ahora no sabe cargar. Se falla
	// aquí, antes de hashear, copiar ni arrancar nada: el error crudo del VMM
	// al cargar no dice que la causa es la actualización (meta.go).
	if causa := causaObsoleto(snap.VMM, m.origenActual().vmm); causa != "" {
		return nil, errObsoleto(req.From, causa)
	}

	// INTEGRIDAD. Antes de tocar nada: si el rootfs dorado o el volcado de estado
	// se corrompieron en disco desde que se congelaron, restaurar produciría una
	// microVM en un estado que ya no es el suyo —o un pánico del invitado— sin una
	// sola señal de la causa. Se falla aquí, claro y pronto, antes de copiar el
	// overlay y de arrancar el VMM.
	if err := m.comprobarFirma(snap); err != nil {
		return nil, err
	}
	if err := m.verifyIntegrity(snap, m.snapDir(req.From)); err != nil {
		return nil, err
	}
	// KERNEL (K2): solo se avisa. Ver avisoKernel.
	if aviso := m.avisoKernel(snap.KernelSHA256, fmt.Sprintf("snapshot %q", req.From)); aviso != "" {
		log.Print(aviso)
	}
	// IPv6 EN EL INVITADO (F2): solo se avisa, y una vez por dorado. Ver
	// avisoIPv6Invitado.
	// Si la imagen del dorado pide la pila IPv6, no hay nada que avisar: es lo
	// declarado, y rehacer el dorado no lo cambiaría.
	if aviso := m.avisoIPv6Invitado(req.From, snap.GuestIPv6Off || m.ipv6DeReceta(snap.Image)); aviso != "" {
		log.Print(aviso)
		// m.bus es nil en algún arnés de test que ejercita runFrom sin
		// necesitar el bus de eventos para nada más; en producción (NewManager)
		// siempre está.
		if m.bus != nil {
			m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvGuestIPv6, Name: req.From, Message: aviso})
		}
	}

	// La ejecución se encendió (o no) al arrancar la plantilla, en la línea de
	// comandos del kernel que se congeló con la memoria: no se puede conceder al
	// restaurar. Pedirla sobre un snapshot que no la tiene es un error, no un
	// silencio que acabaría en un 404 del invitado.
	if req.AllowExec && !snap.AllowExec {
		return nil, fmt.Errorf("%w: snapshot %q was made from a machine without exec; "+
			"commit one created with allow_exec (kling run -allow-exec) to use it as a sandbox",
			ErrExecNotInSnapshot, req.From)
	}

	// HERENCIA desde el snapshot. RunRequest ya promete que "el resto de campos
	// se heredan del snapshot", y la política de red no era una excepción: sin
	// esto, un servicio importado con -egress internet despertaba SIEMPRE sin
	// red, porque quien lo instancia —el gateway, el fondo, el modo efímero—
	// solo conoce el nombre del snapshot. El síntoma era un "fetch failed"
	// dentro del invitado que no señalaba a ninguna parte.
	//
	// Lo que venga en la petición manda; el snapshot solo rellena el hueco.
	if req.Egress == "" {
		req.Egress = snap.Egress
		// Los dominios permitidos viajan con el egress: si se hereda uno, se hereda
		// el otro, o un servicio importado con allowlist despertaría con la lista
		// vacía —sin poder salir a ninguno de sus dominios— sin señal de por qué.
		if len(req.AllowDomains) == 0 {
			req.AllowDomains = snap.AllowDomains
		}
	}
	// Credenciales de plantilla (credenciales.go): se entregan al final, con la
	// máquina viva. Se comprueba YA que se van a poder entregar, porque una
	// réplica de un servicio sin su clave es una réplica rota, y mejor decirlo
	// antes de gastar un arranque que descubrirlo por un 401 dentro.
	credsPlantilla, err := m.cargarCredencialesPlantilla(req.From)
	if err != nil {
		return nil, err
	}
	if len(credsPlantilla) > 0 && req.Egress != string(knet.EgressAllowlist) {
		// Los dominios concretos van en el mensaje: "sus dominios" obliga a ir a
		// buscarlos a otro sitio (snapshots inspect) antes de poder arrancar.
		return nil, fmt.Errorf("template %s has credentials, which need -egress allowlist (this instance would have %q); "+
			"run it with -egress allowlist -allow %s, or clear them with kling template credential %s -clear",
			req.From, req.Egress, strings.Join(snap.AllowDomains, ","), req.From)
	}
	// El techo de CPU, igual. El planificador ya lo pasaba a mano, pero `kling
	// run -from` no: la réplica caía al 50 % de un core del daemon aunque el
	// dorado se hubiera hecho con más. Para un modelo VON de 2 vCPU eso es
	// cuatro veces más lento, y el síntoma —un modelo que genera a paso de
	// tortuga— no apunta al snapshot.
	// Precedencia: el flag > el dorado > la receta de la imagen > el valor
	// por defecto de quien pide > el del daemon (ver techoCPUPorDefecto).
	// Fijo (sin impulso de arranque) si lo fijó quien hizo el dorado, o si
	// quien pide trae uno distinto del del dorado: el planificador del gateway
	// manda siempre el del dorado, y eso no es pedirlo.
	cpuFijo := snap.CPUPctFixed || (req.CPUPct > 0 && req.CPUPct != snap.CPUPct)
	if req.CPUPct <= 0 {
		req.CPUPct = snap.CPUPct
	}
	if req.CPUPct <= 0 {
		req.CPUPct = m.techoCPUPorDefecto(snap.Image, max(req.VCPUs, snap.VCPUs), req.CPUPctDefault)
	}

	id := newID()
	if req.Name == "" {
		req.Name = req.From + "-" + id[:6]
	}
	// Reserva de memoria, igual que en Run y por la misma razón: el gateway
	// despierta varios servicios a la vez cuando el anfitrión aprieta, y sin
	// reservar veían todos la misma memoria libre. Ahora el segundo ve que no
	// cabe y el gateway hace sitio antes de reintentar.
	// La clave de compartición es el snapshot de origen: todas sus instancias
	// mapean el MISMO mem.file dorado, así que la segunda y siguientes solo
	// reservan su fracción divergente. Es aquí donde la densidad se vuelve real.
	if err := m.admitir(); err != nil {
		return nil, err
	}
	releaseMem, merr := m.reserveMemoryMakingRoom(ctx, snap.MemMiB, req.From, "")
	if merr != nil {
		return nil, merr
	}
	defer releaseMem()

	// El directorio nace RESERVADO: desde aquí hasta la entrada en byID —copiar
	// el overlay dorado, resolver volúmenes, montar la red— es de lejos la
	// ventana más ancha del daemon, y el barrido de huérfanos corre cada 10 s en
	// cuanto el disco aprieta. Ver makeMachineDir.
	dir, unreserve, err := m.makeMachineDir(id)
	if err != nil {
		return nil, err
	}
	defer unreserve()

	// Copia del overlay dorado: mismo contenido, fichero propio. Compartirlo
	// haría que las instancias se pisaran el disco entre ellas. Con reflink
	// (nativo o en el almacén) la copia comparte los bloques del dorado hasta
	// que la instancia escribe: coste constante, sea cual sea su tamaño (ver
	// cow.go).
	overlay := filepath.Join(dir, "overlay.ext4")
	modoCoW, err := m.clonarOverlayInstancia(ctx, req.From, filepath.Join(m.snapDir(req.From), "overlay.ext4"), id, overlay)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("copying golden overlay: %w", err)
	}

	// Namespace propio, pero con tap0 y la misma IP interna que tenía la máquina
	// al congelarse: es lo que permite que un solo snapshot sirva para N copias.
	egress, err := knet.ParseEgress(req.Egress)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	// El volumen se resuelve ANTES de arrancar nada: si se pide uno sobre un
	// snapshot que no lo lleva, hay que decirlo aquí y no tras el arranque.
	// El volumen se HEREDA del snapshot, igual que la política de salida: quien
	// despierta un servicio no tiene por qué saber con qué volumen se importó,
	// y el gateway desde luego no lo sabe.
	if len(req.VolumeSet()) == 0 {
		req.Volumes = snap.VolumeSet()
	}
	vols, verr := m.reservarVolumenes(req, id, req.Name)
	// Igual que en Run: se suelta al salir, haya publicado o no. Aqui la ventana
	// era la mas ancha del daemon —incluye copiar el overlay del dorado— y por
	// eso reservar es lo que de verdad la cierra.
	defer m.soltarReservas(id)
	if verr != nil {
		os.RemoveAll(dir)
		return nil, verr
	}
	for _, v := range vols {
		if !v.readOnly {
			repairVolume(ctx, v.path)
		}
	}
	// El CONJUNTO de discos quedó fijado al congelar: Firecracker no admite
	// añadir ni quitar discos a una VM restaurada. Solo se puede reapuntar cada
	// uno a otro fichero, así que el número tiene que coincidir exactamente.
	grabados := snap.VolumeSet()
	if n, quiere := len(grabados), len(vols); n != quiere {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("service %q was imported with %d volume(s) and is being given %d.\n"+
			"A restored microVM can't have disks added or removed: reimport it if you want to change it",
			req.From, n, quiere)
	}
	// El MODO tampoco se puede cambiar, y esto es lo que impedía verlo.
	//
	// PatchDrive solo reapunta el fichero: is_read_only quedó fijado al congelar,
	// igual que el conjunto de discos. Pedir :ro sobre un snapshot que se congeló
	// en escritura no monta nada en solo lectura — monta en ESCRITURA y encima
	// engaña a la contabilidad, que lo apunta como lector y deja entrar a más.
	// El resultado son varios ext4 montados en escritura sobre el mismo fichero,
	// que es exactamente lo que el resto de este fichero existe para impedir.
	//
	// El punto de montaje va por el mismo camino: viaja en la línea de comandos
	// del kernel, que se congeló con la memoria.
	for i, v := range vols {
		g := grabados[i]
		if v.readOnly != g.ReadOnly {
			os.RemoveAll(dir)
			modo := func(ro bool) string {
				if ro {
					return "read-only"
				}
				return "write"
			}
			return nil, fmt.Errorf("volume %q was frozen in %s and is being requested in %s.\n"+
				"The mode is RECORDED in the snapshot and can't change on restore: "+
				"reimport the service if you want the other one",
				v.name, modo(g.ReadOnly), modo(v.readOnly))
		}
		if v.mount != g.Mount {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("volume %q was frozen mounted at %s and is being requested at %s.\n"+
				"The mount point travels in the kernel command line, which was frozen "+
				"with memory: reimport the service if you want to move it",
				v.name, g.Mount, v.mount)
		}
	}

	netcfg, err := m.asignarRed(id)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := m.montarRed(netcfg, id, egress, req.AllowDomains); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("setting up network: %w", err)
	}
	// Con el overlay en el almacén, overlay es un enlace a él y Own (chown, que
	// sigue enlaces) se lo daría en propiedad al VMM: ahí es del daemon y el VMM
	// lo usa por grupo (ver clonarInstancia).
	ceder := []string{dir, overlay}
	if modoCoW == cowModoStore {
		ceder = ceder[:1]
	}
	if err := m.priv.Own(ceder...); err != nil {
		m.desmontarRed(netcfg, id)
		os.RemoveAll(dir)
		return nil, err
	}

	creada := time.Now()
	mc := &api.Machine{
		ID: id, Name: req.Name, Image: snap.Image, From: req.From,
		State: api.StateCreated, VCPUs: snap.VCPUs, MemMiB: snap.MemMiB, MemMaxMiB: snap.MemMaxMiB,
		IP: netcfg.NSIP, NetIndex: netcfg.Index, Egress: string(egress),
		AllowDomains: req.AllowDomains,
		TTLSeconds:   req.TTLSeconds, CPUPct: req.CPUPct, CPUPctFixed: cpuFijo,
		Volumes:   attachments(vols),
		AllowExec: snap.AllowExec, OnTTL: req.OnTTL,
		// Las etiquetas del snapshot se heredan; las de la petición mandan.
		Labels:    api.MergeLabels(sinEtiquetasGrafo(snap.Labels), req.Labels),
		CreatedAt: creada,
		TTLAt:     &creada,
	}
	// El cerrojo de ciclo de vida, igual que en Run y por lo mismo (M-06): sin
	// él, un Remove concurrente veía la máquina en created con PID 0, borraba
	// su directorio y su entrada, y esta función seguía restaurando un VMM que
	// acababa huérfano. Justo antes de publicarla: hasta aquí nadie puede
	// nombrarla (ver doc.go).
	soltarCiclo := m.lockUnaVez(id)
	defer soltarCiclo()
	m.mu.Lock()
	m.byID[id] = mc
	m.persist()
	m.mu.Unlock()

	// Puerta de arranque: restaurar es cargar un snapshot en KVM, tan intensivo
	// como encender en frío, y es EL camino del gateway cuando despierta varios
	// servicios de golpe. Sin acotarlo, esa ráfaga simultánea cuelga el host bajo
	// anidamiento. El defer suelta el hueco al volver; si el contexto se cancela en
	// la cola, se deshace lo ya montado igual que un fallo de spawn.
	release, glErr := m.enterLaunch(ctx)
	if glErr != nil {
		m.desmontarRed(netcfg, id)
		m.fail(mc, glErr)
		return nil, glErr
	}
	defer release()

	var sock string
	var pid int
	var c *fc.Client
	snapDir := m.snapDir(req.From)

	// La máquina queda en disco ANTES de que exista su VMM (ver persistirYa).
	m.persistirYa()

	// El VMM nace ya en su cgroup, como en Thaw, con el techo de ARRANQUE: un
	// núcleo entero mientras restaura y hasta que contesta su agente (resync);
	// después, el configurado. El defer lo baja en cualquier salida. Ver
	// arranque_cpu.go. El techo por defecto, bajo el candado: mc está
	// publicada desde arriba, y List()/Get()/persist() la copian desde otras
	// goroutines (M-02).
	if mc.CPUPct <= 0 {
		m.mu.Lock()
		mc.CPUPct = techoDelDaemon(mc.VCPUs)
		m.mu.Unlock()
	}
	impulso := m.nuevoImpulso(id, mc.CPUPct, mc.VCPUs, mc.CPUPctFixed)
	defer impulso.fin()
	cg := m.cgroupParaLanzar(id, impulso.tope)
	if cg != nil {
		defer cg.Close()
	}
	var enCg bool

	// abortar es la única salida de error a partir de aquí.
	//
	// m.fail() llama a kill(), que lee el PID de la MÁQUINA, y ese PID no se
	// escribe hasta el final de esta función: sin matar explícitamente el proceso
	// que acabamos de lanzar, cada restauración fallida —un snapshot con el TSC
	// invalidado tras reiniciar el host, por ejemplo— deja un firecracker vivo
	// reteniendo su RAM, invisible para `kling ps` y para la contabilidad de
	// memoria. El arranque en frío ya aprendió esta lección (ver Run); este
	// camino no la había copiado.
	abortar := func(err error) (*api.Machine, error) {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		m.desmontarRed(netcfg, id)
		m.fail(mc, err)
		return nil, err
	}
	if m.JailerBlocked != "" {
		return abortar(errors.New(m.JailerBlocked))
	}
	if m.jailerJailed {
		// Restauración dentro de un jail: firecracker corre chrooteado. Todo lo
		// que va a abrir tiene que estar replicado dentro del jail EN SU RUTA
		// ABSOLUTA, porque LoadSnapshot abre cada drive con el path que quedó
		// grabado —comprobado en el laboratorio—.
		pid, sock, enCg, err = m.spawnJailed(id, netcfg, cg)
		if err != nil {
			return abortar(err)
		}
		c = fc.New(sock)
		if err := waitSocket(ctx, c); err != nil {
			return abortar(err)
		}
		// Poblar el jail entre el arranque de jailer y la carga: el snapshot y
		// sus discos, el rootfs base, el overlay dorado (que el snapshot abre) y
		// la copia propia, más los volúmenes.
		volPaths := make([]string, len(vols))
		for i, v := range vols {
			volPaths[i] = v.path
		}
		// Los discos de solo lectura de la imagen: la base y, si el servicio se
		// empaquetó por capas, su capa.
		//
		// NO se re-pasa kling.layer= aquí, ni haría falta: la línea de comandos del
		// kernel se congeló DENTRO de la memoria, y el invitado restaurado despierta
		// con la capa ya montada en su tabla de montajes. Lo único que hace falta es
		// que el fichero siga estando donde el snapshot lo grabó — que es justo lo
		// que hace este enlace. Por eso tampoco hay campo Layer en el meta: se
		// resuelve del nombre de la imagen, como la base, y los snapshots viejos no
		// necesitan nada nuevo dentro.
		imgBase, imgLayer, ierr := m.imageLayer(snap.Image)
		if ierr != nil {
			return abortar(ierr)
		}
		toLink := append([]string{
			filepath.Join(snapDir, "snap.file"),
			filepath.Join(snapDir, "mem.file"),
			imgBase,
			imgLayer,
			overlay,
		}, volPaths...)
		if err := m.prepareJail(id, toLink...); err != nil {
			return abortar(err)
		}
		// El snapshot grabó la ruta del overlay DEL DORADO, y Firecracker la abre
		// en lectura y escritura al cargarlo, antes de que el PATCH de abajo la
		// cambie por la de esta instancia. En esa ruta, dentro del jail, va la
		// copia propia de la instancia (mismo contenido): el dorado, que es de
		// root y de solo lectura para el VMM, no se le expone nunca.
		if err := m.linkComo(id, filepath.Join(snapDir, "overlay.ext4"), overlay); err != nil {
			return abortar(err)
		}
	} else {
		sock = filepath.Join(dir, "fc.sock")
		_ = os.Remove(sock)
		pid, enCg, err = m.spawn(id, sock, netcfg, cg)
		if err != nil {
			return abortar(err)
		}
		c = fc.New(sock)
		if err := waitSocket(ctx, c); err != nil {
			return abortar(err)
		}
	}

	// macOS: la política de salida viaja con la instancia, no con el snapshot.
	if err := m.redAntesDeArrancar(ctx, c, id); err != nil {
		return abortar(err)
	}
	start := time.Now()
	// Pausada: hay que reapuntar el overlay antes de dejarla correr. Con el
	// plazo del volcado y no el de 30 s: cargar también es mover la memoria
	// entera (F-01).
	// Con seguimiento de páginas sucias si la copia se va a congelar en
	// diferencial (diff_volcado.go): solo lo que escriba desde el dorado.
	enDiff := congelarEnDiff()
	if err := c.ConPlazo(plazoVolcado(max(snap.MemMiB, snap.MemMaxMiB))).LoadSnapshotTracking(ctx,
		filepath.Join(snapDir, "snap.file"),
		filepath.Join(snapDir, "mem.file"), false, enDiff); err != nil {
		// Con causa conocida (TSC tras reiniciar el host) se traduce ANTES de
		// propagar: este error acaba en el 502 del gateway y en el CLI, y el
		// texto crudo de Firecracker no le dice a nadie qué hacer.
		err = explainRestoreErr(err, fmt.Sprintf("snapshot %q", req.From), fmt.Sprintf(
			"  kling mcp import %s -force    (imported MCP service)\n"+
				"  kling commit -replace <machine> %s    (manual snapshot)", req.From, req.From))
		// Si no era el TSC, lo que se sepa de con qué se hizo (meta.go).
		if !api.EsFalloTSC(err) {
			err = m.explicarRestauracion(err, req.From, snap)
		}
		return abortar(err)
	}
	// Nada de SetEntropy aquí: tras cargar un snapshot no se pueden añadir
	// dispositivos. El virtio-rng ya viene dentro, porque la plantilla lo tenía
	// al congelarse; y CONFIG_VMGENID hace que el invitado resiembre su pool al
	// detectar que ha sido restaurado.
	if err := c.PatchDrive(ctx, "overlay", overlay); err != nil {
		return abortar(fmt.Errorf("repointing overlay: %w", err))
	}
	// Cada volumen se reapunta igual que el overlay: el dispositivo ya existe
	// dentro del snapshot, y aquí solo se le dice a qué fichero del host mira.
	// Es lo que permite que el mismo snapshot dorado sirva a volúmenes distintos.
	//
	// El identificador tiene que ser el que quedó GRABADO al congelar, que en
	// los snapshots de una sola unidad era "volume" a secas. Equivocarlo deja a
	// la microVM mirando el fichero de otra máquina, o ninguno.
	// El nombre del disco se LEE del snapshot, no se deduce.
	//
	// Antes se deducía del meta.json (¿lista de volúmenes o campos sueltos?), y
	// eso rompía la cadena: el meta cambia en cada commit, el nombre del disco
	// dentro del VMM no. Restaurar de un snapshot legacy y volver a congelar esa
	// instancia producía un meta con lista sobre un VMM cuyo disco seguía
	// llamándose "volume" — y la siguiente restauración fallaba con drive not
	// found, de forma determinista y sin arreglo salvo reimportar.
	usados := make([]string, len(vols))
	for i, v := range vols {
		id, err := m.patchVolumeDrive(ctx, c, grabados[i], i, len(vols), v.path)
		if err != nil {
			return abortar(fmt.Errorf("repointing volume %s: %w", v.name, err))
		}
		usados[i] = id
	}
	// El nombre que de verdad funcionó se anota en la máquina, para que el
	// próximo commit lo escriba en su meta y la cadena se auto-repare: la
	// generación siguiente ya no necesita heurística ni reintento.
	m.mu.Lock()
	mc.Volumes = withDriveIDs(mc.Volumes, usados)
	m.mu.Unlock()
	if err := c.Resume(ctx); err != nil {
		return abortar(err)
	}
	// macOS: sin reenvíos el host no llega al invitado, y acquireVolumes de
	// aquí abajo ya necesita hablarle.
	if err := m.abrirReenvios(ctx, c, id); err != nil {
		return abortar(err)
	}
	// Reloj y CSPRNG propios ANTES de entregar la máquina: cada instancia de
	// este dorado despertó con la memoria de todas las demás. Síncrono y
	// acotado; un agente que no lo sabe hacer no bloquea (ver resync.go).
	resyncT, resyncOK, listo := m.resyncGuest(ctx, id, claveSnapshot(snap), api.ResyncInstance)
	// Y ahora que los discos apuntan a los ficheros de ESTA instancia, el
	// invitado los monta. Se congelaron desmontados a propósito, para que su
	// memoria no llevara dentro la caché de un ext4 que después cambia.
	//
	// Un fallo aquí es un fallo de la máquina: sin volumen, la herramienta
	// escribe en un directorio del overlay que muere con ella, y eso no da ni un
	// aviso hasta que alguien busca lo que guardó.
	if len(vols) > 0 {
		if err := m.acquireVolumes(mc); err != nil {
			return abortar(err)
		}
	}
	// Las credenciales de la plantilla, a ESTA instancia: marcadores nuevos en
	// su MMDS y la clave en su proxy. Antes de devolverla: el puente lee MMDS al
	// lanzar cada sesión, así que la primera ya nace con el marcador. Un fallo
	// aborta la restauración por lo dicho arriba.
	var dominiosCred, anyDBCred []string
	if len(credsPlantilla) > 0 {
		creds, _, err := m.entregarCredenciales(ctx, id, netcfg, c, credsPlantilla)
		if err != nil {
			return abortar(fmt.Errorf("handing %s the credentials of template %s: %w", mc.Name, req.From, err))
		}
		dominiosCred, anyDBCred = dominiosDe(creds), anyDatabaseDe(creds)
	}
	elapsed := time.Since(start).Milliseconds()

	// Si el kernel no lo dejó nacer en su cgroup, se mete ahora, con el techo
	// de arranque: lo baja entregarRestaurada, abajo.
	if !enCg {
		if warn := m.limitCPU(mc.ID, pid, impulso.tope); warn != "" {
			log.Printf("warning: %s: %s", mc.Name, warn)
		}
	}

	m.mu.Lock()
	now := time.Now()
	mc.PID = pid
	mc.State = api.StateRunning
	mc.StartedAt = &now
	mc.ThawMS = elapsed
	mc.CredentialDomains, mc.CredentialAnyDatabase = dominiosCred, anyDBCred
	// En Firecracker la RAM de la copia es el mem.file del dorado, MAP_PRIVATE:
	// compartida con las demás copias hasta que la escriben (ver Squeeze).
	mc.MemShared = restaurarComparteMemoria
	if enDiff {
		mc.DiffBase = filepath.Join(snapDir, "mem.file")
	}
	m.socket[id] = sock
	m.persist()
	m.mu.Unlock()

	// Volúmenes montados y credenciales en MMDS: ahora los ganchos de la
	// imagen (identidad por copia, etc.), en segundo plano.
	m.trasRestaurar(ctx, id, api.ResyncInstance, listo)
	// Fin del impulso de arranque: ya, si el dorado se guardó listo (lo
	// normal) o no declara sonda; si no, cuando la pase.
	impulso.entregarRestaurada(listo)
	m.mu.RLock()
	out := *mc
	m.mu.RUnlock()

	m.bus.Publish(api.Event{Time: now, Type: api.EvStarted, ID: id, Name: mc.Name,
		Message: fmt.Sprintf("instantiated from %s in %d ms%s", req.From, elapsed, resyncNota(resyncT, resyncOK))})
	return &out, nil
}

// writeMeta guarda el meta.json de un snapshot de forma atómica.
//
// Al lado y renombrar, nunca encima. os.WriteFile TRUNCA primero: un corte a
// media escritura —o simplemente un lector concurrente, y Snapshots() se sirve
// en cada petición del gateway— deja un meta vacío o partido. El servicio
// "desaparece" del catálogo, y encima de forma PERMANENTE: RemoveSnapshot se
// niega a borrar lo que no puede leer, así que el directorio queda varado.
//
// El renombrado dentro de un mismo sistema de ficheros es atómico: o está el
// meta viejo o el nuevo. El proyecto ya usa este patrón en writePending y
// saveRecipe; aquí faltaba.
func writeMeta(dir string, b []byte) error {
	// Durable, no solo atomico. Un meta perdido en un corte no da error: el
	// servicio DESAPARECE del catalogo en silencio (Snapshots() salta lo que no
	// puede leer), y RemoveSnapshot se niega despues a borrar lo ilegible, asi
	// que el directorio queda varado con su mem.file de cientos de MB.
	return durable.Escribir(filepath.Join(dir, "meta.json"), b, 0o644)
}

// patchVolumeDrive reapunta un disco de volumen y devuelve el nombre que de
// verdad funcionó.
//
// Prefiere el grabado en el snapshot. Si no lo hay —snapshots anteriores al
// campo— cae en la heurística de siempre, y si esa falla con un solo volumen,
// reintenta con el nombre antiguo: eso cura los snapshots que la heurística ya
// dejó rotos, sin exigir reimportar el servicio.
func (m *Manager) patchVolumeDrive(ctx context.Context, c *fc.Client,
	grabado api.VolumeAttachment, i, total int, path string) (string, error) {

	id := grabado.DriveID
	if id == "" {
		id = volumeDriveID(i)
	}
	err := c.PatchDrive(ctx, id, path)
	if err == nil {
		return id, nil
	}
	// Con un solo volumen no hay ambigüedad posible: o el disco se llama
	// "volume0" o se llama "volume", nunca los dos. Así que reintentar no puede
	// reapuntar el disco de otro por error.
	if total == 1 && id != legacyVolumeDriveID {
		if err2 := c.PatchDrive(ctx, legacyVolumeDriveID, path); err2 == nil {
			log.Printf("this snapshot's disk is called %q, not %q (legacy chain); "+
				"noted for next time", legacyVolumeDriveID, id)
			return legacyVolumeDriveID, nil
		}
	}
	return "", err
}

// withDriveIDs devuelve los adjuntos con el nombre de disco que REALMENTE
// funcionó, para que el próximo commit lo escriba en su meta y la cadena se
// auto-repare.
//
// Devuelve un slice NUEVO y no toca vols. Antes escribía en su sitio, y vols
// es el de una máquina ya publicada: cualquier copia por valor tomada antes
// (List, Get, la foto de persist) compartía ese mismo array, y json.Marshal lo
// leía fuera del lock mientras esto lo escribía (M-03). Quien llama asigna el
// resultado bajo m.mu.
func withDriveIDs(vols []api.VolumeAttachment, ids []string) []api.VolumeAttachment {
	out := append([]api.VolumeAttachment(nil), vols...)
	for i := range out {
		if i < len(ids) && ids[i] != "" {
			out[i].DriveID = ids[i]
		}
	}
	return out
}

// reservaSnapshot es la clave con la que un commit en curso reserva su
// directorio en m.reserved, el mismo registro que protege los de las máquinas.
func reservaSnapshot(name string) string { return "snap:" + name }

// restosDeCommit dice si dir es un snapshot a medias: existe pero no tiene
// meta.json, que es lo último que escribe un commit.
func restosDeCommit(dir string) bool {
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "meta.json"))
	return errors.Is(err, os.ErrNotExist)
}

// sweepSnapshotLeftovers aparta a la papelera los snapshots a medias que lleven
// más de dirGrace sin tocarse y que nadie esté escribiendo. Solo mueve (rápido);
// vaciarPapelera borra. Se llama con m.mu tomado.
func (m *Manager) sweepSnapshotLeftovers() {
	base := filepath.Join(m.root, "snapshots")
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if m.reserved[reservaSnapshot(e.Name())] > 0 || !restosDeCommit(filepath.Join(base, e.Name())) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) < dirGrace {
			continue
		}
		papelera := filepath.Join(m.root, "machines", papeleraDir)
		if err := os.MkdirAll(papelera, 0o700); err != nil {
			return
		}
		destino := filepath.Join(papelera, fmt.Sprintf("snap-%s-%d", e.Name(), time.Now().UnixNano()))
		if err := os.Rename(filepath.Join(base, e.Name()), destino); err != nil {
			log.Printf("reconcile: couldn't move the leftovers of snapshot %q: %v", e.Name(), err)
			continue
		}
		// Ya con m.mu tomado: se olvida en su sitio (ver invalidateSnapCache).
		delete(m.snapCache, e.Name())
		delete(m.memAllocCache, e.Name())
		log.Printf("reconcile: snapshot %q was an interrupted commit; its leftovers go to the trash", e.Name())
	}
}
