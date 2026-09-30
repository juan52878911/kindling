package machine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Pruebas del nodo N2 del plan de remediación: Commit bajo el cerrojo de ciclo
// de vida (M-04), su limpieza única con un ctx que no se cancela (M-05), las
// reservas de snapshot con contador que cierran el TOCTOU de RemoveSnapshot
// contra runFrom (M-15) y el plazo del volcado proporcional a la memoria
// (F-01). Todas sobre el arnés de N0: un fc.Client de verdad contra fcFalso,
// sin KVM ni root.

// plantillaParaCommit deja una máquina running con un VMM falso detrás y su
// overlay propio en disco, que es lo que Commit copia con cp antes de volcar.
func plantillaParaCommit(t *testing.T, m *Manager, id string) (*fcFalso, <-chan struct{}) {
	t.Helper()
	// m viene de newTestManager, que arma el Manager a mano sin pasar por
	// NewManager: m.jailerJailed se queda en su cero (false), así que esta
	// plantilla nunca corre "jailed" sin que haga falta tocar KLING_JAILER.
	if _, err := exec.LookPath("cp"); err != nil {
		t.Skip("sin cp no se puede copiar el overlay")
	}
	m.bus = events.New()
	m.priv = &Privileges{} // sin bajar privilegios: Own no hace nada
	falso, muerto := maquinaCorriendo(t, m, id)
	if err := os.WriteFile(filepath.Join(m.dir(id), "overlay.ext4"), make([]byte, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	// Commit graba kernel_sha256 (K2): en producción Run ya exige que el
	// vmlinux exista antes de arrancar, pero maquinaCorriendo publica la
	// máquina directamente sin pasar por Run, así que aquí hay que ponerlo.
	if err := os.MkdirAll(filepath.Join(m.root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.KernelPath(), []byte("vmlinux de prueba"), 0o644); err != nil {
		t.Fatal(err)
	}
	return falso, muerto
}

// indiceDe devuelve la posición de la primera llamada a metodo+ruta cuyo cuerpo
// contiene trozo (o -1), a partir de desde.
func indiceDe(ll []llamadaFC, desde int, metodo, ruta, trozo string) int {
	for i := desde; i < len(ll); i++ {
		if ll[i].Metodo == metodo && ll[i].Ruta == ruta && strings.Contains(string(ll[i].Cuerpo), trozo) {
			return i
		}
	}
	return -1
}

// comprobarReanudada exige que, tras el volcado, se devolviera el overlay
// propio y después se reanudara la plantilla, y que siga viva y running.
func comprobarReanudada(t *testing.T, m *Manager, falso *fcFalso, id string, muerto <-chan struct{}) {
	t.Helper()
	ll := falso.todas()
	volcado := indiceDe(ll, 0, http.MethodPut, "/snapshot/create", "")
	if volcado < 0 {
		t.Fatalf("no llegó a pedirse el volcado: %+v", ll)
	}
	propio := filepath.Join(m.dir(id), "overlay.ext4")
	devuelto := indiceDe(ll, volcado, http.MethodPatch, "/drives/overlay", propio)
	if devuelto < 0 {
		t.Fatalf("tras el volcado no se le devolvió el overlay propio: %+v", ll)
	}
	reanudada := indiceDe(ll, devuelto, http.MethodPatch, "/vm", "Resumed")
	if reanudada < 0 {
		t.Fatalf("la plantilla no se reanudó después de devolverle el disco: %+v", ll)
	}
	if viva := vivaDe(t, m, id); viva.State != api.StateRunning {
		t.Errorf("la plantilla quedó %s, quería running", viva.State)
	}
	if !sigueVivo(muerto) {
		t.Error("el VMM de la plantilla murió: un volcado fallido no es motivo para matarla")
	}
}

// M-05: el volcado falla. La plantilla vuelve a su disco, se reanuda, y el
// directorio a medias desaparece (sin meta.json sería "restos de commit").
func TestCommitReanudaSiFallaElVolcado(t *testing.T) {
	m := newTestManager(t)
	id := "c0aa170000000001"
	falso, muerto := plantillaParaCommit(t, m, id)
	falso.fallar(http.MethodPut, "/snapshot/create", http.StatusBadRequest, "No space left on device")

	_, err := m.Commit(context.Background(), id, "dorado", false)
	if err == nil || !strings.Contains(err.Error(), "No space left") {
		t.Fatalf("Commit con el volcado roto: %v, quería el error del volcado", err)
	}
	comprobarReanudada(t, m, falso, id, muerto)
	if _, err := os.Stat(m.snapDir("dorado")); !os.IsNotExist(err) {
		t.Errorf("el directorio del snapshot a medias sigue ahí (err=%v)", err)
	}
	if n := len(falso.llamadasA(http.MethodPatch, "/vm")); n != 2 {
		t.Errorf("PATCH /vm = %d llamadas, quería exactamente 2 (pausa y reanudación)", n)
	}
}

// M-05, el caso que motivó WithoutCancel: quien pidió el commit se va a mitad
// del volcado. Antes la limpieza usaba ese mismo ctx y la reanudación ni se
// intentaba: plantilla pausada figurando como running.
func TestCommitConCtxCanceladoReanudaIgual(t *testing.T) {
	m := newTestManager(t)
	id := "c0aa170000000002"
	falso, muerto := plantillaParaCommit(t, m, id)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			cancel()
			// Que el cliente vea el ctx cancelado antes que la respuesta.
			time.Sleep(50 * time.Millisecond)
		}
	})

	if _, err := m.Commit(ctx, id, "dorado", false); err == nil {
		t.Fatal("Commit con el ctx cancelado a mitad no devolvió error")
	}
	comprobarReanudada(t, m, falso, id, muerto)
	if _, err := os.Stat(m.snapDir("dorado")); !os.IsNotExist(err) {
		t.Errorf("el directorio del snapshot a medias sigue ahí (err=%v)", err)
	}
}

// M-05: si no se puede devolver el disco propio, reanudar sería dejarla
// escribiendo en el overlay dorado. Se marca fallida (y dicho), no se reanuda.
func TestCommitSinPoderDevolverElDiscoMarcaFallida(t *testing.T) {
	m := newTestManager(t)
	id := "c0aa170000000003"
	falso, _ := plantillaParaCommit(t, m, id)
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			falso.fallar(http.MethodPatch, "/drives/overlay", http.StatusBadRequest, "drive busy")
		}
	})
	falso.fallar(http.MethodPut, "/snapshot/create", http.StatusBadRequest, "boom")

	if _, err := m.Commit(context.Background(), id, "dorado", false); err == nil {
		t.Fatal("Commit sin poder devolver el disco no devolvió error")
	}
	ll := falso.todas()
	volcado := indiceDe(ll, 0, http.MethodPut, "/snapshot/create", "")
	if i := indiceDe(ll, volcado, http.MethodPatch, "/vm", "Resumed"); i >= 0 {
		t.Errorf("se reanudó una plantilla que seguía apuntando al overlay dorado: %+v", ll)
	}
	viva := vivaDe(t, m, id)
	if viva.State != api.StateFailed || !strings.Contains(viva.LastErr, "disk back") {
		t.Errorf("plantilla %s / %q, quería failed diciendo por qué", viva.State, viva.LastErr)
	}
}

// guestFalso es el agente del invitado lo justo para los volúmenes: contesta
// a /healthz y apunta cada /volume/<op> que le llega.
func guestFalso(t *testing.T) (addr string, ops func() []string) {
	t.Helper()
	var mu sync.Mutex
	var vistos []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if op, ok := strings.CutPrefix(r.URL.Path, "/volume/"); ok {
			mu.Lock()
			vistos = append(vistos, op)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), vistos...)
	}
}

// M-05: los volúmenes se sueltan antes de pausar. Si la pausa falla, la
// plantilla sigue corriendo y hay que devolvérselos; antes salía sin más.
func TestCommitDevuelveLosVolumenesSiFallaLaPausa(t *testing.T) {
	m := newTestManager(t)
	id := "c0aa170000000004"
	falso, muerto := plantillaParaCommit(t, m, id)
	addr, ops := guestFalso(t)
	m.mu.Lock()
	viva := m.byID[id]
	viva.Volumes = []api.VolumeAttachment{{Name: "datos", Mount: "/data"}}
	viva.Forwards = map[string]string{"8080": addr}
	m.mu.Unlock()

	falso.fallar(http.MethodPatch, "/vm", http.StatusBadRequest, "vcpu busy")

	if _, err := m.Commit(context.Background(), id, "dorado", false); err == nil {
		t.Fatal("Commit con la pausa rota no devolvió error")
	}
	got := ops()
	if len(got) != 2 || got[0] != "release" || got[1] != "acquire" {
		t.Fatalf("operaciones de volumen = %v, quería [release acquire]", got)
	}
	if len(falso.llamadasA(http.MethodPut, "/snapshot/create")) != 0 {
		t.Error("con la pausa fallida se pidió igualmente el volcado")
	}
	if v := vivaDe(t, m, id); v.State != api.StateRunning || !sigueVivo(muerto) {
		t.Errorf("una pausa fallida dejó la plantilla %s (vivo=%v)", v.State, sigueVivo(muerto))
	}
	if _, err := os.Stat(m.snapDir("dorado")); !os.IsNotExist(err) {
		t.Errorf("el directorio del snapshot a medias sigue ahí (err=%v)", err)
	}
}

// M-04: Commit espera al cerrojo de ciclo de vida de la plantilla y, al
// conseguirlo, vuelve a leerla. Aquí, mientras esperaba, "otro" la congeló:
// no puede pausar ni volcar una máquina que ya no está corriendo.
func TestCommitEsperaAlCerrojoYReleeLaMaquina(t *testing.T) {
	m := newTestManager(t)
	id := "c0aa170000000005"
	falso, _ := plantillaParaCommit(t, m, id)

	soltar := m.lock(id)
	hecho := make(chan error, 1)
	go func() {
		_, err := m.Commit(context.Background(), id, "dorado", false)
		hecho <- err
	}()

	select {
	case err := <-hecho:
		soltar()
		t.Fatalf("Commit no esperó al cerrojo de la plantilla: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if ll := falso.todas(); len(ll) != 0 {
		soltar()
		t.Fatalf("Commit habló con el VMM sin tener el cerrojo: %+v", ll)
	}

	m.mu.Lock()
	m.byID[id].State = api.StateWarm
	m.mu.Unlock()
	soltar()

	select {
	case err := <-hecho:
		if err == nil || !strings.Contains(err.Error(), "only a running machine") {
			t.Fatalf("Commit sobre una máquina que se congeló mientras esperaba: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Commit no terminó tras soltar el cerrojo")
	}
	if ll := falso.todas(); len(ll) != 0 {
		t.Errorf("Commit habló con el VMM de una máquina congelada: %+v", ll)
	}
}

// El camino feliz, para que la limpieza nueva no lo haya roto: el volcado se
// hace, la plantilla recupera su disco y se reanuda UNA vez, y queda un
// snapshot con meta.json que se puede listar.
func TestCommitCompletoDejaSnapshotYPlantillaEnMarcha(t *testing.T) {
	if _, err := exec.LookPath("fallocate"); err != nil {
		t.Skip("sin fallocate no se puede perforar el volcado")
	}
	m := newTestManager(t)
	id := "c0aa170000000006"
	falso, muerto := plantillaParaCommit(t, m, id)
	dir := m.snapDir("dorado")
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			// Lo que escribiría Firecracker.
			_ = os.WriteFile(filepath.Join(dir, "snap.file"), []byte("estado"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "mem.file"), make([]byte, 1<<20), 0o644)
		}
	})

	snap, err := m.Commit(context.Background(), id, "dorado", false)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if snap.RootfsSHA256 == "" || snap.SnapSHA256 == "" {
		t.Errorf("snapshot sin digests: %+v", snap)
	}
	comprobarReanudada(t, m, falso, id, muerto)
	if n := len(falso.llamadasA(http.MethodPatch, "/vm")); n != 2 {
		t.Errorf("PATCH /vm = %d llamadas, quería 2: la limpieza repitió la reanudación", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		t.Errorf("no quedó meta.json: %v", err)
	}
	if got, err := m.loadSnapshot("dorado"); err != nil || got.Name != "dorado" {
		t.Errorf("loadSnapshot tras el commit: %+v, %v", got, err)
	}
	// Los digests se acaban de calcular sobre estos ficheros: la primera
	// restauración no los vuelve a leer.
	if !m.integridadYaVista("dorado", dir) {
		t.Error("tras el commit, el veredicto de integridad no quedó anotado: la primera restauración volvería a hashear")
	}
}

// M-15: las reservas se cuentan. Con un bool, la primera en soltar dejaba sin
// protección a las demás; y soltar dos veces la misma no puede quitar la de
// otro.
func TestReserveDirCuentaLasReservas(t *testing.T) {
	m := newTestManager(t)
	clave := reservaSnapshot("dorado")
	reservas := func() int {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.reserved[clave]
	}

	a := m.reserveDir(clave)
	b := m.reserveDir(clave)
	if n := reservas(); n != 2 {
		t.Fatalf("dos reservas cuentan %d", n)
	}
	a()
	a() // idempotente: no se come la de b
	if n := reservas(); n != 1 {
		t.Fatalf("tras soltar una (dos veces) quedan %d, quería 1", n)
	}
	b()
	m.mu.RLock()
	_, queda := m.reserved[clave]
	m.mu.RUnlock()
	if queda {
		t.Error("soltadas todas, la entrada sigue en el mapa")
	}
}

// escribirSnapshot deja en disco un snapshot con su meta.json (sin firmar: se
// aceptan salvo con KLING_REQUIRE_SIGNED=1).
func escribirSnapshot(t *testing.T, m *Manager, name string, s api.Snapshot) string {
	t.Helper()
	dir := m.snapDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s.Name = name
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// M-15: el commit que reemplaza (propio=true) descuenta SU reserva, no las
// demás. Si además hay una restauración leyendo el snapshot, se niega.
func TestRemoveSnapshotPropioRespetaLasReservasAjenas(t *testing.T) {
	m := newTestManager(t)
	dir := escribirSnapshot(t, m, "dorado", api.Snapshot{})

	delCommit := m.reserveDir(reservaSnapshot("dorado"))
	defer delCommit()
	deRunFrom := m.reserveDir(reservaSnapshot("dorado"))

	if err := m.removeSnapshot("dorado", true); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("-replace con una restauración en curso: %v, quería 'in use'", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("el snapshot desapareció pese a negarse: %v", err)
	}
	deRunFrom()
	if err := m.removeSnapshot("dorado", true); err != nil {
		t.Fatalf("-replace sin nadie más: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("el snapshot sigue ahí tras borrarlo (err=%v)", err)
	}
}

// M-15, contra un runFrom de verdad: mientras restaura (aquí, detenido
// verificando la integridad del overlay dorado, que es un FIFO que nadie
// escribe todavía), RemoveSnapshot se niega; en cuanto termina, lo borra.
func TestRemoveSnapshotSeNiegaDuranteRunFrom(t *testing.T) {
	t.Setenv("KLING_REQUIRE_SIGNED", "")
	m := newTestManager(t)
	dir := escribirSnapshot(t, m, "dorado", api.Snapshot{
		RootfsSHA256: strings.Repeat("a", 64),
		SnapSHA256:   strings.Repeat("b", 64),
	})
	fifo := filepath.Join(dir, "overlay.ext4")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("no se puede crear un FIFO aquí: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snap.file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	hecho := make(chan error, 1)
	go func() {
		_, err := m.runFrom(context.Background(), api.RunRequest{From: "dorado"})
		hecho <- err
	}()

	// La reserva se toma antes de leer nada: en cuanto aparece, la ventana
	// está abierta (y el FIFO la mantiene abierta hasta que lo escribamos).
	limite := time.Now().Add(5 * time.Second)
	for {
		m.mu.RLock()
		n := m.reserved[reservaSnapshot("dorado")]
		m.mu.RUnlock()
		if n > 0 {
			break
		}
		if time.Now().After(limite) {
			t.Fatal("runFrom no reservó el snapshot")
		}
		time.Sleep(5 * time.Millisecond)
	}

	err := m.RemoveSnapshot("dorado")
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("RemoveSnapshot durante runFrom: %v, quería 'in use, retry'", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		t.Errorf("el snapshot se borró bajo un runFrom en curso: %v", err)
	}

	// Desbloquear runFrom: el FIFO vacío no cuadra con el digest y la
	// restauración falla por integridad, sin llegar a arrancar nada.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	select {
	case err := <-hecho:
		if err == nil || !strings.Contains(err.Error(), "corrupt") {
			t.Fatalf("runFrom sobre un dorado que no cuadra: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runFrom no terminó")
	}

	m.mu.RLock()
	_, queda := m.reserved[reservaSnapshot("dorado")]
	m.mu.RUnlock()
	if queda {
		t.Error("runFrom terminó sin soltar su reserva del snapshot")
	}
	if err := m.RemoveSnapshot("dorado"); err != nil {
		t.Fatalf("RemoveSnapshot tras el runFrom: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("el snapshot sigue ahí (err=%v)", err)
	}
}

// F-01: el plazo del volcado crece con la memoria y nunca baja de 30 s.
func TestPlazoVolcadoCreceConLaMemoria(t *testing.T) {
	for _, c := range []struct {
		mib  int
		want time.Duration
	}{
		{0, 30 * time.Second},
		{512, 30 * time.Second},
		{1024, 30 * time.Second},
		{2048, 50 * time.Second},
		{4096, 90 * time.Second},
		{16384, 330 * time.Second},
	} {
		if got := plazoVolcado(c.mib); got != c.want {
			t.Errorf("plazoVolcado(%d MiB) = %v, quería %v", c.mib, got, c.want)
		}
	}
	for mib := 0; mib < 64<<10; mib += 256 {
		if plazoVolcado(mib+256) < plazoVolcado(mib) {
			t.Fatalf("el plazo baja al pasar de %d a %d MiB", mib, mib+256)
		}
	}
}

// Un secreto inyectado por MMDS mientras Commit espera el cerrojo (el gateway
// abriendo sesión justo cuando alguien hace `kling save`, o un fork) no puede
// acabar en el mem.file del dorado: Commit relee la máquina con el cerrojo y
// se niega sin tocar el VMM.
func TestCommitSeNiegaSiLeInyectaronSecretosMientrasEsperaba(t *testing.T) {
	m := newTestManager(t)
	id := "c0aa170000000006"
	falso, _ := plantillaParaCommit(t, m, id)

	soltar := m.lock(id)
	hecho := make(chan error, 1)
	go func() {
		_, err := m.Commit(context.Background(), id, "dorado", false)
		hecho <- err
	}()
	time.Sleep(100 * time.Millisecond)
	m.mu.Lock()
	m.byID[id].HasSecrets = true // lo que hace PutMMDS con el cerrojo tomado
	m.mu.Unlock()
	soltar()

	select {
	case err := <-hecho:
		if err == nil || !strings.Contains(err.Error(), "secrets") {
			t.Fatalf("Commit de una máquina con secretos = %v, quería negarse", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Commit no terminó tras soltar el cerrojo")
	}
	if ll := falso.todas(); len(ll) != 0 {
		t.Errorf("Commit habló con el VMM de una máquina con secretos: %+v", ll)
	}
	if _, err := os.Stat(m.snapDir("dorado")); !os.IsNotExist(err) {
		t.Errorf("quedó un directorio de snapshot: %v", err)
	}
}
