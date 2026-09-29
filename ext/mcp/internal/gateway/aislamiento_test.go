package gateway

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
)

// daemonAislado es un daemon falso para el aislamiento por sesión: cada POST
// /machines crea una máquina con su propio invitado (el siguiente de invitados),
// y sabe consultar, congelar, descongelar y borrar. El snapshot del servicio
// lleva mcp.isolation=session salvo que compartido lo desactive.
type daemonAislado struct {
	service    string
	invitados  []string // direcciones de los invitados, en orden de creación
	compartido bool

	// alDescongelar, si trae una dirección para la máquina, es su reenvío
	// nuevo tras el thaw (como en macOS, donde restaurar cambia el puerto).
	alDescongelar map[string]string

	mu       sync.Mutex
	maquinas map[string]*api.Machine
	creadas  int
	llamadas []string
}

func (d *daemonAislado) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.llamadas = append(d.llamadas, r.Method+" "+r.URL.Path)
	responder := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/snapshots":
		s := &api.Snapshot{Name: d.service, Labels: map[string]string{api.LabelService: d.service}, Egress: "none"}
		if !d.compartido {
			b, _ := json.Marshal(mcp.IsolationSession)
			s.Annotations = map[string]json.RawMessage{mcp.IsolationKey: b}
		}
		responder([]*api.Snapshot{s})
		return
	case r.URL.Path == "/store/mcp/links":
		_, _ = w.Write([]byte("{}"))
		return
	case r.URL.Path == "/info":
		responder(api.Info{})
		return
	case r.URL.Path == "/machines" && r.Method == http.MethodGet:
		l := []*api.Machine{}
		for _, m := range d.maquinas {
			c := *m
			l = append(l, &c)
		}
		responder(l)
		return
	case r.URL.Path == "/machines" && r.Method == http.MethodPost:
		if d.creadas >= len(d.invitados) {
			http.Error(w, "no more machines in this fake", http.StatusInsufficientStorage)
			return
		}
		var req api.RunRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.creadas++
		m := &api.Machine{ID: fmt.Sprintf("m%d", d.creadas), Name: fmt.Sprintf("%s-%d", d.service, d.creadas),
			State: api.StateRunning, IP: "172.16.0.2", Labels: req.Labels, CreatedAt: time.Now(), MemMiB: 1024,
			Forwards: map[string]string{fmt.Sprint(GuestPort): d.invitados[d.creadas-1]}}
		d.maquinas[m.ID] = m
		c := *m
		responder(&c)
		return
	}
	partes := strings.Split(strings.TrimPrefix(r.URL.Path, "/machines/"), "/")
	m := d.maquinas[partes[0]]
	if m == nil {
		http.Error(w, `{"error":"machine does not exist"}`, http.StatusNotFound)
		return
	}
	switch {
	case len(partes) == 1 && r.Method == http.MethodGet:
	case len(partes) == 1 && r.Method == http.MethodDelete:
		delete(d.maquinas, m.ID)
		w.WriteHeader(http.StatusNoContent)
		return
	case len(partes) == 2 && partes[1] == "guest":
		responder(api.GuestResponse{Status: 200})
		return
	case len(partes) == 2 && partes[1] == "thaw":
		m.State = api.StateRunning
		if a := d.alDescongelar[m.ID]; a != "" {
			m.Forwards = map[string]string{fmt.Sprint(GuestPort): a}
		}
	case len(partes) == 2 && partes[1] == "freeze":
		m.State = api.StateWarm
	default:
		http.NotFound(w, r)
		return
	}
	c := *m
	responder(&c)
}

func (d *daemonAislado) existe(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.maquinas[id] != nil
}

func (d *daemonAislado) congelar(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.maquinas[id].State = api.StateWarm
}

func (d *daemonAislado) vio(llamada string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, l := range d.llamadas {
		if l == llamada {
			return true
		}
	}
	return false
}

func levantarDaemonAislado(t *testing.T, d *daemonAislado) string {
	t.Helper()
	d.maquinas = map[string]*api.Machine{}
	dir, err := os.MkdirTemp("/tmp", "kmcp-ais")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	srv := &http.Server{Handler: d}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// invitadosFalsos levanta n invitados que contestan con su nombre (A, B, ...).
func invitadosFalsos(t *testing.T, n int) ([]*invitadoRepetidor, []string) {
	t.Helper()
	var vs []*invitadoRepetidor
	var addrs []string
	for i := 0; i < n; i++ {
		v := &invitadoRepetidor{nombre: string(rune('A' + i))}
		s := httptest.NewServer(v)
		t.Cleanup(s.Close)
		vs = append(vs, v)
		addrs = append(addrs, strings.TrimPrefix(s.URL, "http://"))
	}
	return vs, addrs
}

func soy(rec *httptest.ResponseRecorder) string {
	var r struct {
		Result struct {
			Soy string `json:"soy"`
		} `json:"result"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	return r.Result.Soy
}

// El caso del hilo de r/mcp: dos sesiones de un servicio aislado no comparten
// máquina, y por tanto tampoco disco. Una sesión congelada vuelve a SU máquina,
// y cerrarla destruye la máquina antes de contestar.
func TestSesionesAisladasPorServicio(t *testing.T) {
	_, addrs := invitadosFalsos(t, 3)
	d := &daemonAislado{service: "notas", invitados: addrs}
	gw := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "")
	h := gw.Handler("")

	r1 := pedir(t, h, "POST", "/mcp/notas", "", cuerpoInit)
	r2 := pedir(t, h, "POST", "/mcp/notas", "", cuerpoInit)
	if r1.Code != 200 || r2.Code != 200 {
		t.Fatalf("initialize: %d %s / %d %s", r1.Code, r1.Body, r2.Code, r2.Body)
	}
	sa, sb := r1.Header().Get(SessionHeader), r2.Header().Get(SessionHeader)
	if sa == "" || sb == "" || sa == sb {
		t.Fatalf("ids de sesión: %q %q", sa, sb)
	}
	if soy(pedir(t, h, "POST", "/mcp/notas", sa, cuerpoCall)) != "A" ||
		soy(pedir(t, h, "POST", "/mcp/notas", sb, cuerpoCall)) != "B" {
		t.Fatal("las dos sesiones no están cada una en su máquina")
	}

	// A se congela (como hace el segador: fuera de las despiertas y congelada
	// en el daemon). Su siguiente petición la descongela a ELLA, no crea otra.
	gw.DropInstance("notas", "m1")
	d.congelar("m1")
	rec := pedir(t, h, "POST", "/mcp/notas", sa, cuerpoCall)
	if rec.Code != 200 || soy(rec) != "A" {
		t.Fatalf("tras congelar, la sesión A fue a %q (%d %s)", soy(rec), rec.Code, rec.Body)
	}
	if !d.vio("POST /machines/m1/thaw") {
		t.Fatal("la máquina de A no se descongeló")
	}
	if got := rec.Header().Get(SessionHeader); got != sa {
		t.Fatalf("la respuesta lleva %q, quería %q", got, sa)
	}

	// Cerrar A destruye su máquina antes del 204; B sigue.
	if rec := pedir(t, h, "DELETE", "/mcp/notas", sa, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body)
	}
	if d.existe("m1") {
		t.Fatal("la máquina de A sigue tras cerrar su sesión")
	}
	if rec := pedir(t, h, "POST", "/mcp/notas", sa, cuerpoCall); rec.Code != http.StatusNotFound {
		t.Fatalf("la sesión cerrada sigue viva: %d", rec.Code)
	}
	if soy(pedir(t, h, "POST", "/mcp/notas", sb, cuerpoCall)) != "B" || !d.existe("m2") {
		t.Fatal("cerrar A afectó a B")
	}

	// Una sesión nueva es una máquina nueva: nunca la de A reciclada.
	r3 := pedir(t, h, "POST", "/mcp/notas", "", cuerpoInit)
	if s3 := r3.Header().Get(SessionHeader); soy(pedir(t, h, "POST", "/mcp/notas", s3, cuerpoCall)) != "C" {
		t.Fatal("la sesión nueva no tiene máquina propia")
	}
}

// Si la máquina de la sesión desaparece (el recolector del daemon, un kling
// rm), la sesión se pierde con un 404: el cliente rehace el initialize. Darle
// otra máquina en silencio sería darle un disco vacío como si fuera el suyo.
func TestSesionAisladaSinMaquinaDa404(t *testing.T) {
	_, addrs := invitadosFalsos(t, 2)
	d := &daemonAislado{service: "notas", invitados: addrs}
	gw := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "")
	h := gw.Handler("")
	sa := pedir(t, h, "POST", "/mcp/notas", "", cuerpoInit).Header().Get(SessionHeader)
	gw.DropInstance("notas", "m1")
	d.mu.Lock()
	delete(d.maquinas, "m1")
	d.mu.Unlock()
	if rec := pedir(t, h, "POST", "/mcp/notas", sa, cuerpoCall); rec.Code != http.StatusNotFound {
		t.Fatalf("sesión sin máquina: %d %s", rec.Code, rec.Body)
	}
	if d.creadas != 1 {
		t.Fatalf("se crearon %d máquinas; la sesión perdida no debe llevarse otra", d.creadas)
	}
}

// Un DELETE o un GET sin sesión no despiertan una primaria compartida en un
// servicio aislado.
func TestServicioAisladoSinSesionNoDespiertaNada(t *testing.T) {
	_, addrs := invitadosFalsos(t, 1)
	d := &daemonAislado{service: "notas", invitados: addrs}
	h := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "").Handler("")
	if rec := pedir(t, h, "DELETE", "/mcp/notas", "", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE sin sesión: %d %s", rec.Code, rec.Body)
	}
	if d.creadas != 0 {
		t.Fatal("un DELETE sin sesión creó una máquina")
	}
}

// Sin la anotación, lo de siempre: las dos sesiones en la misma instancia.
func TestServicioCompartidoSigueCompartiendo(t *testing.T) {
	_, addrs := invitadosFalsos(t, 2)
	d := &daemonAislado{service: "notas", invitados: addrs, compartido: true}
	h := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "").Handler("")
	sa := pedir(t, h, "POST", "/mcp/notas", "", cuerpoInit).Header().Get(SessionHeader)
	sb := pedir(t, h, "POST", "/mcp/notas", "", cuerpoInit).Header().Get(SessionHeader)
	if soy(pedir(t, h, "POST", "/mcp/notas", sa, cuerpoCall)) != "A" ||
		soy(pedir(t, h, "POST", "/mcp/notas", sb, cuerpoCall)) != "A" {
		t.Fatal("en modo service las sesiones deberían compartir la instancia")
	}
}

// El agregador da a cada conversación su propia microVM del servicio aislado,
// y cerrar la conversación la destruye. Sin catálogo en el snapshot, listarlo
// usa también una máquina aislada de usar y tirar, nunca una compartida.
func TestAgregadorConServicioAislado(t *testing.T) {
	_, addrs := invitadosFalsos(t, 4)
	d := &daemonAislado{service: "notas", invitados: addrs}
	gw := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "")
	ctx := t.Context()
	s1 := &aggSession{id: "c1", services: []string{"notas"}, mode: modeProxy, backing: map[string]string{}}
	s2 := &aggSession{id: "c2", services: []string{"notas"}, mode: modeProxy, backing: map[string]string{}}
	gw.agg.sessions[s1.id], gw.agg.sessions[s2.id] = s1, s2

	quien := func(s *aggSession) string {
		t.Helper()
		out, fault := gw.agg.forward(ctx, s, "notas.echo", nil)
		if fault != nil {
			t.Fatalf("forward %s: %+v", s.id, fault)
		}
		var r struct {
			Soy string `json:"soy"`
		}
		b, _ := json.Marshal(out)
		_ = json.Unmarshal(b, &r)
		return r.Soy
	}
	q1, q2 := quien(s1), quien(s2)
	if q1 == "" || q1 == q2 {
		t.Fatalf("las conversaciones c1 y c2 cayeron en %q y %q", q1, q2)
	}
	if again := quien(s1); again != q1 {
		t.Fatalf("c1 cambió de máquina: %q y luego %q", q1, again)
	}
	d.mu.Lock()
	vivas := len(d.maquinas)
	d.mu.Unlock()
	if vivas != 2 {
		t.Fatalf("quedan %d máquinas; quería las 2 de las conversaciones (la del catálogo se destruye)", vivas)
	}
	gw.agg.drop(ctx, "c1")
	d.mu.Lock()
	vivas = len(d.maquinas)
	d.mu.Unlock()
	if vivas != 1 || quien(s2) != q2 {
		t.Fatal("cerrar c1 tenía que destruir su máquina y solo la suya")
	}
}

// Congelada POR DEBAJO del gateway (TTL del daemon, `kling freeze`): la
// instancia sigue registrada pero no contesta. La sesión tiene que despertar
// SU máquina, no quedarse con la entrada muerta ni crear otra.
func TestSesionAisladaCongeladaPorDebajo(t *testing.T) {
	vs, addrs := invitadosFalsos(t, 2)
	muerto := httptest.NewServer(vs[0]) // la dirección de antes del freeze
	d := &daemonAislado{service: "notas", invitados: []string{strings.TrimPrefix(muerto.URL, "http://"), addrs[1]},
		alDescongelar: map[string]string{"m1": addrs[0]}}
	gw := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "")
	h := gw.Handler("")
	sa := pedir(t, h, "POST", "/mcp/notas", "", cuerpoInit).Header().Get(SessionHeader)
	if soy(pedir(t, h, "POST", "/mcp/notas", sa, cuerpoCall)) != "A" {
		t.Fatal("la sesión no arrancó en su máquina")
	}
	muerto.Close()
	d.congelar("m1")

	rec := pedir(t, h, "POST", "/mcp/notas", sa, cuerpoCall)
	if rec.Code != 200 || soy(rec) != "A" {
		t.Fatalf("tras el freeze por debajo: %d %s", rec.Code, rec.Body)
	}
	if !d.vio("POST /machines/m1/thaw") || d.creadas != 1 {
		t.Fatalf("tenía que descongelar m1 sin crear otra (creadas=%d)", d.creadas)
	}
}
