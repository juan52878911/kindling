package gateway

import (
	"context"
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

// puenteLleno es un puente falso con las sesiones de un gateway anterior: todo
// initialize recibe el "session limit" hasta que alguien le hace POST /reset.
type puenteLleno struct {
	nombre string

	mu      sync.Mutex
	lleno   bool
	resets  int
	initsOK int
}

func (p *puenteLleno) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.URL.Path == "/reset" {
		p.resets++
		p.lleno = false
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	body, _ := api.LeerCuerpo(r.Body, 1<<20)
	_ = json.Unmarshal(body, &rpc)
	if rpc.Method == "initialize" {
		if p.lleno {
			http.Error(w, "session limit of 1 reached", http.StatusBadRequest)
			return
		}
		p.initsOK++
	}
	w.Header().Set(SessionHeader, "sid-"+p.nombre)
	w.Header().Set("Content-Type", "application/json")
	if rpc.Method == "tools/list" {
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"echo"}]}}`, string(rpc.ID))
		return
	}
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"soy":%q}}`, string(rpc.ID), p.nombre)
}

// daemonConUnaEnMarcha es un daemon falso con una máquina del servicio YA en
// marcha (la que dejó el gateway anterior) y, en POST /machines, réplicas
// nuevas en las direcciones de nuevas.
func daemonConUnaEnMarcha(t *testing.T, service, vieja string, nuevas ...string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kmcp-adop")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	maquina := func(i int, addr string) *api.Machine {
		return &api.Machine{ID: fmt.Sprintf("m%d", i), Name: fmt.Sprintf("%s-%d", service, i),
			State: api.StateRunning, IP: "172.16.0.2", Labels: map[string]string{api.LabelService: service},
			Forwards: map[string]string{fmt.Sprint(GuestPort): addr}}
	}
	var mu sync.Mutex
	creadas := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshots", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*api.Snapshot{{Name: service,
			Labels: map[string]string{api.LabelService: service}, Egress: "none"}})
	})
	mux.HandleFunc("/store/mcp/links", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(api.Info{}) })
	mux.HandleFunc("/machines", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			if creadas >= len(nuevas) {
				http.Error(w, "no more machines in this fake", http.StatusInsufficientStorage)
				return
			}
			creadas++
			_ = json.NewEncoder(w).Encode(maquina(1+creadas, nuevas[creadas-1]))
			return
		}
		_ = json.NewEncoder(w).Encode([]*api.Machine{maquina(1, vieja)})
	})
	mux.HandleFunc("/machines/{id}/guest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.GuestResponse{Status: 200})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// renew y lo demás que no importa aquí.
		w.WriteHeader(http.StatusNoContent)
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

// #187: al reiniciar el gateway, la instancia que ya corría trae llenas las
// sesiones del proceso anterior. Al adoptarla, el gateway le hace POST /reset
// y el initialize siguiente entra en ella.
func TestAdoptarUnaInstanciaLimpiaSusSesiones(t *testing.T) {
	vieja := &puenteLleno{nombre: "vieja", lleno: true}
	sv := httptest.NewServer(vieja)
	defer sv.Close()
	sock := daemonConUnaEnMarcha(t, "eco", strings.TrimPrefix(sv.URL, "http://"))
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := &aggSession{id: "t", services: []string{"eco"}, mode: modeProxy, backing: map[string]string{}}
	res, fault := gw.agg.forward(ctx, s, "eco.echo", nil)
	if fault != nil {
		t.Fatalf("forward: %+v", fault)
	}
	if !strings.Contains(texto(res), `"soy":"vieja"`) {
		t.Fatalf("respuesta %v: quería la instancia adoptada", res)
	}
	vieja.mu.Lock()
	defer vieja.mu.Unlock()
	if vieja.resets != 1 || vieja.initsOK == 0 {
		t.Fatalf("resets %d, initialize aceptados %d; quería 1 reset y algún initialize", vieja.resets, vieja.initsOK)
	}
}

// Readoptar una máquina PROPIA —soltada por un fallo de vida pasajero y vuelta
// a coger por Ensure— no resetea su puente si este gateway tiene sesiones en
// ella: el reset las borraba justo antes de reapuntarlas. Sin sesiones, sí.
func TestReadoptarConSesionesVivasNoReseteaElPuente(t *testing.T) {
	vieja := &puenteLleno{nombre: "vieja"}
	sv := httptest.NewServer(vieja)
	defer sv.Close()
	sock := daemonConUnaEnMarcha(t, "eco", strings.TrimPrefix(sv.URL, "http://"))
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resets := func() int { vieja.mu.Lock(); defer vieja.mu.Unlock(); return vieja.resets }

	e, err := gw.Ensure(ctx, "eco")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if resets() != 1 {
		t.Fatalf("la primera adopción hizo %d resets, quería 1", resets())
	}

	// Una sesión directa fijada a la máquina: readoptar no resetea.
	gw.Bind("cliente-1", "eco", e)
	gw.DropInstance("eco", e.MachineID())
	if _, err := gw.Ensure(ctx, "eco"); err != nil {
		t.Fatalf("Ensure tras soltarla: %v", err)
	}
	if resets() != 1 {
		t.Fatalf("readoptar con una sesión directa viva reseteó el puente (%d resets)", resets())
	}

	// Una conversación del agregador con sesión en esa máquina: tampoco.
	gw.Forget("cliente-1")
	gw.agg.mu.Lock()
	gw.agg.sessions["agg-1"] = &aggSession{id: "agg-1", backing: map[string]string{
		"eco\x00" + e.MachineID(): "sid-vieja"}}
	gw.agg.mu.Unlock()
	gw.DropInstance("eco", e.MachineID())
	if _, err := gw.Ensure(ctx, "eco"); err != nil {
		t.Fatalf("Ensure tras soltarla: %v", err)
	}
	if resets() != 1 {
		t.Fatalf("readoptar con una sesión del agregador viva reseteó el puente (%d resets)", resets())
	}

	// Sin sesiones de este gateway, la máquina se trata como heredada.
	gw.agg.mu.Lock()
	delete(gw.agg.sessions, "agg-1")
	gw.agg.mu.Unlock()
	gw.DropInstance("eco", e.MachineID())
	if _, err := gw.Ensure(ctx, "eco"); err != nil {
		t.Fatalf("Ensure tras soltarla: %v", err)
	}
	if resets() != 2 {
		t.Fatalf("readoptar sin sesiones no reseteó el puente (%d resets, quería 2)", resets())
	}
}

// El agregador, ante una primaria llena, va a una réplica nueva como el
// initialize directo, en vez de devolver el 400 al cliente.
func TestAgregadorConPrimariaLlenaVaAUnaReplica(t *testing.T) {
	// A, la primaria; B y C, las réplicas que creen el catálogo y el
	// agregador al encontrarla llena.
	a := &invitadoRepetidor{nombre: "A"}
	b := &invitadoRepetidor{nombre: "B"}
	c := &invitadoRepetidor{nombre: "C"}
	sa, sb, sc := httptest.NewServer(a), httptest.NewServer(b), httptest.NewServer(c)
	defer sa.Close()
	defer sb.Close()
	defer sc.Close()
	sock := daemonDosMaquinas(t, "eco", strings.TrimPrefix(sa.URL, "http://"),
		strings.TrimPrefix(sb.URL, "http://"), strings.TrimPrefix(sc.URL, "http://"))
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	// A acepta un solo initialize: la sesión directa de un cliente la llena.
	a.lleno = true
	h := gw.Handler("")
	if r := pedir(t, h, "POST", "/mcp/eco", "", cuerpoInit); r.Code != 200 {
		t.Fatalf("initialize directo: %d %s", r.Code, r.Body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := &aggSession{id: "t", services: []string{"eco"}, mode: modeProxy, backing: map[string]string{}}
	res, fault := gw.agg.forward(ctx, s, "eco.echo", nil)
	if fault != nil {
		t.Fatalf("forward con la primaria llena: %+v", fault)
	}
	if r := texto(res); strings.Contains(r, `"soy":"A"`) || !strings.Contains(r, `"soy":`) {
		t.Fatalf("respuesta %v: quería una réplica, no la primaria llena", res)
	}
}

// texto es la respuesta de forward como texto, sea json.RawMessage, []byte o
// lo que sea (fmt.Sprint de un RawMessage imprime sus bytes como números).
func texto(v any) string {
	switch x := v.(type) {
	case json.RawMessage:
		return string(x)
	case []byte:
		return string(x)
	case string:
		return x
	}
	b, _ := json.Marshal(v)
	return string(b)
}
