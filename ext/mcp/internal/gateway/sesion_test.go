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

	"github.com/juan52878911/kindling/pkg/api"
)

// SESIONES ACUÑADAS POR EL GATEWAY.
//
// Réplicas restauradas del mismo snapshot llegaron a dar el MISMO
// Mcp-Session-Id (CSPRNG del invitado copiado en la restauración, en macOS sin
// VMGenID), y el gateway usaba ese id como clave global: el segundo cliente
// reapuntaba la sesión del primero a su microVM. Aquí dos invitados falsos dan
// adrede el mismo id, como lo haría también un invitado hostil.

const sidRepetido = "mismo-id-en-las-dos"

// invitadoRepetidor es un puente falso que da siempre sidRepetido. Con lleno,
// rechaza un segundo initialize con el "session limit" del puente de verdad,
// para que el gateway cree una réplica. Anota lo que recibe.
type invitadoRepetidor struct {
	nombre string
	lleno  bool

	mu     sync.Mutex
	inits  int
	vistos []string // "MÉTODO ruta sesión"
}

func (v *invitadoRepetidor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	body, _ := api.LeerCuerpo(r.Body, 1<<20)
	_ = json.Unmarshal(body, &rpc)
	v.mu.Lock()
	v.vistos = append(v.vistos, r.Method+" "+r.URL.Path+" "+r.Header.Get(SessionHeader))
	if rpc.Method == "initialize" {
		v.inits++
		if v.lleno && v.inits > 1 {
			v.mu.Unlock()
			http.Error(w, "session limit reached (1)", http.StatusBadRequest)
			return
		}
	}
	v.mu.Unlock()

	// Como algunos servidores Streamable HTTP, la cabecera va en TODAS las
	// respuestas de la sesión: el gateway tiene que traducirla siempre.
	w.Header().Set(SessionHeader, sidRepetido)
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if rpc.Method == "tools/list" {
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"echo"}]}}`, string(rpc.ID))
		return
	}
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"soy":%q}}`, string(rpc.ID), v.nombre)
}

func (v *invitadoRepetidor) recibidos() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.vistos...)
}

// daemonDosMaquinas es un daemon falso que entrega una máquina distinta en
// cada POST /machines: primero la de a, luego la de b.
func daemonDosMaquinas(t *testing.T, service string, addrs ...string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kmcp-ses")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")

	var mu sync.Mutex
	var creadas []*api.Machine
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshots", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*api.Snapshot{{Name: service,
			Labels: map[string]string{api.LabelService: service}, Egress: "none"}})
	})
	mux.HandleFunc("/store/mcp/links", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.Info{})
	})
	mux.HandleFunc("/machines", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			if len(creadas) >= len(addrs) {
				http.Error(w, "no more machines in this fake", http.StatusInsufficientStorage)
				return
			}
			i := len(creadas)
			m := &api.Machine{ID: fmt.Sprintf("m%d", i+1), Name: fmt.Sprintf("%s-%d", service, i+1),
				State: api.StateRunning, IP: "172.16.0.2",
				Labels:   map[string]string{api.LabelService: service},
				Forwards: map[string]string{fmt.Sprint(GuestPort): addrs[i]}}
			creadas = append(creadas, m)
			_ = json.NewEncoder(w).Encode(m)
			return
		}
		// GET: como el falso de addr_test, ninguna "en marcha" que adoptar;
		// así cada instancia nueva es una máquina nueva.
		_ = json.NewEncoder(w).Encode([]*api.Machine{})
	})
	mux.HandleFunc("/machines/{id}/guest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.GuestResponse{Status: 200})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fake daemon: not implemented "+r.URL.Path, http.StatusNotFound)
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func pedir(t *testing.T, h http.Handler, metodo, ruta, sid, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(metodo, ruta, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.Header.Set(SessionHeader, sid)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const (
	cuerpoInit = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	cuerpoCall = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x"}}`
)

func TestSesionesAcunadasConIdRepetidoDelInvitado(t *testing.T) {
	a := &invitadoRepetidor{nombre: "A", lleno: true}
	b := &invitadoRepetidor{nombre: "B"}
	sa, sb := httptest.NewServer(a), httptest.NewServer(b)
	defer sa.Close()
	defer sb.Close()
	sock := daemonDosMaquinas(t, "eco",
		strings.TrimPrefix(sa.URL, "http://"), strings.TrimPrefix(sb.URL, "http://"))
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	h := gw.Handler("")

	// Dos initialize: el primero cae en A; A está lleno y el segundo acaba en
	// una réplica B. Los dos invitados contestan con el MISMO id.
	r1 := pedir(t, h, "POST", "/mcp/eco", "", cuerpoInit)
	r2 := pedir(t, h, "POST", "/mcp/eco", "", cuerpoInit)
	if r1.Code != 200 || r2.Code != 200 {
		t.Fatalf("initialize: %d %s / %d %s", r1.Code, r1.Body, r2.Code, r2.Body)
	}
	ext1, ext2 := r1.Header().Get(SessionHeader), r2.Header().Get(SessionHeader)
	if ext1 == "" || ext2 == "" || ext1 == ext2 {
		t.Fatalf("ids externos: %q %q (tienen que ser distintos)", ext1, ext2)
	}
	if ext1 == sidRepetido || ext2 == sidRepetido || len(ext1) != 32 {
		t.Fatalf("el cliente vio el id del invitado o uno no acuñado: %q %q", ext1, ext2)
	}

	// Cada sesión va a SU instancia, con el id del invitado en la ida y el
	// externo en la vuelta.
	for _, c := range []struct {
		ext, quien string
	}{{ext1, "A"}, {ext2, "B"}, {ext1, "A"}} {
		rec := pedir(t, h, "POST", "/mcp/eco", c.ext, cuerpoCall)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"soy":"`+c.quien+`"`) {
			t.Fatalf("sesión %s: %d %s (quería la instancia %s)", c.ext[:8], rec.Code, rec.Body, c.quien)
		}
		if got := rec.Header().Get(SessionHeader); got != c.ext {
			t.Fatalf("la respuesta lleva %q, quería el id externo %q", got, c.ext)
		}
	}
	for _, v := range []*invitadoRepetidor{a, b} {
		for _, l := range v.recibidos() {
			if strings.Contains(l, ext1) || strings.Contains(l, ext2) {
				t.Fatalf("el invitado %s vio un id externo: %q", v.nombre, l)
			}
			if strings.HasPrefix(l, "POST /mcp ") && !strings.HasSuffix(l, " "+sidRepetido) && !strings.HasSuffix(l, " ") {
				t.Fatalf("el invitado %s recibió un id que no es el suyo: %q", v.nombre, l)
			}
		}
	}

	// Un id inventado —incluido el del invitado, que un cliente podría conocer—
	// no llega a ningún invitado: 404, y el cliente rehace el initialize.
	antesA, antesB := len(a.recibidos()), len(b.recibidos())
	for _, forjado := range []string{sidRepetido, "0123456789abcdef0123456789abcdef"} {
		if rec := pedir(t, h, "POST", "/mcp/eco", forjado, cuerpoCall); rec.Code != http.StatusNotFound {
			t.Fatalf("id forjado %q: %d %s", forjado, rec.Code, rec.Body)
		}
	}
	// Ni el de una sesión de eco usado en la ruta de otro servicio.
	if rec := pedir(t, h, "POST", "/mcp/otro", ext1, cuerpoCall); rec.Code != http.StatusNotFound {
		t.Fatalf("sesión de eco en /mcp/otro: %d %s", rec.Code, rec.Body)
	}
	if len(a.recibidos()) != antesA || len(b.recibidos()) != antesB {
		t.Fatal("una petición con id forjado llegó a un invitado")
	}

	// DELETE cierra la sesión en SU invitado (con su id) y la olvida.
	if rec := pedir(t, h, "DELETE", "/mcp/eco", ext1, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body)
	}
	ra := a.recibidos()
	if last := ra[len(ra)-1]; last != "DELETE /mcp "+sidRepetido {
		t.Fatalf("A recibió %q", last)
	}
	if rec := pedir(t, h, "POST", "/mcp/eco", ext1, cuerpoCall); rec.Code != http.StatusNotFound {
		t.Fatalf("sesión cerrada sigue viva: %d", rec.Code)
	}
	// La otra sigue intacta.
	if rec := pedir(t, h, "POST", "/mcp/eco", ext2, cuerpoCall); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `"soy":"B"`) {
		t.Fatalf("la sesión de B se vio afectada: %d %s", rec.Code, rec.Body)
	}
}

// Las rutas de control del agente y del puente no se reenvían: son del host.
func TestRutasDeControlNoLleganAlInvitado(t *testing.T) {
	a := &invitadoRepetidor{nombre: "A"}
	sa := httptest.NewServer(a)
	defer sa.Close()
	sock := daemonDosMaquinas(t, "eco", strings.TrimPrefix(sa.URL, "http://"))
	h := New(api.NewClient(sock), 5*time.Minute, false, 0, "").Handler("")

	for _, ruta := range []string{"/mcp/eco/resync", "/mcp/eco/volume/release", "/mcp/eco/exec",
		"/mcp/eco/exec/pty", "/mcp/eco/files", "/mcp/eco/reset", "/mcp/eco/x/../resync"} {
		rec := pedir(t, h, "POST", ruta, "", `{"unix_nano":1}`)
		// Una ruta sin limpiar la redirige el propio ServeMux (307) a la
		// limpia, que es 404: tampoco llega al invitado.
		if rec.Code != http.StatusNotFound && !(strings.Contains(ruta, "..") && rec.Code == http.StatusTemporaryRedirect) {
			t.Errorf("%s: %d, quería 404", ruta, rec.Code)
		}
	}
	if n := len(a.recibidos()); n != 0 {
		t.Fatalf("el invitado recibió %d peticiones de control: %v", n, a.recibidos())
	}
	// Y lo normal sigue pasando.
	if rec := pedir(t, h, "POST", "/mcp/eco", "", cuerpoInit); rec.Code != 200 {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
}

// Sin sesión fijada (un método sin cuerpo reenviable) el id que ponga el
// invitado no sale: el cliente no debe adoptar un id que el gateway no conoce.
func TestSidWriterSinExternoQuitaLaCabecera(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &sidWriter{ResponseWriter: rec}
	sw.Header().Set(SessionHeader, "del-invitado")
	sw.WriteHeader(200)
	if got := rec.Header().Get(SessionHeader); got != "" {
		t.Fatalf("cabecera = %q", got)
	}
	rec = httptest.NewRecorder()
	sw = &sidWriter{ResponseWriter: rec, ext: "externo"}
	sw.Header().Set(SessionHeader, "del-invitado")
	_, _ = sw.Write([]byte("x"))
	if got := rec.Header().Get(SessionHeader); got != "externo" {
		t.Fatalf("cabecera = %q", got)
	}
}

// El agregador guarda la sesión del invitado POR INSTANCIA: si la primaria del
// servicio pasa a ser otra máquina, la nueva recibe su propio initialize en vez
// del id de la anterior —que en una réplica del mismo snapshot podía ser el de
// la sesión de otro cliente—.
func TestAgregadorNoLlevaElIdDeUnaInstanciaAOtra(t *testing.T) {
	a := &invitadoRepetidor{nombre: "A"}
	b := &invitadoRepetidor{nombre: "B"}
	sa, sb := httptest.NewServer(a), httptest.NewServer(b)
	defer sa.Close()
	defer sb.Close()
	sock := daemonDosMaquinas(t, "eco",
		strings.TrimPrefix(sa.URL, "http://"), strings.TrimPrefix(sb.URL, "http://"))
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	s := &aggSession{id: "t", services: []string{"eco"}, mode: modeProxy, backing: map[string]string{}}

	ctx := t.Context()
	if _, fault := gw.agg.forward(ctx, s, "eco.echo", nil); fault != nil {
		t.Fatalf("forward a A: %+v", fault)
	}
	gw.DropInstance("eco", "m1") // la primaria pasa a ser otra máquina
	if _, fault := gw.agg.forward(ctx, s, "eco.echo", nil); fault != nil {
		t.Fatalf("forward a B: %+v", fault)
	}
	rb := b.recibidos()
	if len(rb) < 2 || !strings.HasSuffix(rb[0], " ") {
		t.Fatalf("B no recibió primero un initialize sin sesión: %v", rb)
	}
	b.mu.Lock()
	inits := b.inits
	b.mu.Unlock()
	if inits != 1 {
		t.Fatalf("B recibió %d initialize, quería 1: %v", inits, rb)
	}
}

// Un initialize más grande que el tope no se buferea entero: el invitado es
// hostil y podría llenar la memoria del gateway.
func TestGrabadorAcotadoCortaRespuestasEnormes(t *testing.T) {
	g := &grabadorAcotado{ResponseRecorder: httptest.NewRecorder(), max: 10}
	if _, err := g.Write([]byte("0123456789")); err != nil || g.excedido {
		t.Fatalf("hasta el tope tiene que aceptar: err=%v excedido=%v", err, g.excedido)
	}
	if _, err := g.Write([]byte("x")); err == nil || !g.excedido {
		t.Fatal("pasado el tope tiene que cortar y marcarlo")
	}
	if g.Body.Len() != 10 {
		t.Fatalf("buferado %d bytes, quería 10", g.Body.Len())
	}
}
