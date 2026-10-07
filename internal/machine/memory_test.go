package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// No arrancar lo que no cabe. Una microVM que desborda el anfitrión no falla al
// arrancar: arranca, y después el OOM killer del host mata procesos al azar
// —incluidas otras microVMs y el propio daemon—, con el motivo en un journal
// que nadie mira.
func TestNoSeArrancaLoQueNoCabe(t *testing.T) {
	avail := availableMiB()
	if avail <= 0 {
		t.Skip("no puedo leer la memoria de este host (macOS): la comprobación se salta sola")
	}

	// Lo que cabe de sobra, pasa.
	if err := checkHostMemory(16, 0); err != nil {
		t.Errorf("rechazó 16 MiB habiendo %d disponibles: %v", avail, err)
	}
	// Lo que no cabe ni de lejos, no.
	err := checkHostMemory(avail+hostReserveMiB+1024, 0)
	if err == nil {
		t.Fatal("aceptó una microVM más grande que el anfitrión")
	}
	// Y el error tiene que decir las dos cifras y qué hacer, o no sirve de nada.
	for _, quiero := range []string{"doesn't fit", "reserved", "kling ps -a"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(quiero)) {
			t.Errorf("el error no menciona %q: %v", quiero, err)
		}
	}
}

// Sin poder leer la memoria NO se bloquea nada: convertir una comprobación de
// cordura en una dependencia de arranque sería peor que el problema que evita.
func TestSinDatosDeMemoriaNoSeBloquea(t *testing.T) {
	if err := checkHostMemory(0, 0); err != nil {
		t.Errorf("bloqueó una petición sin memoria declarada: %v", err)
	}
}

// reserveMemory cierra el TOCTOU: dos arranques concurrentes no pueden ver los
// dos la misma memoria libre y pasar.
//
// Sin la reserva, entre leer MemAvailable y que el invitado ocupe su RAM pasan
// segundos, y en ese hueco un segundo arranque veía la misma memoria y se sumaba
// al desbordamiento — el OOM del anfitrión que el check existe para impedir.
func TestLaReservaDeMemoriaNoSeCuentaDosVeces(t *testing.T) {
	m := newTestManager(t)
	avail := availableMiB()
	if avail <= 0 {
		t.Skip("sin /proc/meminfo (macOS): la reserva se prueba en Linux")
	}

	// Reservar casi toda la memoria disponible: la primera pasa.
	grande := avail - hostReserveMiB - 64
	if grande <= 0 {
		t.Skip("el host de test no tiene memoria de sobra para el caso")
	}
	release, err := m.reserveMemory(grande, "")
	if err != nil {
		t.Fatalf("la primera reserva debería caber: %v", err)
	}

	// Con esa reserva viva, una segunda del mismo tamaño ya NO cabe, aunque el
	// MemAvailable crudo diga que sí: es justo lo que el TOCTOU no veía.
	if _, err := m.reserveMemory(grande, ""); err == nil {
		t.Error("la segunda reserva pasó: el TOCTOU sigue abierto")
	} else if !api.IsInsufficientMemory(err) {
		t.Errorf("el error no es de memoria insuficiente: %v", err)
	}

	// Liberada la primera, la segunda vuelve a caber.
	release()
	release2, err := m.reserveMemory(grande, "")
	if err != nil {
		t.Errorf("tras liberar debería caber otra vez: %v", err)
	} else {
		release2()
	}

	// Y liberar dos veces no deja el contador en negativo (bloquearía a todos).
	release()
	m.mu.RLock()
	pend := m.pendingMiB
	m.mu.RUnlock()
	if pend < 0 {
		t.Errorf("pendingMiB quedó negativo: %d", pend)
	}
}

// Las copias de un mismo snapshot dorado comparten el mem.file, así que la
// segunda y siguientes solo reservan su fracción divergente. Es lo que convierte
// la densidad en real bajo arranques concurrentes.
func TestReservaCompartidaCobraFraccionALasCopias(t *testing.T) {
	m := newTestManager(t)
	avail := availableMiB()
	const want = 256
	frac := want / shareReserveDiv()
	if avail <= 0 || avail < want+frac+hostReserveMiB {
		t.Skip("sin margen de memoria para el caso (o macOS sin /proc/meminfo)")
	}

	// Primera instancia del snapshot: ancla el mem.file, paga entero.
	rel1, err := m.reserveMemory(want, "ctx")
	if err != nil {
		t.Fatalf("la primera debía caber: %v", err)
	}
	if p := pend(m); p != want {
		t.Fatalf("la primera debía reservar %d entero, reservó %d", want, p)
	}

	// Con una ya arrancando, la copia solo añade su fracción.
	rel2, err := m.reserveMemory(want, "ctx")
	if err != nil {
		t.Fatalf("la copia debía caber (paga fracción): %v", err)
	}
	if p := pend(m); p != want+frac {
		t.Fatalf("la copia debía añadir %d (want/%d); total esperado %d, hubo %d",
			frac, shareReserveDiv(), want+frac, p)
	}
	rel1()
	rel2()
	if p := pend(m); p != 0 {
		t.Fatalf("tras liberar todo, pendingMiB debía ser 0, es %d", p)
	}

	// Una instancia RUNNING del snapshot ancla igual, aunque no haya pendientes.
	m.mu.Lock()
	m.byID["x"] = &api.Machine{ID: "x", From: "svc", State: api.StateRunning}
	m.mu.Unlock()
	rel3, err := m.reserveMemory(want, "svc")
	if err != nil {
		t.Fatalf("con una running anclando, la copia debía caber: %v", err)
	}
	if p := pend(m); p != frac {
		t.Fatalf("con el mem.file anclado por una running, debía cobrar %d; reservó %d", frac, p)
	}
	rel3()

	// Un cold boot (sin shareKey) paga entero aunque el snapshot esté anclado.
	rel4, err := m.reserveMemory(want, "")
	if err != nil {
		t.Fatalf("cold boot debía caber: %v", err)
	}
	if p := pend(m); p != want {
		t.Fatalf("un cold boot no comparte: debía reservar %d entero, reservó %d", want, p)
	}
	rel4()
}

func pend(m *Manager) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pendingMiB
}

// El suelo de MemFree atrapa el caso que MemAvailable no ve (muchos snapshots
// distintos vivos): con un suelo altísimo, checkHostMemory rechaza con 507 limpio
// aunque MemAvailable diga que cabe.
func TestSueloDeMemoriaLibre(t *testing.T) {
	if freeMiB() <= 0 {
		t.Skip("sin /proc/meminfo (macOS): el suelo se prueba en Linux")
	}
	t.Setenv("KLING_MIN_FREE_MIB", "99999999")
	err := checkHostMemory(16, 0)
	if err == nil {
		t.Fatal("con suelo altísimo debía rechazar por el suelo de memoria utilizable")
	}
	if !api.IsInsufficientMemory(err) {
		t.Errorf("el rechazo no es 507: %v", err)
	}
	for _, q := range []string{"really free", "floor", "kling_min_free_mib"} {
		if !strings.Contains(strings.ToLower(err.Error()), q) {
			t.Errorf("el mensaje del suelo no menciona %q: %v", q, err)
		}
	}
	// Suelo 0 lo desactiva: 16 MiB vuelven a pasar.
	t.Setenv("KLING_MIN_FREE_MIB", "0")
	if err := checkHostMemory(16, 0); err != nil {
		t.Errorf("con el suelo desactivado, 16 MiB deberían caber: %v", err)
	}
}

// La caché caliente se descuenta SOLO en lo atribuible a los mem.file vivos.
//
// Es el caso observado en producción: MemFree 108, MemAvailable 3520, UNA sola
// microVM viva — y el guardián rechazaba el arranque dando por caliente una
// caché que en su mayoría era fría y reclamable (3,4 GB al soltarla). El
// descuento tiene que salir de lo atribuible, no de descartar MemAvailable.
func TestLaCacheSoloSeDescuentaEnLoAtribuible(t *testing.T) {
	// Sin mem.files vivos, MemAvailable manda entero: el caso del host con una
	// caché grande y fría.
	if got := effectiveAvailMiB(3520, 108, 0); got != 3520 {
		t.Errorf("sin caché caliente, effective = %d, want 3520", got)
	}
	// Con más caché atribuible que caché total, el descuento se acota a la
	// caché reclamable: queda exactamente MemFree, nunca menos.
	if got := effectiveAvailMiB(3520, 108, 99999); got != 108 {
		t.Errorf("con todo caliente, effective = %d, want 108 (MemFree)", got)
	}
	// Atribución parcial: se descuenta lo caliente y lo frío sigue contando.
	if got := effectiveAvailMiB(4000, 1000, 1200); got != 2800 {
		t.Errorf("con 1200 calientes, effective = %d, want 2800", got)
	}
	// Sin dato de memoria no se inventa nada.
	if got := effectiveAvailMiB(0, 0, 100); got != 0 {
		t.Errorf("sin MemAvailable, effective = %d, want 0", got)
	}
}

// Y checkHostMemory usa ese descuento: con la caché entera marcada como
// caliente, lo utilizable cae a MemFree y una petición grande se rechaza,
// aunque el MemAvailable crudo diga que cabe.
func TestElGuardianDescuentaLaCacheCaliente(t *testing.T) {
	avail, free := availableMiB(), freeMiB()
	if avail <= 0 || free <= 0 {
		t.Skip("sin /proc/meminfo (macOS): el guardián se prueba en Linux")
	}
	t.Setenv("KLING_MIN_FREE_MIB", "0") // aislar el check de tamaño del suelo
	want := free + hostReserveMiB + 64  // no cabe en MemFree, sí en MemAvailable crudo
	if want+hostReserveMiB > avail {
		t.Skip("el host de test no tiene hueco entre MemFree y MemAvailable para el caso")
	}
	if err := checkHostMemory(want, avail); err == nil {
		t.Error("con toda la caché caliente debía rechazar: solo queda MemFree de verdad")
	} else if !api.IsInsufficientMemory(err) {
		t.Errorf("el rechazo no es 507: %v", err)
	}
	// La misma petición sin caché caliente cabe: es la diferencia que corrige
	// el falso rechazo observado.
	if err := checkHostMemory(want, 0); err != nil {
		t.Errorf("sin caché caliente debía caber (%d de %d disponibles): %v", want, avail, err)
	}
}

// `hotMemFilesMiBLocked` no tenia ninguna cobertura: se podia hacer que devolviera
// 0 —anulando el arreglo entero— y toda la suite seguia verde, en Linux tambien.
// El test que decia cubrirlo (`TestElGuardianDescuentaLaCacheCaliente`) mide el
// guardian de punta a punta contra el /proc REAL del host, asi que su resultado
// depende de cuanta caché tenga la maquina en ese momento y no llega a distinguir
// si el descuento se calculo o no.
//
// Este prueba la funcion directamente, con ficheros de verdad en disco, porque lo
// que decide es cuantos bytes ASIGNADOS tienen los mem.file: son dispersos, y ahi
// esta la gracia — 256 MB nominales pueden ser 19 MB reales.
func TestSoloCuentaLosMemFileDeMaquinasVivas(t *testing.T) {
	m := newTestManager(t)

	// Un mem.file de 8 MiB reales dentro del snapshot dorado "dorado".
	crear := func(dir string, mib int) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, "mem.file"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.Write(make([]byte, mib<<20)); err != nil {
			t.Fatal(err)
		}
	}
	crear(m.snapDir("dorado"), 8)

	// Sin maquinas: no hay nada caliente.
	if got := m.hotMemFilesMiBLocked(); got != 0 {
		t.Errorf("sin maquinas vivas deberia ser 0, fue %d MiB", got)
	}

	// Una PARADA que use ese dorado tampoco cuenta: no lo mapea nadie.
	m.byID["aaaa"] = &api.Machine{ID: "aaaa", From: "dorado", State: api.StateStopped}
	if got := m.hotMemFilesMiBLocked(); got != 0 {
		t.Errorf("una maquina parada no mapea su dorado; esperaba 0, fue %d MiB", got)
	}

	// Corriendo si: cuenta los 8 MiB del dorado.
	m.byID["aaaa"].State = api.StateRunning
	got := m.hotMemFilesMiBLocked()
	if got < 7 || got > 9 {
		t.Errorf("una viva sobre el dorado deberia contar ~8 MiB, fue %d", got)
	}

	// Una SEGUNDA sobre el MISMO dorado no lo cuenta dos veces: se comparte.
	m.byID["bbbb"] = &api.Machine{ID: "bbbb", From: "dorado", State: api.StateRunning}
	if dos := m.hotMemFilesMiBLocked(); dos != got {
		t.Errorf("el dorado compartido se conto dos veces: %d -> %d MiB", got, dos)
	}
}

// allocatedBytesCachedLocked (M-12): un acierto no hace NINGÚN stat (si lo
// hiciera, la caché costaría lo mismo que no tenerla); solo la invalidación
// explícita obliga a volver a mirar el disco.
func TestAllocatedBytesCachedLockedNoHaceStatEnUnAcierto(t *testing.T) {
	m := newTestManager(t)
	path := filepath.Join(t.TempDir(), "mem.file")
	if err := os.WriteFile(path, make([]byte, 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	viejo := statMemFile
	t.Cleanup(func() { statMemFile = viejo })
	var stats int
	statMemFile = func(p string) (os.FileInfo, error) { stats++; return os.Stat(p) }

	m.mu.Lock()
	defer m.mu.Unlock()
	primero := m.allocatedBytesCachedLocked(path, "dorado")
	if primero <= 0 || stats != 1 {
		t.Fatalf("fallo de caché: alloc=%d stats=%d, quería >0 y 1", primero, stats)
	}
	for i := 0; i < 3; i++ {
		if got := m.allocatedBytesCachedLocked(path, "dorado"); got != primero {
			t.Fatalf("acierto = %d, quería %d", got, primero)
		}
	}
	if stats != 1 {
		t.Fatalf("los aciertos hicieron stat: %d stats, quería 1", stats)
	}

	// Un fichero que no existe no se cachea: la próxima vez se vuelve a mirar.
	falta := filepath.Join(t.TempDir(), "no-esta")
	if got := m.allocatedBytesCachedLocked(falta, "fantasma"); got != 0 {
		t.Fatalf("sin fichero = %d, quería 0", got)
	}
	if _, hay := m.memAllocCache["fantasma"]; hay {
		t.Fatal("un stat fallido quedó cacheado")
	}
}

// invalidateSnapCache también olvida el tamaño cacheado del mem.file: un
// `commit -replace` que reescribe el dorado no puede heredar el tamaño del que
// reemplazó.
func TestInvalidateSnapCacheOlvidaElTamanoDeMemoria(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.memAllocCache = map[string]int64{"dorado": 99}
	m.mu.Unlock()

	m.invalidateSnapCache("dorado")

	m.mu.RLock()
	_, hay := m.memAllocCache["dorado"]
	m.mu.RUnlock()
	if hay {
		t.Error("invalidateSnapCache no borró la entrada de memAllocCache")
	}
}

// El barrido de restos de commit, que aparta el directorio en su sitio con
// m.mu tomado y sin pasar por removeSnapshot, también olvida lo cacheado.
func TestSweepSnapshotLeftoversOlvidaLaCache(t *testing.T) {
	m := newTestManager(t)
	dir := m.snapDir("amedias")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	viejo := time.Now().Add(-2 * dirGrace)
	if err := os.Chtimes(dir, viejo, viejo); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.memAllocCache = map[string]int64{"amedias": 99}
	m.sweepSnapshotLeftovers()
	_, hay := m.memAllocCache["amedias"]
	m.mu.Unlock()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("el barrido no apartó los restos: %v", err)
	}
	if hay {
		t.Error("sweepSnapshotLeftovers no olvidó el tamaño cacheado")
	}
}

// Una copia de kling db branch del golden de Postgres (~211 MiB asignados)
// tiene que quedarse en caché al congelarla: es lo que hace que volver a una
// rama cueste ~13 ms de despertar y no ~100 (ver cacheTibiaMaxBytes).
func TestCacheTibiaCubreUnaCopiaDePostgres(t *testing.T) {
	const copiaPG = 211 << 20
	if copiaPG > cacheTibiaMaxBytes {
		t.Fatalf("cacheTibiaMaxBytes = %d MiB: una copia de Postgres de %d MiB se soltaría de la caché al congelarla",
			cacheTibiaMaxBytes>>20, copiaPG>>20)
	}
	if cacheTibiaMaxBytes > precargaMaxBytes {
		t.Fatal("lo que se deja en caché al congelar no puede pasar de lo que el thaw precargaría")
	}
}

// Una máquina que salió de un dorado y después se paró y se arrancó con
// kling start (BootMS > 0) arranca en frío desde su disco: ya no mapea el
// mem.file del dorado aunque conserve From. Ni ancla el dorado —el siguiente
// run -from tiene que reservar entero— ni cuenta como caché caliente.
func TestUnaArrancadaConStartNoMapeaSuDorado(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(m.snapDir("dorado"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.snapDir("dorado"), "mem.file"), make([]byte, 8<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.byID["arrancada"] = &api.Machine{ID: "arrancada", From: "dorado", State: api.StateRunning, BootMS: 24}
	anclado, caliente := m.shareAnchoredLocked("dorado"), m.hotMemFilesMiBLocked()
	m.mu.Unlock()
	if anclado {
		t.Error("una arrancada en frío da su dorado por anclado")
	}
	if caliente != 0 {
		t.Errorf("una arrancada en frío cuenta %d MiB de su dorado como caché caliente", caliente)
	}

	// La copia de verdad (run -from, sin BootMS) sí.
	m.mu.Lock()
	m.byID["copia"] = &api.Machine{ID: "copia", From: "dorado", State: api.StateRunning}
	anclado, caliente = m.shareAnchoredLocked("dorado"), m.hotMemFilesMiBLocked()
	m.mu.Unlock()
	if !anclado || caliente < 7 {
		t.Errorf("con una copia viva: anclado %v, caliente %d MiB; quería true y ~8", anclado, caliente)
	}
}
