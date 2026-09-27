package machine

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Pruebas del nodo N1 del plan de remediación: fail() sobre la entrada viva
// (M-01), CPUPct y DriveID sin carreras (M-02, M-03) y el cerrojo de ciclo de
// vida durante el arranque (M-06). Las de Freeze usan el arnés de N0
// (fcFalso + vmmFalso): un fc.Client de verdad contra un VMM falso, sin KVM.

// maquinaCorriendo registra una máquina running con un VMM falso completo
// detrás y su PID anotado, como la dejaría un arranque que salió bien. Es
// maquinaConVMM más el PID, que es lo que kill() lee para matar.
func maquinaCorriendo(t *testing.T, m *Manager, id string) (*fcFalso, <-chan struct{}) {
	t.Helper()
	pid, muerto := vmmFalso(t, m, id)
	falso := nuevoFcFalso(t)
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.PID = pid
	m.socket[id] = falso.Sock
	m.mu.Unlock()
	return falso, muerto
}

// vivaDe devuelve una copia de la entrada VIVA de byID, leída bajo el candado.
func vivaDe(t *testing.T, m *Manager, id string) api.Machine {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	mc := m.byID[id]
	if mc == nil {
		t.Fatalf("la máquina %s no está en byID", id)
	}
	return *mc
}

// M-01: fail() con una COPIA (lo que devuelve Get) escribía en la copia. El
// VMM moría pero la máquina seguía running con un PID muerto hasta que el
// vigilante la relabelaba, perdiendo el error real.
func TestFailSobreUnaCopiaMarcaLaViva(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := newID()
	m.addForTest(id)

	copia, ok := m.Get(id)
	if !ok {
		t.Fatal("Get no la encontró")
	}
	m.fail(copia, errors.New("el error de verdad"))

	viva := vivaDe(t, m, id)
	if viva.State != api.StateFailed {
		t.Errorf("estado de la viva = %s, quería failed", viva.State)
	}
	if viva.LastErr != "el error de verdad" {
		t.Errorf("LastErr de la viva = %q: se perdió el error real", viva.LastErr)
	}
	if viva.FailedAt == nil {
		t.Error("la viva no tiene FailedAt: gcFailed no la recogería nunca")
	}
}

// Si la máquina ya no está registrada, fail() no puede escribir en byID: cae
// sobre lo que le pasaron, sin pánico y sin resucitarla en byID.
func TestFailDeUnaMaquinaNoRegistradaNoLaResucita(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	mc := &api.Machine{ID: newID(), Name: "suelta", State: api.StateCreated}

	m.fail(mc, errors.New("boom"))

	if mc.State != api.StateFailed || mc.LastErr != "boom" {
		t.Errorf("la máquina suelta quedó %s / %q, quería failed / boom", mc.State, mc.LastErr)
	}
	m.mu.RLock()
	_, esta := m.byID[mc.ID]
	m.mu.RUnlock()
	if esta {
		t.Error("fail() metió en byID una máquina que no estaba")
	}
}

// M-01, el caso real: el volcado falla y la reanudación también. Freeze llama a
// fail() con su copia; el VMM se mata, y la entrada VIVA tiene que quedar
// failed con el error, no running con un PID muerto.
func TestFreezeQueNoPuedeReanudarMarcaFallidaLaViva(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "f1ee2e0000000001"
	falso, muerto := maquinaCorriendo(t, m, id)

	falso.fallar(http.MethodPut, "/snapshot/create", http.StatusBadRequest, "disk full")
	// El Pause y el Resume van los dos a PATCH /vm: el fallo del Resume se
	// inyecta cuando llega el volcado, que es justo entre uno y otro.
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			falso.fallar(http.MethodPatch, "/vm", http.StatusBadRequest, "vcpu stuck")
		}
	})

	if _, err := m.Freeze(context.Background(), id); err == nil {
		t.Fatal("Freeze con el volcado roto no devolvió error")
	}

	viva := vivaDe(t, m, id)
	if viva.State != api.StateFailed {
		t.Fatalf("estado de la viva = %s, quería failed: el gateway le seguiría enrutando", viva.State)
	}
	if !strings.Contains(viva.LastErr, "could not resume") || !strings.Contains(viva.LastErr, "vcpu stuck") {
		t.Errorf("LastErr = %q: no cuenta que no se pudo reanudar ni por qué", viva.LastErr)
	}
	select {
	case <-muerto:
	case <-time.After(5 * time.Second):
		t.Error("el VMM sigue vivo: fail() no lo mató")
	}
	if n := len(falso.llamadasA(http.MethodPatch, "/vm")); n != 2 {
		t.Errorf("PATCH /vm = %d llamadas, quería 2 (pausa y el intento de reanudar)", n)
	}
}

// El otro lado: si el volcado falla pero la reanudación sale bien, la máquina
// sigue sana. Marcarla fallida mataría trabajo del usuario por un disco lleno.
func TestFreezeFallidaPeroReanudadaSigueRunning(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "f1ee2e0000000002"
	falso, muerto := maquinaCorriendo(t, m, id)
	falso.fallar(http.MethodPut, "/snapshot/create", http.StatusBadRequest, "disk full")

	if _, err := m.Freeze(context.Background(), id); err == nil {
		t.Fatal("Freeze con el volcado roto no devolvió error")
	}

	viva := vivaDe(t, m, id)
	if viva.State != api.StateRunning {
		t.Errorf("estado = %s, quería running: se reanudó bien", viva.State)
	}
	if viva.LastErr != "" {
		t.Errorf("LastErr = %q en una máquina sana", viva.LastErr)
	}
	if !sigueVivo(muerto) {
		t.Error("mató el VMM de una máquina que se reanudó bien")
	}
	llamadas := falso.todas()
	var orden []string
	for _, l := range llamadas {
		orden = append(orden, l.Metodo+" "+l.Ruta)
	}
	want := []string{"PATCH /vm", "PUT /snapshot/create", "PATCH /vm"}
	if strings.Join(orden, ",") != strings.Join(want, ",") {
		t.Errorf("llamadas = %v, quería %v", orden, want)
	}
	if len(llamadas) == 3 && !strings.Contains(string(llamadas[2].Cuerpo), "Resumed") {
		t.Errorf("la última llamada no reanuda: %s", llamadas[2].Cuerpo)
	}
}

// M-03: la foto de persist() no puede compartir arrays ni mapas con la
// máquina viva. Con la copia por valor, escribir en su sitio sobre Volumes o
// Labels de la viva cambiaba también la foto pendiente, que se serializa fuera
// del candado.
func TestPersistNoComparteMemoriaConLaViva(t *testing.T) {
	m := newTestManager(t)
	id := newID()
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.Volumes = []api.VolumeAttachment{{Name: "datos", DriveID: "volume0"}}
	mc.Shares = []api.ShareAttachment{{Mode: "copy", Mount: "/m", Source: "/a"}}
	mc.AllowDomains = []string{"example.com"}
	mc.Labels = map[string]string{"k": "v"}
	mc.Forwards = map[string]string{"8080": "127.0.0.1:1"}
	m.persist()
	// Lo que hacía withDriveIDs antes: escribir en su sitio.
	mc.Volumes[0].DriveID = "otro"
	mc.Shares[0].Source = "/b"
	mc.AllowDomains[0] = "evil.com"
	mc.Labels["k"] = "cambiada"
	mc.Forwards["8080"] = "127.0.0.1:2"
	m.mu.Unlock()

	// Close vuelca la última foto y para persistLoop: lo que queda en disco es
	// la foto tomada ANTES de las escrituras de arriba, se escribiera cuando se
	// escribiera.
	m.Close()
	var foto *api.Machine
	for _, f := range readState(t, m) {
		if f.ID == id {
			foto = &f
		}
	}
	if foto == nil {
		t.Fatal("la máquina no está en state.json")
	}
	if foto.Volumes[0].DriveID != "volume0" {
		t.Errorf("Volumes de la foto = %q: comparte el array con la viva", foto.Volumes[0].DriveID)
	}
	if foto.Shares[0].Source != "/a" {
		t.Errorf("Shares de la foto = %q: comparte el array con la viva", foto.Shares[0].Source)
	}
	if foto.AllowDomains[0] != "example.com" {
		t.Errorf("AllowDomains de la foto = %q: comparte el array con la viva", foto.AllowDomains[0])
	}
	if foto.Labels["k"] != "v" {
		t.Errorf("Labels de la foto = %q: comparte el mapa con la viva", foto.Labels["k"])
	}
	if foto.Forwards["8080"] != "127.0.0.1:1" {
		t.Errorf("Forwards de la foto = %q: comparte el mapa con la viva", foto.Forwards["8080"])
	}
}

// M-03 bajo -race: runFrom anota los DriveID mientras persistLoop serializa
// la foto anterior fuera del candado. Con arrays compartidos, el detector de
// carreras lo ve aquí.
func TestPersistYDriveIDsSinCarrera(t *testing.T) {
	m := newTestManager(t)
	id := newID()
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.Volumes = []api.VolumeAttachment{{Name: "a"}, {Name: "b"}}
	m.persist()
	m.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 200 {
			m.mu.Lock()
			ids := []string{"volume0", "volume1"}
			if i%2 == 1 {
				ids = []string{"volume", ""}
			}
			mc.Volumes = withDriveIDs(mc.Volumes, ids)
			m.persist()
			m.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			m.writePending()
		}
	}()
	wg.Wait()
}

// withDriveIDs devuelve un slice nuevo: el que recibe puede estar compartido
// con cualquier copia tomada antes (List, Get, una foto de persist).
func TestWithDriveIDsNoTocaElOriginal(t *testing.T) {
	orig := []api.VolumeAttachment{{Name: "a", DriveID: "viejo"}, {Name: "b"}}
	got := withDriveIDs(orig, []string{"volume0", ""})

	if orig[0].DriveID != "viejo" {
		t.Errorf("withDriveIDs escribió en el slice que recibió: %q", orig[0].DriveID)
	}
	if got[0].DriveID != "volume0" || got[1].DriveID != "" {
		t.Errorf("resultado = %+v", got)
	}
	if &got[0] == &orig[0] {
		t.Error("withDriveIDs devolvió el mismo array")
	}
}

// arranqueParado lanza un Run de verdad y lo para justo después de publicar la
// máquina, con su cerrojo tomado, hasta que el test mande por continuar lo que
// debe devolver el gancho. Sin KVM, red ni firecracker: Run llega hasta ahí
// con una imagen y un kernel de pega y mkfs.ext4. Si el entorno no da para
// eso (sin mkfs.ext4, sin memoria), la prueba se salta.
func arranqueParado(t *testing.T, m *Manager) (id string, continuar chan<- error, resultado <-chan error) {
	t.Helper()
	img := "prueba-n1"
	for _, f := range []string{m.imagePath(img), m.KernelPath()} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("no es un disco"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dentro := make(chan string, 1)
	cont := make(chan error)
	m.pruebaTrasPublicar = func(id string) error {
		dentro <- id
		return <-cont
	}
	res := make(chan error, 1)
	go func() {
		_, err := m.Run(context.Background(), api.RunRequest{Image: img, VCPUs: 1, MemMiB: 64})
		res <- err
	}()
	select {
	case id = <-dentro:
	case err := <-res:
		t.Skipf("Run no llegó a publicar la máquina en este entorno: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("Run no llegó a publicar la máquina en 30 s")
	}
	return id, cont, res
}

// M-06: un Remove sobre una máquina que está arrancando esperaba... a nada.
// Veía created con PID 0, no mataba nada, borraba el directorio y la entrada,
// y el arranque seguía y lanzaba un VMM huérfano. Ahora Run tiene el cerrojo
// desde que la publica, y Remove espera a que el arranque acabe.
func TestRemoveEsperaAlArranque(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id, continuar, resultado := arranqueParado(t, m)

	borrada := make(chan error, 1)
	go func() { borrada <- m.Remove(id) }()

	select {
	case err := <-borrada:
		t.Fatalf("Remove no esperó al arranque (err=%v): borraría la máquina bajo boot()", err)
	case <-time.After(200 * time.Millisecond):
	}
	// Sigue ahí, a medio construir, y su directorio también.
	if st := vivaDe(t, m, id).State; st != api.StateCreated {
		t.Errorf("estado durante el arranque = %s, quería created", st)
	}
	if _, err := os.Stat(m.dir(id)); err != nil {
		t.Errorf("el directorio desapareció durante el arranque: %v", err)
	}

	continuar <- errors.New("arranque interrumpido por la prueba")
	select {
	case err := <-resultado:
		if err == nil {
			t.Error("Run devolvió éxito tras un fallo inyectado")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run no terminó")
	}
	select {
	case err := <-borrada:
		if err != nil {
			t.Errorf("Remove tras el arranque: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Remove no terminó tras el arranque")
	}

	m.mu.RLock()
	_, sigue := m.byID[id]
	m.mu.RUnlock()
	if sigue {
		t.Error("la máquina sigue en byID")
	}
	if _, err := os.Stat(m.dir(id)); !os.IsNotExist(err) {
		t.Errorf("el directorio sigue ahí: %v", err)
	}
	if n := m.lifecycle.vivos(); n != 0 {
		t.Errorf("quedaron %d cerrojos: Run no soltó el suyo", n)
	}
}

// Stop, igual: espera al arranque en vez de actuar sobre una máquina a medias.
func TestStopEsperaAlArranque(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id, continuar, resultado := arranqueParado(t, m)

	parada := make(chan struct{})
	go func() {
		_, _ = m.Stop(id)
		close(parada)
	}()
	select {
	case <-parada:
		t.Fatal("Stop no esperó al arranque")
	case <-time.After(200 * time.Millisecond):
	}

	continuar <- errors.New("arranque interrumpido por la prueba")
	<-resultado
	select {
	case <-parada:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop no terminó tras el arranque")
	}
}

// Freeze sobre una máquina que estaba arrancando: espera el cerrojo y, al
// conseguirlo, vuelve a leerla. Antes seguía con la copia de ANTES de esperar
// y rechazaba una máquina recién arrancada "por estar created".
func TestFreezeEsperaAlArranqueYReleeLaMaquina(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "f1ee2e0000000003"
	pid, muerto := vmmFalso(t, m, id)
	falso := nuevoFcFalso(t)
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.State = api.StateCreated // como la deja Run al publicarla
	m.mu.Unlock()

	soltar := m.lock(id) // Run arrancándola
	congelada := make(chan error, 1)
	go func() {
		_, err := m.Freeze(context.Background(), id)
		congelada <- err
	}()
	select {
	case err := <-congelada:
		t.Fatalf("Freeze no esperó al arranque: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// El arranque termina bien: PID, socket y running, como al final de Run.
	m.mu.Lock()
	mc.PID, mc.State = pid, api.StateRunning
	m.socket[id] = falso.Sock
	m.mu.Unlock()
	soltar()

	select {
	case err := <-congelada:
		if err != nil {
			t.Fatalf("Freeze tras el arranque: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Freeze no terminó")
	}
	if st := vivaDe(t, m, id).State; st != api.StateWarm {
		t.Errorf("estado = %s, quería warm", st)
	}
	select {
	case <-muerto:
	case <-time.After(5 * time.Second):
		t.Error("el VMM sigue vivo tras congelar")
	}
}
