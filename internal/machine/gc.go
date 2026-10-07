package machine

// Recolección de espacio a largo plazo.
//
// Cada máquina warm conserva su mem.file, del tamaño de su RAM. Eso es correcto
// —es lo que permite descongelar en milisegundos—, pero significa que N
// servicios dormidos son N gigabytes en disco, sin que nada crezca "mal": es el
// coste normal acumulándose. En un anfitrión con disco holgado da igual; en uno
// justo, llena el disco y entonces NADA arranca, porque una microVM reserva su
// memoria por adelantado.
//
// La política: cuando el disco pasa de una marca alta, se ELIMINAN las
// instancias warm que se pueden recrear —las de un servicio (etiqueta
// service) que vienen de un snapshot dorado que sigue existiendo—, de más antigua a más nueva, hasta bajar de una marca
// objetivo. Eliminar no es perder: su estado vive en el snapshot, y volver
// cuesta ~200 ms. Lo que NO se toca es una warm sin snapshot de respaldo: ahí el
// mem.file ES el único estado, y borrarlo sería perder datos.

import (
	"context"
	"errors"
	"log"
	"os"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

const (
	// defaultDiskHighPct: por encima, se empieza a recuperar espacio.
	defaultDiskHighPct = 90
	// defaultDiskTargetPct: se recupera hasta bajar aquí. El hueco entre las dos
	// evita expulsar en cada tick por un fichero temporal que sube y baja.
	defaultDiskTargetPct = 80
)

// gcDiskHighPct es el umbral por encima del cual empieza la recuperación de
// disco. Ajustable con KLING_GC_DISK_HIGH (1-100), en la línea del resto de
// knobs del daemon (KLING_MAX_PARALLEL_BOOT, KLING_MIN_FREE_MIB…). Un valor
// fuera de rango se ignora y se usa el defecto.
func gcDiskHighPct() int {
	if v := os.Getenv("KLING_GC_DISK_HIGH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 100 {
			return n
		}
	}
	return defaultDiskHighPct
}

// gcDiskTargetPct es hasta dónde se recupera. Ajustable con KLING_GC_DISK_TARGET.
// Debe quedar por DEBAJO de la marca alta: el hueco es lo que evita expulsar en
// cada tick. Si el valor dado no respeta esa invariante, se acota a high-1, que
// es lo mínimo que sigue teniendo sentido.
func gcDiskTargetPct(high int) int {
	target := defaultDiskTargetPct
	if v := os.Getenv("KLING_GC_DISK_TARGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			target = n
		}
	}
	if target >= high {
		target = high - 1
	}
	return target
}

// diskUsedPct devuelve el porcentaje de disco usado en la raíz de datos, o -1 si
// no se puede saber (en cuyo caso quien llama no debe hacer nada: mejor no
// recolectar que recolectar a ciegas).
func (m *Manager) diskUsedPct() int {
	var st syscall.Statfs_t
	if err := syscall.Statfs(m.root, &st); err != nil {
		return -1
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	if total == 0 {
		return -1
	}
	return int(100 * (total - free) / total)
}

// gcDisk libera disco eliminando instancias warm recuperables si se pasa de la
// marca alta.
//
// Recuperable = viene de un snapshot dorado (From != "") que todavía existe. Se
// eliminan de más antigua a más nueva: la que lleva más tiempo dormida es la que
// menos probable es que se pida pronto.
// gcPausaInutil es cuanto se deja de expulsar cuando expulsar no sirve.
const gcPausaInutil = 30 * time.Minute

// evictarSirve dice si merece la pena seguir expulsando.
//
// La marca alta se mide sobre TODO el sistema de ficheros, no sobre la huella de
// kindling. Si lo que llena el disco es de otro —observado en el laboratorio:
// containerd con 7,8 GB de 20— expulsar no lo va a arreglar, y lo unico que
// consigue es destruir el warm-pooling, que es el valor entero del producto:
// cada instancia se expulsaba entre 1 y 11 segundos despues de congelarse, cada
// diez segundos, indefinidamente. El daemon lo decia en el log una vez por tick
// durante un dia entero y todas las llamadas pagaban un arranque en frio de
// 30-40 s sin que nada correlacionara las dos cosas.
//
// Sirve si bajo del umbral, o si al menos hizo progreso: parar tras una pasada
// que SI libero seria dejar el disco llenandose.
func evictarSirve(antes, despues, alta int) bool {
	return despues < alta || despues < antes
}

func (m *Manager) gcDisk(ctx context.Context) {
	if m.barridoBloqueado() {
		return
	}
	// En pausa: la pasada anterior no consiguio nada y el disco no es nuestro.
	if !m.gcPausadoHasta.IsZero() && time.Now().Before(m.gcPausadoHasta) {
		return
	}
	high := gcDiskHighPct()
	target := gcDiskTargetPct(high)
	pct := m.diskUsedPct()
	if pct < high {
		return
	}
	antes := pct

	type cand struct {
		id    string
		name  string
		from  string
		since int64
	}
	// La foto, bajo el candado; lo que mira el disco (retieneDatos, el meta
	// del dorado), fuera: con el disco lleno cada stat cuesta, y el candado
	// global no puede esperar a nadie.
	m.mu.RLock()
	var fotos []*api.Machine
	for _, mc := range m.byID {
		if mc.State != api.StateWarm || mc.From == "" {
			continue
		}
		fotos = append(fotos, mc.Clone())
	}
	m.mu.RUnlock()

	var cands []cand
	for _, mc := range fotos {
		// Solo las de un servicio (la etiqueta service: el planificador las
		// crea y las recrea igual desde el dorado del servicio). Cualquier
		// otra que haya corrido desde su dorado tiene en su overlay y en su
		// volcado lo que escribió desde entonces, y eso no está en ningún
		// otro sitio: una copia de kling db congelada al cambiar de rama, una
		// copia de fork, una máquina de run -from. El dorado no la "recrea":
		// la devuelve al principio. Se observó en el laboratorio: con el
		// disco al 88 % el GC borró copias congeladas de ramas de kling db.
		if mc.Service() == "" {
			continue
		}
		// Con volúmenes no es "recreable desde su snapshot": es una máquina
		// con estado, y el dorado no sabe nada de lo que pasó desde entonces.
		if m.retieneDatos(mc) != "" {
			continue
		}
		// Solo si su snapshot sigue ahí para recrearla. Sin él, esta warm es
		// irrecuperable y no se toca. Con la caché: varias copias del mismo
		// dorado no leen su meta.json una vez cada una.
		if _, _, err := m.loadSnapshotCached(mc.From); err != nil {
			continue
		}
		var since int64
		if mc.FrozenAt != nil {
			since = mc.FrozenAt.UnixNano()
		}
		cands = append(cands, cand{mc.ID, mc.Name, mc.From, since})
	}

	// Más antigua (FrozenAt menor) primero.
	sort.Slice(cands, func(i, j int) bool { return cands[i].since < cands[j].since })

	for _, c := range cands {
		if m.diskUsedPct() < target {
			return
		}
		// Se eligió con una foto: con el cerrojo tomado tiene que seguir
		// congelada y ser la MISMA congelación (FrozenAt). Si entre medias la
		// despertaron —un thaw, un renew seguido de thaw del gateway—, borrarla
		// era matar una máquina en uso con sus sesiones dentro.
		since := c.since
		sigue := func(mc *api.Machine) bool {
			var ahora int64
			if mc.FrozenAt != nil {
				ahora = mc.FrozenAt.UnixNano()
			}
			return mc.State == api.StateWarm && ahora == since
		}
		if err := m.removeSi(c.id, sigue); err != nil {
			if !errors.Is(err, errYaNoToca) {
				log.Printf("gc: couldn't remove dormant instance %s: %v", c.name, err)
			}
			continue
		}
		log.Printf("gc: disk at %d%%, removed dormant instance %s (recreates from %s)",
			m.diskUsedPct(), c.name, c.from)
	}

	if p := m.diskUsedPct(); p >= high {
		if evictarSirve(antes, p, high) {
			// Bajo algo pero no lo suficiente: se sigue en el proximo tick.
			log.Printf("gc: disk still at %d%% after recovering what could be recovered; "+
				"check images, volumes, or instances without a backup snapshot", p)
			return
		}
		// No bajo NADA. Lo que ocupa el disco no es de kindling, asi que seguir
		// expulsando solo cuesta warm-pooling. Se para un rato y se dice claro.
		m.gcPausadoHasta = time.Now().Add(gcPausaInutil)
		log.Printf("gc: evicting freed nothing and the disk is still at %d%% — what fills it "+
			"is NOT kindling's. Eviction paused for %s so warm instances stop being thrown "+
			"away for nothing; look at what else uses this filesystem (du -sh /var/lib/*).",
			p, gcPausaInutil)
	}
}

// defaultFailedRetention es cuánto se conserva una máquina failed antes de
// recogerla. Una hora: de sobra para que `kling ps -a` y su LastErr cuenten qué
// pasó, y lo bastante corto para que los intentos fallidos no se acumulen — se
// observaron nueve en 21 horas, uno por instanciación rota, para siempre.
const defaultFailedRetention = time.Hour

// defaultStoppedRetention es lo mismo para las paradas que se pueden recoger
// (ver retieneDatos). Más larga que la de las failed: una parada se puede
// volver a arrancar (kling start), y quien la paró puede volver mañana.
const defaultStoppedRetention = 24 * time.Hour

// failedRetention devuelve la retención, ajustable con KLING_FAILED_RETENTION
// (una duración de Go: "30m", "2h"; "0" desactiva la recogida).
func failedRetention() time.Duration {
	return retencion("KLING_FAILED_RETENTION", defaultFailedRetention)
}

// stoppedRetention es la de las paradas: KLING_STOPPED_RETENTION, "0" la
// desactiva.
func stoppedRetention() time.Duration {
	return retencion("KLING_STOPPED_RETENTION", defaultStoppedRetention)
}

func retencion(variable string, defecto time.Duration) time.Duration {
	if v := os.Getenv(variable); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return defecto
}

// gcFailed recoge las máquinas failed y stopped que ya cumplieron su tiempo
// de gracia.
//
// Automático y no un comando, por la misma razón que gcDisk: failed es un
// estado TERMINAL, así que conservarlas no da ninguna opción nueva — solo
// diagnóstico, y para eso basta la ventana de gracia. Una parada sí se puede
// arrancar otra vez (kling start), así que solo se recogen las que no pierden
// nada que no esté en otro sitio (retieneDatos: las instancias de un
// servicio, que su dorado recrea) y con una gracia más larga.
// Un daemon que exige limpieza manual de sus propios restos acumula restos,
// que es exactamente lo observado.
//
// Una failed o parada sin fecha (estado anterior al campo) no se borra al
// instante: se le arranca el reloj ahora y cae en la pasada que le toque. Es
// la diferencia entre recoger basura y borrar algo que quizá falló hace un
// minuto.
//
// El candado global solo para la foto y el sello de fecha: retieneDatos lee
// el disco (el sha256 del volcado de cada failed) y corre fuera, cada diez
// segundos, sin parar a nadie. Lo elegido con la foto se borra solo si, con
// el cerrojo de la máquina, sigue igual (removeSi): un start o un rm entre
// medias se respetan.
func (m *Manager) gcFailed() {
	retFallida, retParada := failedRetention(), stoppedRetention()
	if (retFallida <= 0 && retParada <= 0) || m.barridoBloqueado() {
		return
	}
	now := time.Now()

	var fotos []*api.Machine
	stamped := false
	m.mu.Lock()
	for _, mc := range m.byID {
		var desde **time.Time
		var ret time.Duration
		switch mc.State {
		case api.StateFailed:
			desde, ret = &mc.FailedAt, retFallida
		case api.StateStopped:
			desde, ret = &mc.StoppedAt, retParada
		default:
			continue
		}
		if ret <= 0 {
			continue
		}
		if *desde == nil {
			t := now
			*desde = &t
			stamped = true
			continue
		}
		if now.Sub(**desde) >= ret {
			fotos = append(fotos, mc.Clone())
		}
	}
	if stamped {
		m.persist()
	}
	m.mu.Unlock()

	for _, f := range fotos {
		if m.retieneDatos(f) != "" {
			continue
		}
		f := f
		sigue := func(cur *api.Machine) bool {
			return cur.State == f.State && mismaHora(cur.FailedAt, f.FailedAt) && mismaHora(cur.StoppedAt, f.StoppedAt)
		}
		if err := m.removeSi(f.ID, sigue); err != nil {
			if !errors.Is(err, errYaNoToca) {
				log.Printf("gc: couldn't collect %s machine %s: %v", f.State, f.Name, err)
			}
			continue
		}
		if f.State == api.StateFailed {
			log.Printf("gc: collected failed machine %s (failed with: %s)", f.Name, f.LastErr)
		} else {
			log.Printf("gc: collected stopped machine %s (stopped for over %s; KLING_STOPPED_RETENTION)", f.Name, retParada)
		}
	}
}

// mismaHora compara dos fechas opcionales.
func mismaHora(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// retieneDatos dice por qué borrar mc perdería algo que no existe en otro
// sitio, o "" si no pierde nada. La recogida automática (gcDisk, gcFailed)
// solo toca lo que devuelve "": una failed de verdad inservible, o una warm
// que se recrea igual desde su dorado. Lo demás lo retira el operador con
// `kling rm`, sabiendo lo que borra.
//
//   - Volúmenes enganchados: es una máquina con estado. El volumen sobrevive a
//     Remove, pero la máquina que lo usaba (y su overlay coherente con él) no.
//   - Un volcado completo en su directorio: una failed que lo tiene es casi
//     siempre un Thaw que falló, y ese mem.file + snap.file es el ÚNICO estado
//     de una warm. Borrarla a la hora era perder la máquina entera.
//   - Una arrancada en frío (sin From) cuyo VMM murió corriendo: su overlay es
//     el único disco que tiene, con todo lo que escribió.
//   - Una parada, salvo la instancia de un servicio que sale de su dorado (la
//     misma regla que gcDisk): su overlay tiene lo que escribió desde que
//     nació —una copia de kling db parada guarda su base de datos ahí—, y
//     kling start la arranca otra vez sobre él. Las paradas por un reinicio
//     del host (reconcile) son de estas.
//
// Lee el disco (el sha256 del volcado de una failed): quien llama le pasa una
// copia y no sostiene m.mu.
func (m *Manager) retieneDatos(mc *api.Machine) string {
	if len(mc.Volumes) > 0 {
		return "it has volumes attached"
	}
	switch mc.State {
	case api.StateFailed:
		if volcadoValido(m.dir(mc.ID)) == nil {
			return "it keeps a complete snapshot of its own"
		}
		if mc.From == "" && mc.LastErr == errProcesoDesaparecido {
			return "it was cold-booted and ran: its overlay is the only copy of its disk"
		}
	case api.StateStopped:
		if mc.From == "" || mc.Service() == "" {
			return "its overlay is the only copy of what it wrote (kling start boots it again)"
		}
	}
	return ""
}
