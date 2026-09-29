package machine

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// Pruebas de P3 (`kling sandbox fork`): el Commit es de verdad, contra el
// arnés fcFalso; la restauración de cada copia se sustituye (restaurarFork),
// porque restaurar de verdad pide KVM y el binario de firecracker. Lo que se
// prueba es lo que añade fork.go: el orden, la herencia de lo que decide la
// petición, el todo-o-nada y la vida del snapshot temporal.

// origenParaFork deja un sandbox running con VMM falso, listo para Commit, y
// hace que el volcado escriba lo que escribiría Firecracker en el snapshot que
// se esté creando (su nombre es aleatorio: se busca por el prefijo).
func origenParaFork(t *testing.T, m *Manager, id string) (*fcFalso, <-chan struct{}) {
	t.Helper()
	if _, err := exec.LookPath("fallocate"); err != nil {
		t.Skip("sin fallocate no se puede perforar el volcado")
	}
	falso, muerto := plantillaParaCommit(t, m, id)
	m.mu.Lock()
	viva := m.byID[id]
	viva.Name = "caja"
	viva.AllowExec = true
	viva.TTLSeconds = 900
	viva.OnTTL = api.OnTTLRemove
	viva.Labels = map[string]string{api.LabelKind: api.KindSandbox}
	m.mu.Unlock()
	falso.enGancho(func(metodo, ruta string) {
		if ruta != "/snapshot/create" {
			return
		}
		dirs, _ := filepath.Glob(filepath.Join(m.root, "snapshots", "fork-*"))
		for _, d := range dirs {
			_ = os.WriteFile(filepath.Join(d, "snap.file"), []byte("estado"), 0o644)
			_ = os.WriteFile(filepath.Join(d, "mem.file"), make([]byte, 1<<20), 0o644)
		}
	})
	return falso, muerto
}

// copiasFalsas sustituye la restauración: registra cada copia en byID como la
// dejaría runFrom (From, etiquetas heredadas del snapshot y las de la
// petición encima). Con fallarEn > 0, la copia número fallarEn falla y deja
// una entrada failed, como hace runFrom a través de m.fail.
func copiasFalsas(t *testing.T, m *Manager, fallarEn int) func() []api.RunRequest {
	t.Helper()
	var mu sync.Mutex
	var pedidas []api.RunRequest
	antes := restaurarFork
	t.Cleanup(func() { restaurarFork = antes })
	restaurarFork = func(ctx context.Context, m *Manager, req api.RunRequest) (*api.Machine, error) {
		// Mientras se restaura, el snapshot ya lleva la marca y sigue
		// reservado por el fork: el barrido no puede llevárselo.
		if !esSnapshotDeFork(m.snapDir(req.From)) {
			t.Errorf("restaurando de %s sin la marca de fork", req.From)
		}
		if !m.forkEnUso(req.From) {
			t.Errorf("restaurando de %s sin que nadie lo retenga", req.From)
		}
		mu.Lock()
		pedidas = append(pedidas, req)
		n := len(pedidas)
		mu.Unlock()
		snap, err := m.loadSnapshot(req.From)
		if err != nil {
			return nil, err
		}
		id := newID()
		mc := &api.Machine{ID: id, Name: req.Name, From: req.From, State: api.StateRunning,
			AllowExec: snap.AllowExec, TTLSeconds: req.TTLSeconds, OnTTL: req.OnTTL,
			Labels: api.MergeLabels(snap.Labels, req.Labels), CreatedAt: time.Now()}
		if n == fallarEn {
			mc.State = api.StateFailed
		}
		m.mu.Lock()
		m.byID[id] = mc
		m.mu.Unlock()
		if n == fallarEn {
			return nil, errors.New("restore failed: no KVM here")
		}
		c := *mc
		return &c, nil
	}
	return func() []api.RunRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]api.RunRequest(nil), pedidas...)
	}
}

// deSnapshot devuelve las máquinas registradas que salieron del snapshot name.
func deSnapshot(m *Manager, name string) []api.Machine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []api.Machine
	for _, mc := range m.byID {
		if mc.From == name {
			out = append(out, *mc)
		}
	}
	return out
}

func snapshotsFork(t *testing.T, m *Manager) []string {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(m.root, "snapshots", "fork-*"))
	return dirs
}

// El camino feliz: el original se pausa, se vuelca y se reanuda una sola vez;
// salen N copias del snapshot temporal, con lo heredado del original; el
// snapshot queda marcado y vive mientras quede una copia, y el barrido se lo
// lleva en cuanto no queda ninguna.
func TestForkCreaCopiasYElSnapshotVivePorEllas(t *testing.T) {
	m := newTestManager(t)
	id := "f0aa170000000001"
	falso, muerto := origenParaFork(t, m, id)
	pedidas := copiasFalsas(t, m, 0)

	snap, copias, err := m.Fork(context.Background(), "caja", ForkOptions{Count: 3})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if !strings.HasPrefix(snap, "fork-"+id[:12]+"-") || !validName.MatchString(snap) {
		t.Errorf("nombre del snapshot temporal = %q", snap)
	}
	comprobarReanudada(t, m, falso, id, muerto)
	if n := len(falso.llamadasA(http.MethodPatch, "/vm")); n != 2 {
		t.Errorf("PATCH /vm = %d llamadas, quería 2 (una pausa y una reanudación para las N copias)", n)
	}

	if len(copias) != 3 {
		t.Fatalf("copias = %d, quería 3", len(copias))
	}
	nombres := map[string]bool{}
	for _, c := range copias {
		if c.From != snap || !strings.HasPrefix(c.Name, "caja-") || nombres[c.Name] {
			t.Errorf("copia %+v: quería From=%s y un nombre caja-<algo> único", c, snap)
		}
		nombres[c.Name] = true
		if c.Labels[api.LabelKind] != api.KindSandbox || c.Labels[api.LabelForkOf] != id {
			t.Errorf("etiquetas de la copia = %v: quería kind=sandbox (del original) y %s=%s",
				c.Labels, api.LabelForkOf, id)
		}
	}
	for _, r := range pedidas() {
		if !r.AllowExec || r.TTLSeconds != 900 || r.OnTTL != api.OnTTLRemove {
			t.Errorf("petición de restauración %+v: quería exec, ttl 900 y on_ttl remove del original", r)
		}
	}

	dir := m.snapDir(snap)
	if !esSnapshotDeFork(dir) {
		t.Fatal("el snapshot temporal no lleva la marca de fork")
	}
	m.mu.RLock()
	reservas := m.reserved[reservaSnapshot(snap)]
	m.mu.RUnlock()
	if reservas != 0 {
		t.Errorf("Fork terminó con %d reserva(s) del snapshot sin soltar", reservas)
	}

	// Con copias vivas (o dormidas), el barrido no lo toca.
	m.mu.Lock()
	m.byID[copias[0].ID].State = api.StateWarm
	m.mu.Unlock()
	m.barrerForks()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("el barrido se llevó un snapshot de fork con copias: %v", err)
	}
	// Sin ninguna, sí.
	m.mu.Lock()
	for _, c := range copias {
		delete(m.byID, c.ID)
	}
	m.mu.Unlock()
	m.barrerForks()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("sin copias, el snapshot de fork sigue ahí (err=%v)", err)
	}
	if v := vivaDe(t, m, id); v.State != api.StateRunning {
		t.Errorf("el original quedó %s", v.State)
	}
}

// Lo que dice la petición manda sobre lo heredado.
func TestForkRespetaTTLDeLaPeticion(t *testing.T) {
	m := newTestManager(t)
	origenParaFork(t, m, "f0aa170000000002")
	pedidas := copiasFalsas(t, m, 0)

	if _, _, err := m.Fork(context.Background(), "caja", ForkOptions{TTLSeconds: 60, OnTTL: api.OnTTLFreeze}); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	p := pedidas()
	if len(p) != 1 || p[0].TTLSeconds != 60 || p[0].OnTTL != api.OnTTLFreeze {
		t.Fatalf("peticiones = %+v: quería una (Count 0 es 1), con ttl 60 y on_ttl freeze", p)
	}
}

// Todo o nada: si la segunda copia no se restaura, la primera se elimina,
// también la entrada failed de la segunda, y el snapshot temporal desaparece.
// El original sigue corriendo.
func TestForkFallidoDeshaceCopiasYSnapshot(t *testing.T) {
	m := newTestManager(t)
	id := "f0aa170000000003"
	falso, muerto := origenParaFork(t, m, id)
	copiasFalsas(t, m, 2)

	snap, copias, err := m.Fork(context.Background(), "caja", ForkOptions{Count: 3})
	if err == nil || !strings.Contains(err.Error(), "fork 2 of 3") {
		t.Fatalf("Fork con la 2.ª restauración rota: %v", err)
	}
	if snap != "" || copias != nil {
		t.Errorf("un fork fallido devolvió %q y %d copias", snap, len(copias))
	}
	if dirs := snapshotsFork(t, m); len(dirs) != 0 {
		t.Errorf("quedaron snapshots de fork: %v", dirs)
	}
	m.mu.RLock()
	quedan := len(m.byID)
	m.mu.RUnlock()
	if quedan != 1 {
		t.Errorf("quedan %d máquinas, quería solo el original", quedan)
	}
	comprobarReanudada(t, m, falso, id, muerto)
}

// Lista (la espera del agente, en el daemon) también puede tumbar el fork.
func TestForkListaRechazaYSeDeshace(t *testing.T) {
	m := newTestManager(t)
	origenParaFork(t, m, "f0aa170000000004")
	copiasFalsas(t, m, 0)

	_, _, err := m.Fork(context.Background(), "caja", ForkOptions{Count: 2,
		Lista: func(ctx context.Context, mc *api.Machine) error { return errors.New("agent never answered") }})
	if err == nil || !strings.Contains(err.Error(), "agent never answered") {
		t.Fatalf("Fork con Lista fallando: %v", err)
	}
	if dirs := snapshotsFork(t, m); len(dirs) != 0 {
		t.Errorf("quedaron snapshots de fork: %v", dirs)
	}
	m.mu.RLock()
	quedan := len(m.byID)
	m.mu.RUnlock()
	if quedan != 1 {
		t.Errorf("quedan %d máquinas, quería solo el original", quedan)
	}
}

// Si el propio Commit falla, no hay nada que restaurar ni que borrar, y el
// original sigue corriendo (eso ya lo garantiza Commit).
func TestForkConCommitFallidoNoRestauraNada(t *testing.T) {
	m := newTestManager(t)
	id := "f0aa170000000005"
	falso, muerto := origenParaFork(t, m, id)
	pedidas := copiasFalsas(t, m, 0)
	falso.fallar(http.MethodPut, "/snapshot/create", http.StatusBadRequest, "No space left on device")

	if _, _, err := m.Fork(context.Background(), "caja", ForkOptions{Count: 2}); err == nil ||
		!strings.Contains(err.Error(), "No space left") {
		t.Fatalf("Fork con el volcado roto: %v", err)
	}
	if n := len(pedidas()); n != 0 {
		t.Errorf("se intentaron %d restauraciones sin snapshot", n)
	}
	if dirs := snapshotsFork(t, m); len(dirs) != 0 {
		t.Errorf("quedaron snapshots de fork: %v", dirs)
	}
	comprobarReanudada(t, m, falso, id, muerto)
}

// Lo que no se puede ramificar se rechaza ANTES de pausar nada.
func TestForkRechazaAntesDePausar(t *testing.T) {
	m := newTestManager(t)
	id := "f0aa170000000006"
	falso, _ := origenParaFork(t, m, id)
	pedidas := copiasFalsas(t, m, 0)

	cambiar := func(f func(*api.Machine)) {
		m.mu.Lock()
		viva := m.byID[id]
		viva.State, viva.HasSecrets, viva.Volumes, viva.Shares, viva.CredentialDomains = api.StateRunning, false, nil, nil, nil
		f(viva)
		m.mu.Unlock()
	}
	casos := []struct {
		nombre string
		ref    string
		opt    ForkOptions
		cambio func(*api.Machine)
		quiere error
	}{
		{"no existe", "nadie", ForkOptions{}, func(*api.Machine) {}, ErrNoMachine},
		{"demasiadas", "caja", ForkOptions{Count: api.ForkMax + 1}, func(*api.Machine) {}, ErrFork},
		{"negativo", "caja", ForkOptions{Count: -1}, func(*api.Machine) {}, ErrFork},
		{"dormida", "caja", ForkOptions{}, func(v *api.Machine) { v.State = api.StateWarm }, ErrNotRunning},
		{"secretos", "caja", ForkOptions{}, func(v *api.Machine) { v.HasSecrets = true }, ErrFork},
		{"volumen rw", "caja", ForkOptions{}, func(v *api.Machine) {
			v.Volumes = []api.VolumeAttachment{{Name: "datos", Mount: "/data"}}
		}, ErrFork},
		{"carpeta", "caja", ForkOptions{}, func(v *api.Machine) {
			v.Shares = []api.ShareAttachment{{Mode: "copy", Mount: "/mnt"}}
		}, ErrFork},
		{"credenciales", "caja", ForkOptions{}, func(v *api.Machine) { v.CredentialDomains = []string{"api.example.com"} }, ErrFork},
	}
	for _, c := range casos {
		cambiar(c.cambio)
		if _, _, err := m.Fork(context.Background(), c.ref, c.opt); !errors.Is(err, c.quiere) {
			t.Errorf("%s: %v, quería %v", c.nombre, err, c.quiere)
		}
	}
	// El mensaje dice cómo hacerlo bien.
	cambiar(func(v *api.Machine) { v.CredentialDomains = []string{"api.example.com"} })
	if _, _, err := m.Fork(context.Background(), "caja", ForkOptions{}); err == nil ||
		!strings.Contains(err.Error(), "api.example.com") || !strings.Contains(err.Error(), "run -from") {
		t.Errorf("credenciales: %v", err)
	}
	// Con el almacén en disco basta, aunque state.json no lo diga todavía.
	cambiar(func(*api.Machine) {})
	if err := m.guardarCredenciales(id, []credproxy.Credential{{Env: "KEY", Domain: "api.example.com",
		Placeholder: credproxy.PlaceholderPrefix + "aa", Secret: "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Fork(context.Background(), "caja", ForkOptions{}); !errors.Is(err, ErrFork) {
		t.Errorf("almacén sin CredentialDomains: %v", err)
	}
	if err := m.guardarCredenciales(id, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(falso.todas()); n != 0 {
		t.Errorf("se habló %d veces con el VMM para peticiones que había que rechazar antes", n)
	}
	if len(pedidas()) != 0 || len(snapshotsFork(t, m)) != 0 {
		t.Error("un fork rechazado llegó a restaurar o a dejar snapshot")
	}
}

// El barrido solo se lleva snapshots MARCADOS como de fork, y ni siquiera esos
// mientras alguien los tenga reservados.
func TestBarrerForksSoloLosMarcadosYLibres(t *testing.T) {
	m := newTestManager(t)
	ajeno := escribirSnapshot(t, m, "fork-de-usuario", api.Snapshot{})
	marcado := escribirSnapshot(t, m, "fork-temporal", api.Snapshot{})
	if err := os.WriteFile(filepath.Join(marcado, forkMarca), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	soltar := m.reserveDir(reservaSnapshot("fork-temporal"))
	m.barrerForks()
	if _, err := os.Stat(marcado); err != nil {
		t.Fatalf("el barrido se llevó un snapshot de fork reservado: %v", err)
	}
	soltar()
	m.barrerForks()
	if _, err := os.Stat(marcado); !os.IsNotExist(err) {
		t.Errorf("libre y sin copias, el snapshot de fork sigue ahí (err=%v)", err)
	}
	if _, err := os.Stat(ajeno); err != nil {
		t.Errorf("el barrido se llevó un snapshot sin marca (de un usuario): %v", err)
	}
}

// Las etiquetas pedidas llegan a cada copia en su nacimiento, junto a fork-of;
// las inválidas se rechazan antes de pausar nada.
func TestForkEtiquetasDeLasCopias(t *testing.T) {
	m := newTestManager(t)
	id := "f0aa170000000007"
	falso, _ := origenParaFork(t, m, id)
	pedidas := copiasFalsas(t, m, 0)

	for _, malas := range []map[string]string{
		{"Mal": "x"}, {api.LabelForkOf: "otro"}, {api.LabelKind: "service"},
	} {
		if _, _, err := m.Fork(context.Background(), "caja", ForkOptions{Labels: malas}); !errors.Is(err, ErrFork) {
			t.Errorf("etiquetas %v: %v, quería ErrFork", malas, err)
		}
	}
	if n := len(falso.todas()); n != 0 {
		t.Errorf("se habló %d veces con el VMM para etiquetas inválidas", n)
	}

	_, copias, err := m.Fork(context.Background(), "caja", ForkOptions{Count: 2,
		Labels: map[string]string{"kling.db.state": "preparing"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(copias) != 2 || len(pedidas()) != 2 {
		t.Fatalf("copias %d, pedidas %d", len(copias), len(pedidas()))
	}
	for _, r := range pedidas() {
		if r.Labels["kling.db.state"] != "preparing" || r.Labels[api.LabelForkOf] != id {
			t.Errorf("la petición de restauración lleva %v", r.Labels)
		}
	}
	for _, c := range copias {
		if c.Labels["kling.db.state"] != "preparing" || c.Labels[api.LabelKind] != api.KindSandbox {
			t.Errorf("copia con %v", c.Labels)
		}
	}
}

// Si el almacén de credenciales no se puede mirar (ni existe ni deja de
// existir: ENOTDIR), se rechaza en vez de suponer que no hay credenciales.
func TestForkSinCredencialesFallaCerrado(t *testing.T) {
	m := newTestManager(t)
	src := &api.Machine{ID: "f0aa170000000008", Name: "caja"}
	if err := m.forkSinCredenciales(src); err != nil {
		t.Fatalf("sin almacén: %v", err)
	}
	// El "directorio" de la máquina es un fichero: Stat da ENOTDIR.
	if err := os.MkdirAll(filepath.Dir(m.dir(src.ID)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.dir(src.ID), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.forkSinCredenciales(src); !errors.Is(err, ErrFork) {
		t.Errorf("almacén ilegible: %v, quería ErrFork", err)
	}
}

// Unas credenciales dadas mientras el fork esperaba el cerrojo de la máquina
// (TOCTOU con SetCredentials) lo rechazan antes de pausarla.
func TestForkRepiteLaComprobacionBajoElCerrojo(t *testing.T) {
	m := newTestManager(t)
	id := "f0aa170000000009"
	falso, _ := origenParaFork(t, m, id)
	pedidas := copiasFalsas(t, m, 0)

	soltar := m.lock(id) // como un SetCredentials en curso
	hecho := make(chan error, 1)
	go func() {
		_, _, err := m.Fork(context.Background(), "caja", ForkOptions{})
		hecho <- err
	}()
	time.Sleep(200 * time.Millisecond) // el fork ya pasó su comprobación sin cerrojo
	if err := m.guardarCredenciales(id, []credproxy.Credential{{Env: "KEY", Domain: "api.example.com",
		Placeholder: credproxy.PlaceholderPrefix + "aa", Secret: "x"}}); err != nil {
		t.Fatal(err)
	}
	soltar()
	if err := <-hecho; !errors.Is(err, ErrFork) || !strings.Contains(err.Error(), "run -from") {
		t.Fatalf("fork tras darle credenciales: %v", err)
	}
	if n := len(falso.todas()); n != 0 {
		t.Errorf("se pausó/volcó la máquina (%d llamadas al VMM)", n)
	}
	if len(pedidas()) != 0 || len(snapshotsFork(t, m)) != 0 {
		t.Error("el fork rechazado dejó copias o snapshot")
	}
}
