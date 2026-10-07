package upgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// klingFalso es un "kling" de mentira: un script que contesta a `version` y,
// si esquemas no está vacío, a `upgrade -schemas`, como lo haría uno de
// verdad. Es lo que Sondear ejecuta.
func klingFalso(version, esquemas string) []byte {
	s := "#!/bin/sh\n"
	if esquemas != "" {
		s += fmt.Sprintf("if [ \"$1 $2\" = \"upgrade -schemas\" ]; then echo '{\"kling\":\"%s\",\"api\":1,\"schemas\":%s}'; exit 0; fi\n", version, esquemas)
	}
	s += fmt.Sprintf("if [ \"$1\" = version ]; then echo 'kling %s'; echo 'daemon: not reachable'; exit 0; fi\nexit 2\n", version)
	return []byte(s)
}

const esquemasHoy = `{"state":2,"meta":1,"credentials":1}`

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// releaseFalsa sirve /releases/latest (redirige a la etiqueta) y los assets
// de una release con su SHA256SUMS. sumas sustituye a las buenas si no es nil.
type releaseFalsa struct {
	etiqueta string
	assets   map[string][]byte
	sumas    map[string]string
	pedidos  []string
	mu       sync.Mutex
	// alPedir corre en cada petición: lo que pasa en el host mientras baja.
	alPedir func(path string)
}

func (r *releaseFalsa) servir(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.pedidos = append(r.pedidos, req.URL.Path)
		r.mu.Unlock()
		if r.alPedir != nil {
			r.alPedir(req.URL.Path)
		}
		pre := "/releases/download/" + r.etiqueta + "/"
		switch {
		case req.URL.Path == "/releases/latest":
			http.Redirect(w, req, "/releases/tag/"+r.etiqueta, http.StatusFound)
		case req.URL.Path == pre+"SHA256SUMS":
			var b strings.Builder
			for n, body := range r.assets {
				s := sha(body)
				if r.sumas != nil {
					var ok bool
					if s, ok = r.sumas[n]; !ok {
						continue
					}
				}
				fmt.Fprintf(&b, "%s  %s\n", s, n)
			}
			w.Write([]byte(b.String()))
		case strings.HasPrefix(req.URL.Path, pre):
			body, ok := r.assets[strings.TrimPrefix(req.URL.Path, pre)]
			if !ok {
				http.NotFound(w, req)
				return
			}
			w.Write(body)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// daemonFalso es el daemon tal como lo ve upgrade: versión, máquinas y
// dorados. caido simula que no contesta.
type daemonFalso struct {
	mu       sync.Mutex
	version  string
	caido    bool
	maquinas []*api.Machine
	dorados  []*api.Snapshot
	root     string
	// mudo es una versión que arranca pero nunca contesta; alPreguntar corre
	// en cada Info, con la versión que corre.
	mudo        string
	alPreguntar func(version string)
	// alCongelar corre en cada Freeze; si devuelve error, el freeze falla.
	alCongelar func(id string) error
}

func (d *daemonFalso) Info(context.Context) (*api.Info, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.alPreguntar != nil {
		d.alPreguntar(d.version)
	}
	if d.caido || (d.mudo != "" && d.version == d.mudo) {
		return nil, errors.New("connection refused")
	}
	return &api.Info{Version: d.version, Root: d.root}, nil
}

func (d *daemonFalso) List(context.Context) ([]*api.Machine, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.caido {
		return nil, errors.New("connection refused")
	}
	return append([]*api.Machine(nil), d.maquinas...), nil
}

func (d *daemonFalso) Freeze(_ context.Context, id string) (*api.Machine, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.caido {
		return nil, errors.New("connection refused")
	}
	if d.alCongelar != nil {
		if err := d.alCongelar(id); err != nil {
			return nil, err
		}
	}
	for _, m := range d.maquinas {
		if m.ID == id {
			m.State = api.StateWarm
			return m, nil
		}
	}
	return nil, errors.New("no such machine")
}

func (d *daemonFalso) Snapshots(context.Context) ([]*api.Snapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*api.Snapshot(nil), d.dorados...), nil
}

// servicioFalso para y arranca el daemon falso con el kling que haya en
// binario, como haría systemd. alArrancar deja simular lo que hace el daemon
// nuevo al arrancar (migrar, fallar, perder una máquina).
type servicioFalso struct {
	d          *daemonFalso
	binario    string
	llamadas   []string
	alArrancar func(version string) error
	// alParar corre al parar, con la versión que corría: lo que escribe el
	// daemon al apagarse.
	alParar func(version string)
}

func (s *servicioFalso) String() string { return "fake.service" }

func (s *servicioFalso) Parar(context.Context) error {
	s.llamadas = append(s.llamadas, "stop")
	if s.alParar != nil {
		s.alParar(s.d.version)
	}
	s.d.mu.Lock()
	s.d.caido = true
	s.d.mu.Unlock()
	return nil
}

func (s *servicioFalso) Arrancar(ctx context.Context) error {
	s.llamadas = append(s.llamadas, "start")
	ib, err := Sondear(ctx, s.binario)
	if err != nil {
		return err
	}
	if s.alArrancar != nil {
		if err := s.alArrancar(ib.Kling); err != nil {
			return err
		}
	}
	s.d.mu.Lock()
	s.d.caido = false
	s.d.version = ib.Kling
	s.d.mu.Unlock()
	return nil
}

// montaje es un host de prueba: un kling v1.0.0 instalado y corriendo sobre
// una raíz con un state.json v0, y una release v1.1.0 que lo sustituye.
type montaje struct {
	dir, raiz, bin, guest string
	viejo, nuevo          []byte
	rel                   *releaseFalsa
	srv                   *httptest.Server
	d                     *daemonFalso
	svc                   *servicioFalso
	out                   bytes.Buffer
}

const estadoV0 = `[{"id":"aaaa","state":"frozen"},{"id":"bbbb","state":"running"}]`

func nuevoMontaje(t *testing.T) *montaje {
	t.Helper()
	m := &montaje{dir: t.TempDir(), raiz: t.TempDir()}
	m.bin = filepath.Join(m.dir, "bin", "kling")
	m.guest = filepath.Join(m.dir, "lib", "kling-guest")
	os.MkdirAll(filepath.Dir(m.bin), 0o755)
	os.MkdirAll(filepath.Dir(m.guest), 0o755)
	m.viejo = klingFalso("v1.0.0", esquemasHoy)
	m.nuevo = klingFalso("v1.1.0", esquemasHoy)
	os.WriteFile(m.bin, m.viejo, 0o755)
	os.WriteFile(m.guest, []byte("guest viejo"), 0o755)
	os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(estadoV0), 0o600)
	m.rel = &releaseFalsa{etiqueta: "v1.1.0", assets: map[string][]byte{
		"kling-linux-amd64":       m.nuevo,
		"kling-guest-linux-amd64": []byte("guest nuevo"),
	}}
	m.srv = m.rel.servir(t)
	m.d = &daemonFalso{version: "v1.0.0", root: m.raiz,
		maquinas: []*api.Machine{{ID: "aaaa", State: api.StateWarm}, {ID: "bbbb", State: api.StateRunning}},
		dorados:  []*api.Snapshot{{Name: "pg"}}}
	m.svc = &servicioFalso{d: m.d, binario: m.bin}
	return m
}

func (m *montaje) opciones() Opciones {
	return Opciones{
		Dir:    filepath.Join(m.raiz, "upgrade"),
		Fuente: &Fuente{Repo: m.srv.URL, permitirHTTP: true},
		Piezas: []Pieza{
			{Asset: Asset{Nombre: "kling-linux-amd64", Corto: "kling"}, Destino: m.bin},
			{Asset: Asset{Nombre: "kling-guest-linux-amd64", Corto: "kling-guest"}, Destino: m.guest},
		},
		Servicio: m.svc, Daemon: m.d, Plazo: 2e9, Out: &m.out,
	}
}

func leer(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// noTocado comprueba que un fallo antes de parar no cambió nada.
func (m *montaje) noTocado(t *testing.T) {
	t.Helper()
	if leer(t, m.bin) != string(m.viejo) || leer(t, m.guest) != "guest viejo" {
		t.Error("the installed binaries changed")
	}
	if len(m.svc.llamadas) != 0 {
		t.Errorf("the daemon was touched: %v", m.svc.llamadas)
	}
	if _, err := os.Stat(filepath.Join(m.raiz, "upgrade", "backups")); err == nil {
		t.Error("a backup was made before failing: it must fail before")
	}
}

func TestActualizarCambiaArrancaYVerifica(t *testing.T) {
	m := nuevoMontaje(t)
	m.svc.alArrancar = func(v string) error {
		if v == "v1.1.0" { // el daemon nuevo migra state.json con su copia
			os.WriteFile(filepath.Join(m.raiz, "state.json.v0.bak"), []byte(estadoV0), 0o600)
			os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(`{"schema":1,"machines":[]}`), 0o600)
		}
		return nil
	}
	res, err := Actualizar(context.Background(), m.opciones())
	if err != nil {
		t.Fatalf("%v\n%s", err, m.out.String())
	}
	if res.Desde != "v1.0.0" || res.Hacia != "v1.1.0" {
		t.Errorf("from %s to %s", res.Desde, res.Hacia)
	}
	if leer(t, m.bin) != string(m.nuevo) || leer(t, m.guest) != "guest nuevo" {
		t.Error("the binaries were not replaced")
	}
	if strings.Join(m.svc.llamadas, ",") != "stop,start" {
		t.Errorf("service calls %v, want stop,start", m.svc.llamadas)
	}
	if st, _ := os.Stat(m.bin); st.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", st.Mode())
	}
	// La copia guarda lo de antes; lo bajado no se queda en disco.
	if got := leer(t, filepath.Join(res.Copia, "0-kling")); got != string(m.viejo) {
		t.Error("the backup does not hold the previous kling")
	}
	if got := leer(t, filepath.Join(res.Copia, "state.json")); got != estadoV0 {
		t.Error("the backup does not hold the previous state.json")
	}
	if _, err := os.Stat(filepath.Join(m.raiz, "upgrade", "v1.1.0")); !os.IsNotExist(err) {
		t.Error("the downloads were left behind")
	}
	if !strings.Contains(m.out.String(), "state.json: schema 0 -> 1") {
		t.Errorf("the plan does not mention the state.json migration:\n%s", m.out.String())
	}
}

func TestEtiquetaPedidaYUltima(t *testing.T) {
	m := nuevoMontaje(t)
	f := &Fuente{Repo: m.srv.URL, permitirHTTP: true}
	if got, err := f.UltimaEtiqueta(context.Background()); err != nil || got != "v1.1.0" {
		t.Fatalf("latest = %q, %v", got, err)
	}
	// Sin la puerta de los tests, http no se usa.
	if _, err := (&Fuente{Repo: m.srv.URL}).UltimaEtiqueta(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http accepted: %v", err)
	}
	o := m.opciones()
	o.Etiqueta = "v1.1.0/../x"
	if _, err := Actualizar(context.Background(), o); err == nil {
		t.Error("a tag with a path was accepted")
	}
}

func TestChecksumMaloNoTocaNada(t *testing.T) {
	m := nuevoMontaje(t)
	m.rel.sumas = map[string]string{
		"kling-linux-amd64":       sha(m.nuevo),
		"kling-guest-linux-amd64": sha([]byte("otra cosa")),
	}
	_, err := Actualizar(context.Background(), m.opciones())
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want a sha256 mismatch", err)
	}
	m.noTocado(t)
}

func TestAssetSinSumaNoSeAcepta(t *testing.T) {
	m := nuevoMontaje(t)
	m.rel.sumas = map[string]string{"kling-linux-amd64": sha(m.nuevo)}
	_, err := Actualizar(context.Background(), m.opciones())
	if err == nil || !strings.Contains(err.Error(), "not listed") {
		t.Fatalf("err = %v, want not listed", err)
	}
	m.noTocado(t)
}

func TestEsquemaIncompatibleNoTocaNada(t *testing.T) {
	for nombre, prep := range map[string]func(m *montaje){
		"state.json del futuro": func(m *montaje) {
			os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(`{"schema":3,"machines":[]}`), 0o600)
		},
		"almacén de credenciales del futuro": func(m *montaje) {
			os.MkdirAll(filepath.Join(m.raiz, "machines", "aaaa"), 0o700)
			os.WriteFile(filepath.Join(m.raiz, "machines", "aaaa", "credentials.enc"), []byte("KLCS\x02nonce..."), 0o600)
		},
		"destino anterior a los esquemas sobre un estado v1": func(m *montaje) {
			m.rel.assets["kling-linux-amd64"] = klingFalso("v1.1.0", "")
			os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(`{"schema":1,"machines":[]}`), 0o600)
		},
	} {
		t.Run(nombre, func(t *testing.T) {
			m := nuevoMontaje(t)
			prep(m)
			_, err := Actualizar(context.Background(), m.opciones())
			if err == nil || !strings.Contains(err.Error(), "cannot use this daemon's data") {
				t.Fatalf("err = %v", err)
			}
			m.noTocado(t)
		})
	}
}

func TestMismaVersionYAnterior(t *testing.T) {
	m := nuevoMontaje(t)
	o := m.opciones()
	o.Etiqueta = "v1.0.0"
	res, err := Actualizar(context.Background(), o)
	if err != nil || !res.AlDia {
		t.Fatalf("same version: %v %+v", err, res)
	}
	if len(m.rel.pedidos) != 0 {
		t.Errorf("downloaded for nothing: %v", m.rel.pedidos)
	}

	m = nuevoMontaje(t)
	m.d.version = "v1.2.0"
	_, err = Actualizar(context.Background(), m.opciones())
	if err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("downgrade without -force: %v", err)
	}
	m.noTocado(t)
}

func TestEnSecoNoCambiaNada(t *testing.T) {
	m := nuevoMontaje(t)
	o := m.opciones()
	o.EnSeco = true
	if _, err := Actualizar(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	m.noTocado(t)
	if !strings.Contains(m.out.String(), "replace "+m.bin) {
		t.Errorf("the plan does not list the binaries:\n%s", m.out.String())
	}
}

// Si el daemon nuevo no arranca después de migrar, todo vuelve: binarios,
// state.json y la copia de migración, y el viejo arranca.
func TestFalloAlArrancarVuelveAtras(t *testing.T) {
	m := nuevoMontaje(t)
	m.svc.alArrancar = func(v string) error {
		if v != "v1.1.0" {
			return nil
		}
		os.WriteFile(filepath.Join(m.raiz, "state.json.v0.bak"), []byte(estadoV0), 0o600)
		os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(`{"schema":1,"machines":[]}`), 0o600)
		return errors.New("exit status 1")
	}
	_, err := Actualizar(context.Background(), m.opciones())
	var va *ErrVueltaAtras
	if !errors.As(err, &va) || va.Fallo != nil {
		t.Fatalf("err = %v, want a clean rollback", err)
	}
	if leer(t, m.bin) != string(m.viejo) || leer(t, m.guest) != "guest viejo" {
		t.Error("the previous binaries are not back")
	}
	if leer(t, filepath.Join(m.raiz, "state.json")) != estadoV0 {
		t.Error("state.json is not back")
	}
	if _, err := os.Stat(filepath.Join(m.raiz, "state.json.v0.bak")); !os.IsNotExist(err) {
		t.Error("the migration copy is still there: the next upgrade would not make a fresh one")
	}
	if m.d.version != "v1.0.0" || m.d.caido {
		t.Errorf("daemon %s down=%v, want v1.0.0 up", m.d.version, m.d.caido)
	}
}

// Arrancar no basta: si el daemon nuevo no ve una máquina congelada o un
// dorado que había, se vuelve atrás.
func TestPerderUnaMaquinaVuelveAtras(t *testing.T) {
	for nombre, perder := range map[string]func(d *daemonFalso){
		"congelada que despierta": func(d *daemonFalso) { d.maquinas[0] = &api.Machine{ID: "aaaa", State: api.StateFailed} },
		"congelada que falta":     func(d *daemonFalso) { d.maquinas = d.maquinas[1:] },
		"dorado que falta":        func(d *daemonFalso) { d.dorados = nil },
	} {
		t.Run(nombre, func(t *testing.T) {
			m := nuevoMontaje(t)
			var guardadas []*api.Machine
			var dorados []*api.Snapshot
			m.svc.alArrancar = func(v string) error {
				m.d.mu.Lock()
				defer m.d.mu.Unlock()
				if v == "v1.1.0" {
					guardadas = append([]*api.Machine(nil), m.d.maquinas...)
					dorados = m.d.dorados
					perder(m.d)
				} else if guardadas != nil {
					m.d.maquinas, m.d.dorados = guardadas, dorados
				}
				return nil
			}
			_, err := Actualizar(context.Background(), m.opciones())
			var va *ErrVueltaAtras
			if !errors.As(err, &va) || va.Fallo != nil || !strings.Contains(err.Error(), "lost things") {
				t.Fatalf("err = %v", err)
			}
			if leer(t, m.bin) != string(m.viejo) {
				t.Error("the previous kling is not back")
			}
		})
	}
}

func TestVolverAtrasManual(t *testing.T) {
	m := nuevoMontaje(t)
	m.svc.alArrancar = func(v string) error {
		if v == "v1.1.0" {
			if _, err := os.Stat(filepath.Join(m.raiz, "state.json.v0.bak")); os.IsNotExist(err) {
				os.WriteFile(filepath.Join(m.raiz, "state.json.v0.bak"), []byte(estadoV0), 0o600)
				os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(`{"schema":1,"machines":[]}`), 0o600)
			}
		}
		return nil
	}
	o := m.opciones()
	res, err := Actualizar(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	m.svc.llamadas = nil
	if _, err := VolverAtras(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if leer(t, m.bin) != string(m.viejo) || leer(t, m.guest) != "guest viejo" {
		t.Error("the previous binaries are not back")
	}
	if leer(t, filepath.Join(m.raiz, "state.json")) != estadoV0 {
		t.Error("the migrated state.json is not back from its .v0.bak")
	}
	if m.d.version != "v1.0.0" {
		t.Errorf("daemon at %s", m.d.version)
	}
	if strings.Join(m.svc.llamadas, ",") != "stop,start" {
		t.Errorf("service calls %v", m.svc.llamadas)
	}
	if _, err := os.Stat(res.Copia); !os.IsNotExist(err) {
		t.Error("a used backup must go: the next -rollback is to the one before")
	}
	if _, err := VolverAtras(context.Background(), o); err == nil || !strings.Contains(err.Error(), "no upgrade backup") {
		t.Errorf("second rollback: %v", err)
	}
}

func TestSoloSeGuardanDosCopias(t *testing.T) {
	m := nuevoMontaje(t)
	o := m.opciones()
	o.Forzar = true
	for i := 0; i < 3; i++ {
		if _, err := Actualizar(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(o.Dir, "backups"))
	if len(ents) != copiasGuardadas {
		t.Errorf("%d backups kept, want %d", len(ents), copiasGuardadas)
	}
}

// Desde un directorio: con SHA256SUMS se verifica igual; sin él, se acepta
// (los ficheros ya están en esta máquina) y vale el nombre corto.
func TestDesdeDirectorio(t *testing.T) {
	m := nuevoMontaje(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "kling"), m.nuevo, 0o755)
	os.WriteFile(filepath.Join(dir, "kling-guest"), []byte("guest nuevo"), 0o755)
	o := m.opciones()
	o.Fuente = &Fuente{Dir: dir}
	if _, err := Actualizar(context.Background(), o); err != nil {
		t.Fatalf("%v\n%s", err, m.out.String())
	}
	if leer(t, m.bin) != string(m.nuevo) {
		t.Error("not replaced")
	}

	m = nuevoMontaje(t)
	os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sha(m.nuevo)+"  kling\n"+sha([]byte("x"))+"  kling-guest\n"), 0o644)
	o = m.opciones()
	o.Fuente = &Fuente{Dir: dir}
	if _, err := Actualizar(context.Background(), o); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	m.noTocado(t)
}

func TestSondearUnKlingAnterior(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kling")
	os.WriteFile(p, klingFalso("v0.17.0", ""), 0o755)
	ib, err := Sondear(context.Background(), p)
	if err != nil || ib.Kling != "v0.17.0" || !ib.SinEsquemas {
		t.Fatalf("%+v %v", ib, err)
	}
}

// Solo el CLI (-cli, o un contexto remoto): sin daemon que parar; se cambia el
// binario, se comprueba que dice la versión nueva, y -rollback lo devuelve.
func TestSoloElCLI(t *testing.T) {
	m := nuevoMontaje(t)
	o := m.opciones()
	o.Servicio, o.Daemon, o.Raiz, o.Actual = nil, nil, "", "v1.0.0"
	o.Piezas = o.Piezas[:1]
	res, err := Actualizar(context.Background(), o)
	if err != nil || res.Hacia != "v1.1.0" {
		t.Fatalf("%+v %v\n%s", res, err, m.out.String())
	}
	if leer(t, m.bin) != string(m.nuevo) {
		t.Error("not replaced")
	}
	if _, err := VolverAtras(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if leer(t, m.bin) != string(m.viejo) {
		t.Error("not rolled back")
	}
	o.Servicio = m.svc
	if _, err := Actualizar(context.Background(), o); err == nil {
		t.Error("a service without a daemon to check was accepted")
	}
}

// La vuelta atrás automática devuelve el state.json de la copia aunque el
// daemon nuevo lo reescribiera sin migrarlo (misma versión, campos nuevos), y
// no toca las copias de migración que ya estaban antes de actualizar.
func TestVueltaAtrasDevuelveElEstadoYRespetaLasCopiasViejas(t *testing.T) {
	m := nuevoMontaje(t)
	v1 := `{"schema":1,"machines":[{"id":"aaaa","state":"frozen"}]}`
	os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(v1), 0o600)
	meta := filepath.Join(m.raiz, "snapshots", "pg", "meta.json")
	os.MkdirAll(filepath.Dir(meta), 0o700)
	os.WriteFile(meta, []byte(`{"schema":1,"name":"pg"}`), 0o600)
	os.WriteFile(meta+".v0.bak", []byte(`{"name":"pg","viejo":true}`), 0o600)
	m.svc.alArrancar = func(v string) error {
		if v == "v1.1.0" {
			os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(`{"schema":1,"machines":[],"nuevo":1}`), 0o600)
			return errors.New("exit status 1")
		}
		return nil
	}
	_, err := Actualizar(context.Background(), m.opciones())
	var va *ErrVueltaAtras
	if !errors.As(err, &va) || va.Fallo != nil {
		t.Fatalf("err = %v", err)
	}
	if got := leer(t, filepath.Join(m.raiz, "state.json")); got != v1 {
		t.Errorf("state.json = %s, want the one from before", got)
	}
	if got := leer(t, meta); got != `{"schema":1,"name":"pg"}` {
		t.Errorf("an old migration copy was restored over meta.json: %s", got)
	}
	if _, err := os.Stat(meta + ".v0.bak"); err != nil {
		t.Error("an old migration copy was consumed")
	}
}

// Lo que hace el gateway mientras se actualiza no es una pérdida: una
// congelada que un cliente despierta en cuanto el daemon nuevo contesta, una
// máquina de sesión que se borra, y lo que cambie durante la descarga (la
// foto se toma después).
func TestLoQueHaceElGatewayNoEsUnaPerdida(t *testing.T) {
	m := nuevoMontaje(t)
	m.d.maquinas = append(m.d.maquinas, &api.Machine{ID: "cccc", State: api.StateWarm})
	m.rel.alPedir = func(path string) {
		if strings.HasSuffix(path, "SHA256SUMS") { // una sesión acaba y su congelada se borra
			m.d.mu.Lock()
			m.d.maquinas = m.d.maquinas[:2]
			m.d.mu.Unlock()
		}
	}
	m.svc.alArrancar = func(v string) error {
		if v == "v1.1.0" {
			m.d.mu.Lock()
			m.d.maquinas = []*api.Machine{{ID: "aaaa", State: api.StateRunning}}
			m.d.mu.Unlock()
		}
		return nil
	}
	if _, err := Actualizar(context.Background(), m.opciones()); err != nil {
		t.Fatalf("%v\n%s", err, m.out.String())
	}
	if leer(t, m.bin) != string(m.nuevo) {
		t.Error("rolled back for nothing")
	}
}

// servicioConCtx es systemd de verdad en lo que importa aquí: sus órdenes van
// con exec.CommandContext y no corren con un contexto cancelado.
type servicioConCtx struct{ *servicioFalso }

func (s servicioConCtx) Parar(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "true").Run(); err != nil {
		s.llamadas = append(s.llamadas, "stop failed")
		return err
	}
	return s.servicioFalso.Parar(ctx)
}

func (s servicioConCtx) Arrancar(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "true").Run(); err != nil {
		s.llamadas = append(s.llamadas, "start failed")
		return err
	}
	return s.servicioFalso.Arrancar(ctx)
}

// Un Ctrl-C mientras se espera al daemon nuevo vuelve atrás entera: parar el
// nuevo, devolver los binarios y arrancar el viejo, aunque el contexto de
// quien llama ya esté cancelado.
func TestSeñalDuranteLaVerificacionVuelveAtras(t *testing.T) {
	m := nuevoMontaje(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.d.mudo = "v1.1.0"
	m.d.alPreguntar = func(v string) {
		if v == "v1.1.0" {
			cancel()
		}
	}
	o := m.opciones()
	o.Servicio = servicioConCtx{m.svc}
	_, err := Actualizar(ctx, o)
	var va *ErrVueltaAtras
	if !errors.As(err, &va) || va.Fallo != nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v, want a clean rollback after the interruption\n%s", err, m.out.String())
	}
	if got := strings.Join(m.svc.llamadas, ","); got != "stop,start,stop,start" {
		t.Errorf("service calls %s, want stop,start,stop,start", got)
	}
	if leer(t, m.bin) != string(m.viejo) || m.d.version != "v1.0.0" || m.d.caido {
		t.Errorf("not back on v1.0.0: daemon %s down=%v", m.d.version, m.d.caido)
	}
}

// Un dorado de v0.4 que el kling nuevo ya no lee se dice antes de parar nada,
// con el mismo mensaje que daría el daemon, y no después como "template is
// gone". Lo mismo un state.json de v0.13 y un links.json sin migrar.
func TestEstadoObsoletoNoParaNada(t *testing.T) {
	for nombre, prep := range map[string]func(m *montaje){
		"meta.json de v0.4": func(m *montaje) {
			meta := filepath.Join(m.raiz, "snapshots", "pg", "meta.json")
			os.MkdirAll(filepath.Dir(meta), 0o700)
			os.WriteFile(meta, []byte(`{"name":"pg","tools":[{"name":"q"}]}`), 0o600)
		},
		"state.json de v0.13": func(m *montaje) {
			os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(`[{"id":"aaaa","state":"warm"}]`), 0o600)
		},
		"links.json de v0.4": func(m *montaje) {
			os.WriteFile(filepath.Join(m.raiz, "links.json"), []byte(`[]`), 0o600)
		},
	} {
		t.Run(nombre, func(t *testing.T) {
			m := nuevoMontaje(t)
			prep(m)
			_, err := Actualizar(context.Background(), m.opciones())
			if err == nil || !strings.Contains(err.Error(), "cannot use this daemon's data") || !strings.Contains(err.Error(), "v0.17") {
				t.Fatalf("err = %v", err)
			}
			m.noTocado(t)
		})
	}
	// Un destino anterior a `upgrade -schemas` (v0.17) aún los migra.
	m := nuevoMontaje(t)
	m.rel.assets["kling-linux-amd64"] = klingFalso("v1.1.0", "")
	m.nuevo = m.rel.assets["kling-linux-amd64"]
	os.WriteFile(filepath.Join(m.raiz, "links.json"), []byte(`[]`), 0o600)
	if _, err := Actualizar(context.Background(), m.opciones()); err != nil {
		t.Fatalf("to a v0.17-like kling: %v", err)
	}
}

// Dos intentos fallidos seguidos no se llevan la copia del último bueno:
// -rollback sigue volviendo a la versión de antes de él.
func TestFallosNoBorranLaCopiaBuena(t *testing.T) {
	m := nuevoMontaje(t)
	o := m.opciones()
	if _, err := Actualizar(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	m.svc.binario = m.bin
	m.rel.etiqueta = "v1.2.0"
	m.rel.assets["kling-linux-amd64"] = klingFalso("v1.2.0", esquemasHoy)
	m.svc.alArrancar = func(v string) error {
		if v == "v1.2.0" {
			return errors.New("exit status 1")
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		var va *ErrVueltaAtras
		if _, err := Actualizar(context.Background(), o); !errors.As(err, &va) || va.Fallo != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if m.d.version != "v1.1.0" {
		t.Fatalf("daemon at %s after the failed attempts", m.d.version)
	}
	if _, err := VolverAtras(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if m.d.version != "v1.0.0" || leer(t, m.bin) != string(m.viejo) {
		t.Errorf("-rollback went to %s, want v1.0.0", m.d.version)
	}
}

// Una release por https que redirige un asset a http se rechaza: lo que
// cualquiera en el camino puede cambiar no se baja, venga de donde venga.
func TestRedireccionAHTTPSeRechaza(t *testing.T) {
	plano := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("binario cambiado"))
	}))
	defer plano.Close()
	cuerpo := []byte("binario")
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			fmt.Fprintf(w, "%s  kling-linux-amd64\n", sha(cuerpo))
		case strings.HasSuffix(r.URL.Path, "/kling-linux-amd64"):
			http.Redirect(w, r, plano.URL+"/kling-linux-amd64", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer tls.Close()
	f := &Fuente{Repo: tls.URL, Client: tls.Client()}
	_, err := f.Bajar(context.Background(), "v1.1.0", []Asset{{Nombre: "kling-linux-amd64"}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "only downloaded over https") {
		t.Fatalf("err = %v, want the https refusal", err)
	}
}

// despertarYPerder es un daemon nuevo que despierta la congelada aaaa (un
// cliente la pidió en cuanto contestó) y pierde el dorado pg: verificar falla
// con la congelada ya corriendo y escribiendo en su disco.
func (m *montaje) despertarYPerder() {
	m.svc.alArrancar = func(v string) error {
		if v == "v1.1.0" {
			m.d.mu.Lock()
			m.d.maquinas = []*api.Machine{{ID: "aaaa", State: api.StateRunning}, {ID: "bbbb", State: api.StateRunning}}
			m.d.dorados = nil
			m.d.mu.Unlock()
		}
		return nil
	}
	m.d.alCongelar = func(id string) error {
		m.svc.llamadas = append(m.svc.llamadas, "freeze "+id)
		return nil
	}
}

// Volver atrás deja el state.json de antes, donde aaaa está congelada: antes
// de parar se congela otra vez con el daemon nuevo, o el viejo la despertaría
// con un mem.file más viejo que su disco.
func TestVueltaAtrasRecongelaLasQueDesperto(t *testing.T) {
	m := nuevoMontaje(t)
	m.despertarYPerder()
	_, err := Actualizar(context.Background(), m.opciones())
	var va *ErrVueltaAtras
	if !errors.As(err, &va) || va.Fallo != nil {
		t.Fatalf("err = %v\n%s", err, m.out.String())
	}
	if got := strings.Join(m.svc.llamadas, ","); got != "stop,start,freeze aaaa,stop,start" {
		t.Errorf("calls %s, want aaaa frozen again before stopping the new daemon", got)
	}
	if m.d.version != "v1.0.0" {
		t.Errorf("daemon at %s", m.d.version)
	}
}

// Si no se puede congelar otra vez, no se vuelve atrás (salvo -force): el
// nuevo sigue corriendo y se dice cuál, y la copia se queda para -rollback.
func TestVueltaAtrasSinRecongelarSeNiega(t *testing.T) {
	m := nuevoMontaje(t)
	m.despertarYPerder()
	m.d.alCongelar = func(string) error { return errors.New("it has secrets") }
	_, err := Actualizar(context.Background(), m.opciones())
	var va *ErrVueltaAtras
	if !errors.As(err, &va) || va.Fallo == nil || !strings.Contains(err.Error(), "not rolled back") || !strings.Contains(err.Error(), "aaaa") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(m.svc.llamadas, ","); got != "stop,start" || m.d.version != "v1.1.0" {
		t.Errorf("calls %s, daemon %s: the new one must keep running", got, m.d.version)
	}
	if _, err := os.Stat(va.Copia); err != nil {
		t.Error("the backup must stay for -rollback")
	}

	m = nuevoMontaje(t)
	m.despertarYPerder()
	m.d.alCongelar = func(string) error { return errors.New("it has secrets") }
	o := m.opciones()
	o.Forzar = true
	if _, err := Actualizar(context.Background(), o); !errors.As(err, &va) || va.Fallo != nil {
		t.Fatalf("-force: %v", err)
	}
	if m.d.version != "v1.0.0" {
		t.Errorf("-force did not roll back: %s", m.d.version)
	}
}

// -rollback hace lo mismo con la copia de la migración (state.json.v0.bak):
// congela otra vez lo que el nuevo despertó, y si el daemon no contesta y su
// state.json dice que corre, se niega sin tocar nada.
func TestVolverAtrasRecongela(t *testing.T) {
	prep := func(t *testing.T) (*montaje, Opciones) {
		m := nuevoMontaje(t)
		m.svc.alArrancar = func(v string) error {
			if v == "v1.1.0" {
				os.WriteFile(filepath.Join(m.raiz, "state.json.v0.bak"), []byte(estadoV0), 0o600)
				os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(`{"schema":1,"machines":[{"id":"aaaa","state":"running"}]}`), 0o600)
			}
			return nil
		}
		o := m.opciones()
		if _, err := Actualizar(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		m.d.maquinas[0].State = api.StateRunning
		m.d.alCongelar = func(id string) error {
			m.svc.llamadas = append(m.svc.llamadas, "freeze "+id)
			return nil
		}
		m.svc.llamadas = nil
		return m, o
	}
	m, o := prep(t)
	if _, err := VolverAtras(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(m.svc.llamadas, ","); got != "freeze aaaa,stop,start" {
		t.Errorf("calls %s", got)
	}

	m, o = prep(t)
	m.d.caido = true
	_, err := VolverAtras(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "aaaa") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	if len(m.svc.llamadas) != 0 || leer(t, m.bin) != string(m.nuevo) {
		t.Errorf("something changed: %v", m.svc.llamadas)
	}
	o.Forzar = true
	if _, err := VolverAtras(context.Background(), o); err != nil {
		t.Fatalf("-force: %v", err)
	}
	if leer(t, m.bin) != string(m.viejo) {
		t.Error("-force did not roll back")
	}
}

// -rollback con el daemon caído (el nuevo no arranca tras un reinicio): se
// para y arranca por su servicio y se espera al viejo, que contesta solo
// después de arrancarlo.
func TestVolverAtrasConElDaemonCaido(t *testing.T) {
	m := nuevoMontaje(t)
	o := m.opciones()
	if _, err := Actualizar(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	m.d.caido = true
	m.svc.llamadas = nil
	if _, err := VolverAtras(context.Background(), o); err != nil {
		t.Fatalf("%v\n%s", err, m.out.String())
	}
	if m.d.version != "v1.0.0" || m.d.caido || leer(t, m.bin) != string(m.viejo) {
		t.Errorf("daemon %s down=%v", m.d.version, m.d.caido)
	}
	if got := strings.Join(m.svc.llamadas, ","); got != "stop,start" {
		t.Errorf("calls %s", got)
	}
}

// La copia del state.json se toma ya parado el daemon: lo que el viejo
// escribió al apagarse (o mientras se bajaba la release) vuelve con él.
func TestLaCopiaDelEstadoEsDespuesDeParar(t *testing.T) {
	m := nuevoMontaje(t)
	ultimo := `[{"id":"aaaa","state":"frozen"},{"id":"cccc","state":"frozen"}]`
	m.svc.alParar = func(v string) {
		if v == "v1.0.0" {
			os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(ultimo), 0o600)
		}
	}
	m.svc.alArrancar = func(v string) error {
		if v == "v1.1.0" {
			os.WriteFile(filepath.Join(m.raiz, "state.json"), []byte(`{"schema":1,"machines":[]}`), 0o600)
			return errors.New("exit status 1")
		}
		return nil
	}
	_, err := Actualizar(context.Background(), m.opciones())
	var va *ErrVueltaAtras
	if !errors.As(err, &va) || va.Fallo != nil {
		t.Fatalf("err = %v", err)
	}
	if got := leer(t, filepath.Join(m.raiz, "state.json")); got != ultimo {
		t.Errorf("state.json = %s, want what the old daemon wrote when it stopped", got)
	}
}

// Dos upgrades a la vez sobre la misma raíz: el segundo no toca nada.
func TestDosUpgradesALaVez(t *testing.T) {
	m := nuevoMontaje(t)
	o := m.opciones()
	soltar, err := bloquear(o.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Actualizar(context.Background(), o); err == nil || !strings.Contains(err.Error(), "another kling upgrade") {
		t.Fatalf("err = %v", err)
	}
	m.noTocado(t)
	if _, err := VolverAtras(context.Background(), o); err == nil || !strings.Contains(err.Error(), "another kling upgrade") {
		t.Fatalf("rollback: %v", err)
	}
	soltar()
	if _, err := Actualizar(context.Background(), o); err != nil {
		t.Fatalf("after the other one finished: %v", err)
	}
}
