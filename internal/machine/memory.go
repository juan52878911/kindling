package machine

// No arrancar microVMs que no caben en el anfitrión.
//
// Una microVM pide su memoria por adelantado y el invitado la usa de verdad: no
// hay sobrecompromiso que valga cuando dentro corre un servidor que se la come.
// Arrancar la cuarta de 1024 MiB en un host que solo tiene 1,8 GiB no da un
// error de arranque — arranca, y después el OOM killer del ANFITRIÓN empieza a
// matar procesos al azar, incluidos otros firecracker y el propio daemon.
//
// El síntoma es de los peores del proyecto: microVMs que mueren solas, sin
// motivo en su consola, mientras el fallo real está en un journal del host que
// nadie mira. Comprobado en el laboratorio: nueve líneas de OOM del kernel y
// treinta de cuarenta llamadas fallando.

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/juan52878911/kindling/internal/api"
	"os"
	"strconv"
	"strings"
)

// hostReserveMiB es lo que se le deja al anfitrión pase lo que pase: el daemon,
// el gateway, sshd y el margen del propio kernel. Sin esta reserva, la última
// microVM que cabe deja al host sin aire y el OOM killer elige a quien quiera.
const hostReserveMiB = 384

// defaultMinFreeMiB es el suelo de memoria REALMENTE libre (MemFree) por debajo
// del cual no se arranca, pase lo que diga MemAvailable. Es la red que faltaba
// para los snapshots distintos: cuando cada mem.file caliente infla la caché,
// MemAvailable miente y sin este suelo el arranque fallaba al no poder ni crear
// sus ficheros. 384 MiB deja margen para que un arranque toque su working-set y
// el kernel respire. Ajustable con KLING_MIN_FREE_MIB (0 lo desactiva).
const defaultMinFreeMiB = 384

func minFreeMiB() int {
	if v := os.Getenv("KLING_MIN_FREE_MIB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultMinFreeMiB
}

// meminfoMiB lee un campo de /proc/meminfo en MiB. Devuelve 0 si no se puede.
func meminfoMiB(key string) int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, key+":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return 0
		}
		kb, err := strconv.Atoi(f[0])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}

// availableMiB devuelve la memoria realmente disponible del anfitrión.
//
// MemAvailable y no MemFree: incluye la caché reclamable, que es lo que de
// verdad se puede usar. MemFree solo mediría lo que está sin tocar, y en un host
// que lleva horas sirviendo imágenes eso es casi nada aunque haya gigas
// reclamables.
//
// Devuelve 0 si no se puede saber; quien llama debe tratarlo como "no comprobar"
// en vez de como "no hay memoria".
func availableMiB() int { return meminfoMiB("MemAvailable") }

// freeMiB devuelve la memoria REALMENTE libre (MemFree), sin contar caché.
//
// Es el complemento honesto de availableMiB para un caso concreto: muchas
// microVMs de snapshots DISTINTOS vivas a la vez. Cada una mantiene su mem.file
// en caché de página, que MemAvailable cuenta como "disponible" (es reclaimable)
// aunque esté caliente —reclamarla es re-faultear desde disco—. Así, con
// snapshots distintos MemAvailable se queda alto mientras MemFree se agota, y el
// arranque siguiente ni puede crear sus ficheros: falla feo en vez de con un 507.
func freeMiB() int { return meminfoMiB("MemFree") }

// shareReserveDiv dice entre cuánto se divide la reserva de una instancia
// ADICIONAL del mismo snapshot dorado. Las copias de un snapshot comparten su
// mem.file por copia-en-escritura: la primera lo mapea entero, las siguientes
// solo añaden las páginas que divergen —en la práctica una fracción pequeña—.
// Reservar mem_mib entero a cada una es correcto pero deja densidad sobre la
// mesa; dividir por 4 (por defecto) mantiene un margen holgado sobre lo que se
// mide (~1/20) sin apostar la casa. Ajustable con KLING_SHARE_RESERVE_DIV; ponlo
// a 1 para volver al comportamiento conservador de antes.
func shareReserveDiv() int {
	if v := os.Getenv("KLING_SHARE_RESERVE_DIV"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return 4
}

// reserveMemory comprueba que la microVM cabe y, si cabe, RESERVA su memoria
// hasta que termine de arrancar. Devuelve una función para liberar la reserva,
// que hay que llamar SIEMPRE —arranque bien o mal— o la reserva se queda colgada
// y bloquea a los siguientes.
//
// La reserva es lo que cierra el TOCTOU: entre leer la memoria y que el invitado
// la ocupe de verdad pasan segundos, y en ese hueco otro arranque veía la misma
// memoria libre. Contar lo que ya está en vuelo hace que el segundo vea que no
// cabe y espere su turno —o desaloje— en vez de sumarse al desbordamiento.
//
// shareKey identifica el snapshot dorado del que sale la instancia (vacío = cold
// boot o thaw, que NO comparten mem.file). Si ya hay una instancia viva de ese
// snapshot mapeando el mem.file, esta solo reserva su fracción divergente: es lo
// que convierte la densidad teórica de kindling en real bajo arranques
// concurrentes, donde antes el pendingMiB sumaba mem_mib entero por copia y
// rechazaba con 507 aunque la RAM real apenas se moviera.
func (m *Manager) reserveMemory(wantMiB int, shareKey string) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	effMiB := wantMiB
	if shareKey != "" && m.shareAnchoredLocked(shareKey) {
		effMiB = wantMiB / shareReserveDiv()
		if effMiB < 1 {
			effMiB = 1
		}
	}
	if err := checkHostMemory(effMiB+m.pendingMiB, m.hotMemFilesMiBLocked()); err != nil {
		return nil, err
	}
	m.pendingMiB += effMiB
	if shareKey != "" {
		m.snapPending[shareKey]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			m.pendingMiB -= effMiB
			if m.pendingMiB < 0 {
				m.pendingMiB = 0
			}
			if shareKey != "" {
				if m.snapPending[shareKey]--; m.snapPending[shareKey] <= 0 {
					delete(m.snapPending, shareKey)
				}
			}
			m.mu.Unlock()
		})
	}, nil
}

// shareAnchoredLocked dice si ya hay una instancia —arrancando o viva y
// corriendo— de este snapshot que tenga el mem.file mapeado. Solo cuentan las
// RUNNING: una warm está congelada, su proceso muerto y el mem.file sin mapear,
// así que la siguiente vuelve a anclar el mapeo entero. Se llama con m.mu tomado.
func (m *Manager) shareAnchoredLocked(shareKey string) bool {
	if m.snapPending[shareKey] > 0 {
		return true
	}
	for _, mc := range m.byID {
		if mc.From == shareKey && mc.State == api.StateRunning {
			return true
		}
	}
	return false
}

// hotMemFilesMiBLocked estima cuánta caché de página es atribuible a los
// mem.file de las microVMs vivas: los snapshots dorados con instancias RUNNING
// (todas mapean el MISMO fichero, se cuenta una vez), los que están arrancando
// ahora mismo (snapPending), y el mem.file propio de una máquina descongelada.
//
// Es la pieza que separa "la caché está caliente" de "TODA la caché está
// caliente". Descartar MemAvailable entero castigaba al caso normal: con una
// sola microVM viva, el grueso del buff/cache del host es caché fría de
// imágenes y ficheros, perfectamente reclamable, y el arranque se rechazaba
// con gigas genuinamente disponibles. Se llama con m.mu tomado.
//
// Es una cota superior (el fichero puede no estar entero en caché), y eso es lo
// conservador: sobrestimar lo caliente rechaza antes, nunca deja pasar de más.
func (m *Manager) hotMemFilesMiBLocked() int {
	var total int64
	snapSeen := make(map[string]bool)
	count := func(snap string) {
		if snap == "" || snapSeen[snap] {
			return
		}
		snapSeen[snap] = true
		total += allocatedBytes(filepath.Join(m.snapDir(snap), "mem.file"))
	}
	for _, mc := range m.byID {
		if mc.State != api.StateRunning {
			continue
		}
		count(mc.From)
		// Una máquina descongelada mapea su propio volcado, no el dorado.
		total += allocatedBytes(filepath.Join(m.dir(mc.ID), "mem.file"))
	}
	// Los que están arrancando aún no figuran RUNNING pero ya mapean su dorado.
	for snap := range m.snapPending {
		count(snap)
	}
	return int(total >> 20)
}

// effectiveAvailMiB es la memoria con la que de verdad se puede contar: de
// MemAvailable se descuenta SOLO la caché atribuible a los mem.file vivos
// (hotMiB), nunca más de lo que hay de caché reclamable (avail - free).
//
// MemAvailable cuenta esa caché como disponible porque es reclaimable, pero
// reclamarla es re-faultear la memoria de una microVM viva desde disco. El
// resto de la caché —imágenes, overlays, ficheros fríos— sí es dinero contante:
// el kernel la suelta al momento cuando alguien pide memoria.
func effectiveAvailMiB(avail, free, hotMiB int) int {
	if avail <= 0 {
		return avail
	}
	reclaim := avail - free
	if reclaim < 0 {
		reclaim = 0
	}
	if hotMiB > reclaim {
		hotMiB = reclaim
	}
	return avail - hotMiB
}

// checkHostMemory se niega a arrancar una microVM que no cabe.
//
// Es un aviso caro de ignorar y barato de dar: leer /proc/meminfo cuesta
// microsegundos, y el fallo que evita cuesta una tarde de diagnóstico.
//
// No comprueba nada si no puede leer la memoria: convertir una comprobación de
// cordura en una dependencia de arranque sería peor que el problema.
//
// hotMiB es la caché atribuible a los mem.file de las microVMs vivas (ver
// hotMemFilesMiBLocked); 0 significa "ninguna" y deja mandar a MemAvailable.
func checkHostMemory(wantMiB, hotMiB int) error {
	avail := availableMiB()
	if avail <= 0 || wantMiB <= 0 {
		return nil
	}
	eff := effectiveAvailMiB(avail, freeMiB(), hotMiB)
	// Suelo de memoria realmente utilizable. Atrapa el caso que el check de
	// tamaño no ve: muchos snapshots DISTINTOS vivos, cada uno con su mem.file
	// caliente en caché. Ahí MemAvailable sigue alto mientras lo utilizable se
	// agota, y el arranque siguiente fallaba al no poder ni crear sus ficheros.
	//
	// El suelo se compara contra eff y no contra MemFree a secas: la caché que
	// NO es de mem.files vivos se reclama al instante y cuenta como libre.
	// Compararlo contra MemFree rechazaba arranques con gigas genuinamente
	// reclamables — comprobado con una sola microVM viva y 3,4 GB de caché fría.
	if floor := minFreeMiB(); floor > 0 && eff < floor {
		return &api.StatusError{Code: api.StatusInsufficientMemory, Message: fmt.Sprintf(
			"low on really free memory: %d MiB usable (floor %d).\n"+
				"MemAvailable says %d MiB, but %d MiB of it is hot cache from the mem.files of the "+
				"live microVMs: reclaiming it means re-faulting their memory from disk.\n"+
				"Freeze or remove an instance (`kling ps -a`), or lower the floor with KLING_MIN_FREE_MIB",
			eff, floor, avail, avail-eff)}
	}
	if wantMiB+hostReserveMiB <= eff {
		return nil
	}
	return &api.StatusError{Code: api.StatusInsufficientMemory, Message: fmt.Sprintf(
		"doesn't fit: the microVM asks for %d MiB and the host only has %d MiB left "+
			"(%d is reserved for the host itself).\n"+
			"Starting it anyway would make the host's OOM killer kill processes at random, "+
			"including other microVMs.\nSee: `kling ps -a`, or give it less memory with -mem",
		wantMiB, eff, hostReserveMiB)}
}
