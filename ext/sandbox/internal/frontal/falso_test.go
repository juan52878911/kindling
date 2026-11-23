package frontal

// Daemon falso para los tests: implementa, sobre un socket Unix, la parte del
// API de kindling que usa el frontal. Sin KVM, sin microVMs: máquinas en un
// mapa y respuestas guionizadas.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

type falso struct {
	t   *testing.T
	srv *httptest.Server
	// endpoint es la ruta del socket Unix.
	endpoint string

	mu        sync.Mutex
	trazas    []string // Traceparent de las peticiones recibidas
	maquinas  map[string]*api.Machine
	snapshots []string
	// snapAnot son las anotaciones de cada snapshot (por nombre), donde vive la
	// receta que plantilla.Construir cuelga de AnotacionReceta.
	snapAnot map[string]map[string]json.RawMessage
	// snapMem y snapHecho son la memoria y el instante de guardado de cada
	// snapshot (por nombre). Restaurar sin mem_mib da la del snapshot.
	snapMem   map[string]int
	snapHecho map[string]time.Time
	imagenes  []string
	ficheros  map[string][]byte
	libreMiB  int64

	// crearCodigo hace fallar POST /sandboxes con ese estado (507, 409...).
	crearCodigo int
	// crearErrTSC hace que restaurar DESDE UN SNAPSHOT (req.From != "") falle
	// con el mensaje real que un host reiniciado deja ver al restaurar, y que
	// api.EsFalloTSC reconoce. Un commit (lo que hace plantilla.Construir al
	// terminar) lo apaga: es justo lo que arregla el fallo simulado.
	crearErrTSC bool
	// execLibera: el exec falso manda la primera salida, espera a que se cierre
	// este canal y entonces manda el exit. Es lo que permite comprobar que el
	// frontal no acumula el flujo.
	execLibera chan struct{}
	ultimoExec api.ExecRequest
	// ultimoShell es el cuerpo del último POST shell; sesionesEstrictas hace
	// que un exec con una sesión que el daemon no tiene conteste 404, como el real.
	ultimoShell       api.ShellRequest
	sesionesEstrictas bool
	// procs es el estado del daemon falso para procesos y sesiones.
	procs procsFalsos
	// shellIlegal: la shell falsa manda al cliente una trama que no le toca.
	shellIlegal bool
	// creadas cuenta los POST /sandboxes aceptados.
	creadas int
	// intentos cuenta los POST /sandboxes recibidos; crearFallaDesde > 0 hace
	// fallar (500) los que lleguen del N-ésimo en adelante. peticiones guarda
	// lo recibido.
	intentos        int
	crearFallaDesde int
	peticiones      []api.SandboxRequest
	// volumenes del host, por nombre; clonar los copia con las etiquetas que
	// el núcleo real NO copia (ninguna).
	volumenes map[string]*api.Volume
	// renewFalla hace fallar POST /sandboxes/{ref}/renew con un 500.
	renewFalla bool
	// renewEmpezado, si no es nil, se cierra en el siguiente renew, que se
	// queda sin contestar hasta que el frontal cancele la llamada.
	renewEmpezado chan struct{}

	// grafos por id, y el store (espacio -> clave -> valor).
	grafos map[string]*api.Graph
	store  map[string]map[string]json.RawMessage
	// grafoUpCodigo hace fallar POST /graphs con ese estado; thawFalla, POST
	// /graphs/{ref}/thaw.
	grafoUpCodigo int
	thawFalla     bool
	freezeFalla   bool
	// thawEspera, si no es nil, para el SIGUIENTE thaw (solo ese) hasta que se
	// cierre, fuera de f.mu; antes cierra thawEmpezado.
	thawEspera   chan struct{}
	thawEmpezado chan struct{}
	// grafosUp cuenta los POST /graphs aceptados.
	grafosUp int
	// sandboxesEspera, si no es nil, para todo GET /sandboxes hasta que se
	// cierre (o la petición se cancele); el primero cierra sandboxesEmpezado.
	// Es un host colgado. Con sandboxesColgarN > 0 solo para la N-ésima (las
	// demás contestan): una petición que cuenta la cuota antes de reclamar
	// lista dos veces, y lo que se quiere colgar es la segunda.
	sandboxesEspera   chan struct{}
	sandboxesEmpezado chan struct{}
	sandboxesUnaVez   sync.Once
	sandboxesColgarN  int
	sandboxesLlamadas int
	// sandboxesFoto hace que el GET /sandboxes retenido conteste con lo que
	// había al RECIBIRLO y no al soltarlo: un listado lento cuyo contenido ya
	// es viejo cuando llega.
	sandboxesFoto bool
	// etiquetasDemora retrasa cada PUT labels antes de aplicarlo: ensancha la
	// ventana entre leer y escribir de dos réplicas del frontal.
	etiquetasDemora time.Duration
	// snapLista y snapUno cuentan GET /snapshots y GET /snapshots/{name};
	// snapUnoViejo hace que la segunda ruta no exista (un daemon anterior).
	snapLista, snapUno int
	snapUnoViejo       bool
	// sandboxesFalla hace fallar GET /sandboxes con un 500: un host que
	// contesta a su sonda de estado pero no a la lista.
	sandboxesFalla bool
	// etiquetasEspera, si no es nil, para el PRIMER PUT labels (antes de
	// aplicarlo) hasta que se cierre; antes cierra etiquetasEmpezado.
	etiquetasEspera   chan struct{}
	etiquetasEmpezado chan struct{}
	etiquetasUnaVez   sync.Once
	// etiquetasLentas retrasa cada PUT labels que pone un tenant.
	etiquetasLentas time.Duration

	// conReclamo anuncia api.CapabilityClaim y sirve POST /sandboxes/claim.
	// Sin él, el daemon es uno viejo: el frontal tiene que usar las dos fases.
	conReclamo bool
	// reclamoLlamadas cuenta los POST /sandboxes/claim; etiquetasLlamadas,
	// renovaciones y lecturas, los PUT labels, renew y GET /machines/{ref}.
	reclamoLlamadas, etiquetasLlamadas, renovaciones, lecturas int
	// reclamoReq guarda lo que pidió cada reclamo.
	reclamoReq []api.ClaimRequest
	// reclamoCorta hace que el daemon etiquete y renueve la candidata pero
	// corte la conexión sin contestar; reclamoPlano404 contesta con un 404 sin
	// código (un daemon que ya no tiene la ruta); borrados cuenta los DELETE.
	reclamoCorta, reclamoPlano404 bool
	borrados                      []string

	// execCorta hace que exec mande un evento y corte la conexión.
	execCorta bool
	// ultimoTar son los parámetros del último PUT files?format=tar.
	ultimoTar url.Values
	// tamanoStat, si no es 0, es el tamaño que anuncia el stat de un fichero
	// (para probar topes sin guardar cientos de MiB).
	tamanoStat int64
	// dirs son los directorios que el falso conoce (clave ref:ruta).
	dirs map[string]bool

	// ing es el estado de la entrada a puertos (ingress_test.go).
	ing ingresoFalso
}

// nuevoFalso arranca un daemon falso en un socket Unix corto (macOS limita
// la ruta de un socket a 104 bytes y t.TempDir() puede pasarse).
func nuevoFalso(t *testing.T) *falso {
	t.Helper()
	dir, err := os.MkdirTemp("", "fr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	f := &falso{t: t, endpoint: sock, maquinas: map[string]*api.Machine{},
		ficheros: map[string][]byte{}, libreMiB: 4096, imagenes: []string{"base"}}
	rutas := f.rutas()
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tp := r.Header.Get("Traceparent"); tp != "" {
			f.mu.Lock()
			f.trazas = append(f.trazas, tp)
			f.mu.Unlock()
		}
		rutas.ServeHTTP(w, r)
	}))
	f.srv.Listener = ln
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *falso) rutas() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n := len(f.maquinas)
		var caps []string
		if f.conReclamo {
			caps = []string{api.CapabilityClaim}
		}
		f.mu.Unlock()
		writeJSON(w, 200, api.Info{Version: "fake", Machines: n, Capabilities: caps})
	})
	m.HandleFunc("POST /sandboxes/claim", f.reclamar)
	m.HandleFunc("GET /procstats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.ProcStats{AvailableMiB: f.libreMiB})
	})
	m.HandleFunc("GET /snapshots", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.snapLista++
		out := []*api.Snapshot{}
		for _, n := range f.snapshots {
			out = append(out, &api.Snapshot{Name: n, AllowExec: true, Annotations: f.snapAnot[n],
				MemMiB: f.snapMem[n], CreatedAt: f.snapHecho[n]})
		}
		f.mu.Unlock()
		writeJSON(w, 200, out)
	})
	m.HandleFunc("GET /snapshots/{name}", func(w http.ResponseWriter, r *http.Request) {
		nombre := r.PathValue("name")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.snapUno++
		if f.snapUnoViejo {
			http.NotFound(w, r)
			return
		}
		hay := false
		for _, s := range f.snapshots {
			hay = hay || s == nombre
		}
		if !hay {
			failDaemon(w, 404, fmt.Errorf("no snapshot %q", nombre))
			return
		}
		writeJSON(w, 200, &api.Snapshot{Name: nombre, AllowExec: true, Annotations: f.snapAnot[nombre],
			MemMiB: f.snapMem[nombre], CreatedAt: f.snapHecho[nombre]})
	})
	m.HandleFunc("PUT /snapshots/{name}/annotations/{key}", func(w http.ResponseWriter, r *http.Request) {
		var valor json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&valor); err != nil {
			failDaemon(w, 400, err)
			return
		}
		nombre, clave := r.PathValue("name"), r.PathValue("key")
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.snapAnot == nil {
			f.snapAnot = map[string]map[string]json.RawMessage{}
		}
		if f.snapAnot[nombre] == nil {
			f.snapAnot[nombre] = map[string]json.RawMessage{}
		}
		f.snapAnot[nombre][clave] = valor
		writeJSON(w, 200, &api.Snapshot{Name: nombre, AllowExec: true, Annotations: f.snapAnot[nombre],
			MemMiB: f.snapMem[nombre], CreatedAt: f.snapHecho[nombre]})
	})
	// POST /machines y POST /machines/{ref}/commit: lo que plantilla.Construir
	// necesita para rehacer un dorado (Run, un exec que ya sirve /machines/{ref}/exec,
	// y Commit).
	m.HandleFunc("POST /machines", func(w http.ResponseWriter, r *http.Request) {
		var req api.RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			failDaemon(w, 400, err)
			return
		}
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		id := "build-" + hex.EncodeToString(b)
		mc := &api.Machine{ID: id, Name: id, Image: req.Image, State: api.StateRunning,
			AllowExec: req.AllowExec, IP: "10.0.0.3"}
		f.mu.Lock()
		f.maquinas[mc.ID] = mc
		f.mu.Unlock()
		c := *mc
		writeJSON(w, 201, &c)
	})
	m.HandleFunc("POST /machines/{ref}/commit", func(w http.ResponseWriter, r *http.Request) {
		var req api.CommitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			failDaemon(w, 400, err)
			return
		}
		f.mu.Lock()
		ya := false
		for _, s := range f.snapshots {
			ya = ya || s == req.Name
		}
		if !ya {
			f.snapshots = append(f.snapshots, req.Name)
		}
		if f.snapAnot == nil {
			f.snapAnot = map[string]map[string]json.RawMessage{}
		}
		// Un replace es un dorado nuevo, aunque conserve el nombre: se limpia lo
		// que hubiera hasta que Construir cuelgue la receta de nuevo.
		f.snapAnot[req.Name] = map[string]json.RawMessage{}
		// Esto es justo lo que arregla el fallo de TSC simulado.
		f.crearErrTSC = false
		f.mu.Unlock()
		writeJSON(w, 200, &api.Snapshot{Name: req.Name, AllowExec: true, CreatedAt: time.Now().UTC()})
	})
	m.HandleFunc("GET /images", func(w http.ResponseWriter, r *http.Request) {
		out := []api.Image{}
		for _, n := range f.imagenes {
			out = append(out, api.Image{Name: n})
		}
		writeJSON(w, 200, out)
	})
	m.HandleFunc("POST /sandboxes", f.crear)
	m.HandleFunc("GET /volumes", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		out := []*api.Volume{}
		for _, v := range f.volumenes {
			c := *v
			out = append(out, &c)
		}
		f.mu.Unlock()
		writeJSON(w, 200, out)
	})
	m.HandleFunc("POST /volumes", func(w http.ResponseWriter, r *http.Request) {
		var req api.CreateVolumeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			failDaemon(w, 400, err)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.volumenes[req.Name] != nil {
			failDaemon(w, 409, fmt.Errorf("exists"))
			return
		}
		v := &api.Volume{Name: req.Name, SizeBytes: int64(req.SizeMiB) << 20, Labels: req.Labels}
		f.sembrarVolumenLocked(v)
		writeJSON(w, 201, v)
	})
	m.HandleFunc("POST /volumes/{name}/clone", func(w http.ResponseWriter, r *http.Request) {
		var req api.CloneVolumeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			failDaemon(w, 400, err)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		src := f.volumenes[r.PathValue("name")]
		if src == nil {
			failDaemon(w, 404, fmt.Errorf("no volume"))
			return
		}
		v := &api.Volume{Name: req.To, SizeBytes: src.SizeBytes}
		f.sembrarVolumenLocked(v)
		writeJSON(w, 201, &api.CloneVolumeResult{Volume: v})
	})
	m.HandleFunc("DELETE /volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		v := f.volumenes[r.PathValue("name")]
		if v == nil {
			failDaemon(w, 404, fmt.Errorf("no volume"))
			return
		}
		if len(v.UsedBy) > 0 {
			failDaemon(w, 409, fmt.Errorf("volume in use"))
			return
		}
		delete(f.volumenes, v.Name)
		w.WriteHeader(204)
	})
	m.HandleFunc("GET /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sandboxesLlamadas++
		colgar := f.sandboxesEspera != nil && (f.sandboxesColgarN == 0 || f.sandboxesLlamadas == f.sandboxesColgarN)
		falla := f.sandboxesFalla
		f.mu.Unlock()
		if falla {
			failDaemon(w, 500, fmt.Errorf("listing timed out"))
			return
		}
		var foto []*api.Machine
		if colgar && f.sandboxesFoto {
			f.mu.Lock()
			foto = []*api.Machine{}
			for _, mc := range f.maquinas {
				if mc.Labels[api.LabelKind] == api.KindSandbox {
					c := *mc
					c.Labels = api.MergeLabels(nil, mc.Labels)
					foto = append(foto, &c)
				}
			}
			f.mu.Unlock()
		}
		if colgar {
			f.sandboxesUnaVez.Do(func() { close(f.sandboxesEmpezado) })
			select {
			case <-f.sandboxesEspera:
			case <-r.Context().Done():
				return
			}
		}
		if foto != nil {
			writeJSON(w, 200, foto)
			return
		}
		f.mu.Lock()
		out := []*api.Machine{}
		for _, mc := range f.maquinas {
			if mc.Labels[api.LabelKind] == api.KindSandbox {
				c := *mc
				out = append(out, &c)
			}
		}
		f.mu.Unlock()
		writeJSON(w, 200, out)
	})
	m.HandleFunc("POST /sandboxes/{ref}/renew", func(w http.ResponseWriter, r *http.Request) {
		var req api.RenewRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.renovaciones++
		f.mu.Unlock()
		if f.renewEmpezado != nil {
			// Avisa y no contesta hasta que el frontal se rinda.
			close(f.renewEmpezado)
			f.renewEmpezado = nil
			<-r.Context().Done()
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.renewFalla {
			failDaemon(w, 500, fmt.Errorf("renew failed"))
			return
		}
		mc, ok := f.maquinas[r.PathValue("ref")]
		if !ok {
			failDaemon(w, 404, fmt.Errorf("no sandbox"))
			return
		}
		ahora := time.Now()
		mc.TTLAt, mc.TTLSeconds = &ahora, req.TTLSeconds
		c := *mc
		writeJSON(w, 200, &c)
	})
	m.HandleFunc("DELETE /sandboxes/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.maquinas[r.PathValue("ref")]; !ok {
			failDaemon(w, 404, fmt.Errorf("no sandbox"))
			return
		}
		f.borrados = append(f.borrados, r.PathValue("ref"))
		delete(f.maquinas, r.PathValue("ref"))
		w.WriteHeader(204)
	})
	m.HandleFunc("GET /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lecturas++
		mc, ok := f.maquinas[r.PathValue("ref")]
		if !ok {
			failDaemon(w, 404, fmt.Errorf("no machine"))
			return
		}
		c := *mc
		c.Labels = api.MergeLabels(nil, mc.Labels)
		writeJSON(w, 200, &c)
	})
	m.HandleFunc("PUT /machines/{ref}/labels", func(w http.ResponseWriter, r *http.Request) {
		var labels map[string]string
		_ = json.NewDecoder(r.Body).Decode(&labels)
		f.mu.Lock()
		f.etiquetasLlamadas++
		f.mu.Unlock()
		if f.etiquetasEspera != nil {
			// Solo la PRIMERA espera: es un frontal lento frente a otro.
			primera := false
			f.etiquetasUnaVez.Do(func() { primera = true; close(f.etiquetasEmpezado) })
			if primera {
				<-f.etiquetasEspera
			}
		}
		if f.etiquetasDemora > 0 {
			time.Sleep(f.etiquetasDemora)
		}
		if _, conTenant := labels[LabelTenant]; conTenant && f.etiquetasLentas > 0 {
			time.Sleep(f.etiquetasLentas)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		mc, ok := f.maquinas[r.PathValue("ref")]
		if !ok {
			failDaemon(w, 404, fmt.Errorf("no machine"))
			return
		}
		mc.Labels = api.MergeLabels(mc.Labels, labels)
		w.WriteHeader(204)
	})
	f.rutasGrafos(m)
	f.rutasIngreso(m)
	f.rutasProcesos(m)
	m.HandleFunc("POST /machines/{ref}/exec", f.exec)
	m.HandleFunc("POST /machines/{ref}/shell", f.shell)
	m.HandleFunc("/machines/{ref}/files", f.files)
	return m
}

func (f *falso) existe(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.maquinas[ref]
	return ok
}

func (f *falso) crear(w http.ResponseWriter, r *http.Request) {
	var req api.SandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		failDaemon(w, 400, err)
		return
	}
	f.mu.Lock()
	tsc := f.crearErrTSC
	f.intentos++
	f.peticiones = append(f.peticiones, req)
	falla := f.crearFallaDesde > 0 && f.intentos >= f.crearFallaDesde
	f.mu.Unlock()
	if falla {
		failDaemon(w, 500, fmt.Errorf("boom"))
		return
	}
	if req.From != "" && tsc {
		// El mensaje real que deja ver explainRestoreErr del núcleo cuando la
		// causa es un reinicio del host: api.EsFalloTSC lo reconoce por el
		// token "TSC", y va en un 500 porque el daemon no tiene un código propio
		// para esto (es la traducción, no un StatusError).
		failDaemon(w, 500, fmt.Errorf("Could not set TSC scaling within the snapshot: Invalid argument (os error 22)"))
		return
	}
	if f.crearCodigo != 0 {
		msg := "no room"
		if f.crearCodigo == api.StatusMachineLimit {
			msg = "machine limit reached"
		}
		failDaemon(w, f.crearCodigo, fmt.Errorf("%s", msg))
		return
	}
	if req.From != "" {
		ok := false
		for _, s := range f.snapshots {
			ok = ok || s == req.From
		}
		if !ok {
			failDaemon(w, 404, fmt.Errorf("no snapshot %q", req.From))
			return
		}
	}
	labels := map[string]string{}
	for k, v := range req.Labels {
		labels[k] = v
	}
	labels[api.LabelKind] = api.KindSandbox
	if req.From != "" && req.MemMiB == 0 {
		f.mu.Lock()
		req.MemMiB = f.snapMem[req.From]
		f.mu.Unlock()
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	ahora := time.Now()
	mc := &api.Machine{
		ID: hex.EncodeToString(b), Name: "sbx-" + hex.EncodeToString(b[:2]), Image: req.Image, From: req.From,
		State: api.StateRunning, VCPUs: req.VCPUs, MemMiB: req.MemMiB, Egress: req.Egress,
		AllowDomains: req.AllowDomains, TTLSeconds: req.TTLSeconds, OnTTL: req.OnTTL,
		AllowExec: true, CreatedAt: ahora, StartedAt: &ahora, TTLAt: &ahora, Labels: labels, IP: "10.0.0.2",
	}
	if mc.OnTTL == api.OnTTLFreeze {
		mc.OnTTL = ""
	}
	f.mu.Lock()
	f.maquinas[mc.ID] = mc
	f.creadas++
	f.mu.Unlock()
	c := *mc
	writeJSON(w, 201, &c)
}

// sembrar mete una máquina a mano, como si la hubiera creado otro.
// reclamar es POST /sandboxes/claim: elige, etiqueta y renueva bajo f.mu, como
// el daemon real (machine.Reclamar).
func (f *falso) reclamar(w http.ResponseWriter, r *http.Request) {
	var req api.ClaimRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reclamoLlamadas++
	f.reclamoReq = append(f.reclamoReq, req)
	if f.reclamoPlano404 {
		http.NotFound(w, r)
		return
	}
	if len(req.Match) == 0 {
		failDaemon(w, 400, fmt.Errorf("match can't be empty"))
		return
	}
	var elegida *api.Machine
	for _, mc := range f.maquinas {
		if mc.State != api.StateRunning || !reclamableFalso(mc.Labels, req) {
			continue
		}
		elegida = mc
		break
	}
	if elegida == nil {
		writeJSON(w, 404, api.Error{Message: "no claimable machine matches the request", Code: api.CodeNoClaimable})
		return
	}
	elegida.Labels = api.MergeLabels(elegida.Labels, req.Labels)
	if req.TTLSeconds > 0 {
		elegida.TTLSeconds = req.TTLSeconds
	}
	ahora := time.Now()
	elegida.TTLAt = &ahora
	if f.reclamoCorta {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
		return
	}
	c := *elegida
	c.Labels = api.MergeLabels(nil, elegida.Labels)
	writeJSON(w, 200, &c)
}

func reclamableFalso(labels map[string]string, req api.ClaimRequest) bool {
	for k, v := range req.Match {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	for _, k := range req.Absent {
		if _, ok := labels[k]; ok {
			return false
		}
	}
	for _, k := range [...]string{api.LabelOwner, api.LabelClaim} {
		if _, ok := labels[k]; ok {
			return false
		}
	}
	return true
}

func (f *falso) sembrar(mc *api.Machine) *api.Machine {
	f.mu.Lock()
	defer f.mu.Unlock()
	if mc.ID == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		mc.ID = hex.EncodeToString(b)
	}
	if mc.State == "" {
		mc.State = api.StateRunning
	}
	if mc.Labels == nil {
		mc.Labels = map[string]string{}
	}
	mc.Labels[api.LabelKind] = api.KindSandbox
	if mc.CreatedAt.IsZero() {
		mc.CreatedAt = time.Now()
	}
	f.maquinas[mc.ID] = mc
	return mc
}

func (f *falso) maquina(id string) *api.Machine {
	f.mu.Lock()
	defer f.mu.Unlock()
	if mc, ok := f.maquinas[id]; ok {
		c := *mc
		return &c
	}
	return nil
}

func (f *falso) exec(w http.ResponseWriter, r *http.Request) {
	if !f.existe(r.PathValue("ref")) {
		failDaemon(w, 404, fmt.Errorf("no machine"))
		return
	}
	var req api.ExecRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.ultimoExec = req
	libera := f.execLibera
	estricto := f.sesionesEstrictas
	f.mu.Unlock()
	if estricto && req.Session != "" {
		f.procs.mu.Lock()
		_, hay := f.procs.sesiones[req.Session]
		f.procs.mu.Unlock()
		if !hay {
			failDaemon(w, 404, fmt.Errorf("no session %q (expired or never created)", req.Session))
			return
		}
	}

	salida := []byte("hola\n")
	if f.execCorta {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(api.ExecEvent{Stream: "stdout", Data: salida})
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	if r.URL.Query().Get("wait") == "1" {
		if libera != nil {
			<-libera
		}
		writeJSON(w, 200, api.ExecResult{ExitCode: 3, Stdout: salida, DurationMS: 7})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	enc := json.NewEncoder(w)
	fl := w.(http.Flusher)
	_ = enc.Encode(api.ExecEvent{Stream: "stdout", Data: salida})
	fl.Flush()
	if libera != nil {
		select {
		case <-libera:
		case <-r.Context().Done():
			return
		}
	}
	code := 3
	_ = enc.Encode(api.ExecEvent{Exit: &code, DurationMS: 7})
	fl.Flush()
}

func (f *falso) files(w http.ResponseWriter, r *http.Request) {
	if !f.existe(r.PathValue("ref")) {
		failDaemon(w, 404, fmt.Errorf("no machine"))
		return
	}
	q := r.URL.Query()
	clave := r.PathValue("ref") + ":" + q.Get("path")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if q.Get("list") == "1" {
			pref := strings.TrimRight(q.Get("path"), "/") + "/"
			lista := api.FileList{Path: q.Get("path"), Entries: []api.FileEntry{}}
			for k, v := range f.ficheros {
				if p, ok := strings.CutPrefix(k, r.PathValue("ref")+":"+pref); ok && !strings.Contains(p, "/") {
					lista.Entries = append(lista.Entries, api.FileEntry{Name: p, Type: "file", Size: int64(len(v)), Mode: "0644"})
				}
			}
			sort.Slice(lista.Entries, func(i, j int) bool { return lista.Entries[i].Name < lista.Entries[j].Name })
			writeJSON(w, 200, lista)
			return
		}
		if q.Get("format") == "tar" {
			b, ok := f.empaquetarLocked(r.PathValue("ref"), q.Get("path"))
			if !ok {
				failDaemon(w, 404, fmt.Errorf("no such directory"))
				return
			}
			w.Header().Set("Content-Type", "application/x-tar")
			_, _ = w.Write(b)
			return
		}
		b, ok := f.ficheros[clave]
		if !ok && f.dirs[clave] && q.Get("stat") == "1" {
			writeJSON(w, 200, api.FileStat{Path: q.Get("path"), Mode: "0755", IsDir: true})
			return
		}
		if !ok {
			failDaemon(w, 404, fmt.Errorf("no such file"))
			return
		}
		if q.Get("stat") == "1" {
			size := int64(len(b))
			if f.tamanoStat > 0 {
				size = f.tamanoStat
			}
			writeJSON(w, 200, api.FileStat{Path: q.Get("path"), Size: size, Mode: "0644"})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(b)
	case http.MethodPut:
		b, err := io.ReadAll(io.LimitReader(r.Body, api.FileMaxUpload+1))
		if err != nil {
			failDaemon(w, 400, err)
			return
		}
		if int64(len(b)) > api.FileMaxUpload {
			failDaemon(w, 413, fmt.Errorf("too big"))
			return
		}
		if q.Get("format") == "tar" {
			f.ultimoTar = q
			res, err := f.extraerTarLocked(r.PathValue("ref"), q.Get("path"), q.Get("mkdir") == "1", b)
			if err != nil {
				failDaemon(w, 400, err)
				return
			}
			writeJSON(w, 200, res)
			return
		}
		f.ficheros[clave] = b
		writeJSON(w, 200, api.FileStat{Path: q.Get("path"), Size: int64(len(b)), Mode: q.Get("mode")})
	case http.MethodDelete:
		delete(f.ficheros, clave)
		w.WriteHeader(204)
	}
}

// shell hace de invitado: devuelve como salida lo que le mandan, contesta a
// una señal con un exit, y si shellIlegal manda antes un resize hacia fuera.
func (f *falso) shell(w http.ResponseWriter, r *http.Request) {
	if !f.existe(r.PathValue("ref")) {
		failDaemon(w, 404, fmt.Errorf("no machine"))
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), api.ShellProto) {
		failDaemon(w, 426, fmt.Errorf("upgrade required"))
		return
	}
	// Como el daemon real: el cuerpo se consume ANTES de secuestrar, o sus
	// bytes se leerían como tramas.
	var req api.ShellRequest
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
	f.mu.Lock()
	f.ultimoShell = req
	f.mu.Unlock()
	conn, brw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + api.ShellProto + "\r\nConnection: Upgrade\r\n\r\n")
	_ = brw.Flush()
	if f.shellIlegal {
		_ = api.WriteFrame(conn, api.ShellResize, api.ResizePayload(50, 200))
		// Se queda esperando a que el frontal cuelgue.
		_, _, _ = api.ReadFrame(brw.Reader)
		return
	}
	for {
		t, p, err := api.ReadFrame(brw.Reader)
		if err != nil {
			return
		}
		switch t {
		case api.ShellData:
			_ = api.WriteFrame(conn, api.ShellData, append([]byte("eco:"), p...))
		case api.ShellResize:
			_ = api.WriteFrame(conn, api.ShellData, []byte("resized"))
		case api.ShellSignal:
			_ = api.WriteFrame(conn, api.ShellExit, api.ExitPayload(130))
			return
		}
	}
}

// ---- cliente de pruebas

// conexionShell es una sesión kling-shell/1 abierta a mano contra el frontal.
type conexionShell struct {
	conn net.Conn
	br   *bufio.Reader
}

func abrirShell(t *testing.T, srv *httptest.Server, token, id string) (*conexionShell, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sandboxes/"+id+"/shell", strings.NewReader(`{"term":"xterm"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", api.ShellProto)
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, resp
	}
	c := &conexionShell{conn: conn, br: br}
	t.Cleanup(func() { conn.Close() })
	return c, resp
}

func (c *conexionShell) leer(t *testing.T) (byte, []byte) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	tp, p, err := api.ReadFrame(c.br)
	if err != nil {
		t.Fatalf("reading a frame: %v", err)
	}
	return tp, p
}

// ---- grafos y store

func (f *falso) rutasGrafos(m *http.ServeMux) {
	m.HandleFunc("GET /machines", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		out := []*api.Machine{}
		for _, mc := range f.maquinas {
			c := *mc
			c.Labels = api.MergeLabels(nil, mc.Labels)
			out = append(out, &c)
		}
		f.mu.Unlock()
		writeJSON(w, 200, out)
	})
	m.HandleFunc("GET /store/{ns}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		keys := []string{}
		for k := range f.store[r.PathValue("ns")] {
			keys = append(keys, k)
		}
		writeJSON(w, 200, api.StoreKeys{Keys: keys})
	})
	m.HandleFunc("GET /store/{ns}/{key}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		v, ok := f.store[r.PathValue("ns")][r.PathValue("key")]
		if !ok {
			failDaemon(w, 404, fmt.Errorf("no key"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(v)
	})
	m.HandleFunc("PUT /store/{ns}/{key}", func(w http.ResponseWriter, r *http.Request) {
		var v json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			failDaemon(w, 400, err)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.store == nil {
			f.store = map[string]map[string]json.RawMessage{}
		}
		ns := r.PathValue("ns")
		if f.store[ns] == nil {
			f.store[ns] = map[string]json.RawMessage{}
		}
		f.store[ns][r.PathValue("key")] = v
		w.WriteHeader(204)
	})
	m.HandleFunc("DELETE /store/{ns}/{key}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.store[r.PathValue("ns")], r.PathValue("key"))
		w.WriteHeader(204)
	})
	m.HandleFunc("POST /graphs", func(w http.ResponseWriter, r *http.Request) {
		var req api.GraphRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			failDaemon(w, 400, err)
			return
		}
		g := req.Graph
		if err := api.ValidateGraph(&g); err != nil {
			failDaemon(w, 400, err)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.grafoUpCodigo != 0 {
			failDaemon(w, f.grafoUpCodigo, fmt.Errorf("no room for the graph"))
			return
		}
		for _, otro := range f.grafos {
			if otro.Name == g.Name {
				failDaemon(w, 409, fmt.Errorf("graph %q already exists", g.Name))
				return
			}
		}
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		g.ID = "g" + hex.EncodeToString(b)
		g.CreatedAt = time.Now().UTC()
		for nombre, n := range g.Nodes {
			nb := make([]byte, 8)
			_, _ = rand.Read(nb)
			ahora := time.Now()
			mc := &api.Machine{ID: hex.EncodeToString(nb), Name: g.Name + "-" + nombre, From: n.From, Image: n.Image,
				State: api.StateRunning, AllowExec: true, CreatedAt: ahora, StartedAt: &ahora,
				Labels: api.MergeLabels(n.Labels, map[string]string{api.LabelGraph: g.ID, api.LabelGraphNode: nombre})}
			f.maquinas[mc.ID] = mc
			n.MachineID = mc.ID
			g.Nodes[nombre] = n
		}
		if f.grafos == nil {
			f.grafos = map[string]*api.Graph{}
		}
		f.grafos[g.ID] = &g
		f.grafosUp++
		writeJSON(w, 201, f.vistaGrafoLocked(&g))
	})
	m.HandleFunc("GET /graphs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []*api.Graph{}
		for _, g := range f.grafos {
			out = append(out, f.vistaGrafoLocked(g))
		}
		writeJSON(w, 200, out)
	})
	m.HandleFunc("GET /graphs/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		g, ok := f.grafos[r.PathValue("ref")]
		if !ok {
			failDaemon(w, 404, fmt.Errorf("no graph"))
			return
		}
		writeJSON(w, 200, f.vistaGrafoLocked(g))
	})
	estado := func(nuevo api.State, falla func() bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			g, ok := f.grafos[r.PathValue("ref")]
			if !ok {
				failDaemon(w, 404, fmt.Errorf("no graph"))
				return
			}
			if falla() {
				failDaemon(w, 500, fmt.Errorf("could not change the graph state"))
				return
			}
			for _, n := range g.Nodes {
				if mc := f.maquinas[n.MachineID]; mc != nil {
					mc.State = nuevo
					if nuevo == api.StateRunning {
						ahora := time.Now()
						mc.StartedAt = &ahora
					}
				}
			}
			writeJSON(w, 200, f.vistaGrafoLocked(g))
		}
	}
	m.HandleFunc("POST /graphs/{ref}/freeze", estado(api.StateWarm, func() bool { return f.freezeFalla }))
	thaw := estado(api.StateRunning, func() bool { return f.thawFalla })
	m.HandleFunc("POST /graphs/{ref}/thaw", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		espera, empezado := f.thawEspera, f.thawEmpezado
		f.thawEspera, f.thawEmpezado = nil, nil
		f.mu.Unlock()
		if espera != nil {
			close(empezado)
			<-espera
		}
		thaw(w, r)
	})
	m.HandleFunc("DELETE /graphs/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		g, ok := f.grafos[r.PathValue("ref")]
		if !ok {
			failDaemon(w, 404, fmt.Errorf("no graph"))
			return
		}
		for _, n := range g.Nodes {
			delete(f.maquinas, n.MachineID)
		}
		delete(f.grafos, g.ID)
		w.WriteHeader(204)
	})
}

// vistaGrafoLocked copia el grafo con el estado de cada nodo, como el daemon.
func (f *falso) vistaGrafoLocked(g *api.Graph) *api.Graph {
	c := *g
	c.Nodes = map[string]api.GraphNode{}
	corriendo := 0
	for nombre, n := range g.Nodes {
		if mc := f.maquinas[n.MachineID]; mc != nil {
			n.State = string(mc.State)
			if mc.State == api.StateWarm {
				n.State = "frozen"
			}
			if mc.State == api.StateRunning {
				corriendo++
			}
		} else {
			n.State = "missing"
		}
		c.Nodes[nombre] = n
	}
	switch {
	case corriendo == len(g.Nodes):
		c.State = api.GraphStateRunning
	case corriendo == 0:
		c.State = api.GraphStateFrozen
	default:
		c.State = api.GraphStatePartial
	}
	return &c
}

// grafosVivos cuenta los grafos que hay en el daemon.
func (f *falso) grafosVivos() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.grafos)
}

// failDaemon contesta como el daemon: el mensaje entero, sin el saneado que
// fail aplica a los errores que van al tenant.
func failDaemon(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, api.Error{Message: err.Error()})
}

func (f *falso) sembrarVolumenLocked(v *api.Volume) {
	if f.volumenes == nil {
		f.volumenes = map[string]*api.Volume{}
	}
	f.volumenes[v.Name] = v
}

// sembrarVolumen mete un volumen a mano.
func (f *falso) sembrarVolumen(v *api.Volume) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sembrarVolumenLocked(v)
}

func (f *falso) tieneVolumen(n string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.volumenes[n] != nil
}

// extraerTarLocked extrae el tar dentro de path como lo haría el invitado:
// los ficheros pasan a f.ficheros y los directorios a f.dirs. Con mkdir crea
// también path. Rechaza entradas con ".." o absolutas.
func (f *falso) extraerTarLocked(ref, path string, mkdir bool, b []byte) (*api.TarResult, error) {
	if f.dirs == nil {
		f.dirs = map[string]bool{}
	}
	if mkdir {
		f.dirs[ref+":"+filepath.Clean(path)] = true
	} else if !f.dirs[ref+":"+filepath.Clean(path)] {
		return nil, fmt.Errorf("%s is not a directory", path)
	}
	res := &api.TarResult{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return res, nil
		}
		if err != nil {
			return nil, err
		}
		nombre := filepath.Clean(h.Name)
		if filepath.IsAbs(nombre) || nombre == ".." || strings.HasPrefix(nombre, "../") {
			return nil, fmt.Errorf("unsafe entry %q", h.Name)
		}
		dest := ref + ":" + filepath.Join(path, nombre)
		res.Entries++
		switch h.Typeflag {
		case tar.TypeDir:
			f.dirs[dest] = true
		default:
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			f.dirs[ref+":"+filepath.Dir(filepath.Join(path, nombre))] = true
			f.ficheros[dest] = data
			res.Files++
			res.Bytes += int64(len(data))
		}
	}
}

// empaquetarLocked devuelve un tar con los ficheros bajo path (nombres
// relativos a path).
func (f *falso) empaquetarLocked(ref, path string) ([]byte, bool) {
	raiz := ref + ":" + filepath.Clean(path)
	if !f.dirs[raiz] {
		return nil, false
	}
	var nombres []string
	for k := range f.ficheros {
		if rel, ok := strings.CutPrefix(k, raiz+"/"); ok {
			nombres = append(nombres, rel)
		}
	}
	sort.Strings(nombres)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range nombres {
		data := f.ficheros[raiz+"/"+n]
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(data))})
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	return buf.Bytes(), true
}
