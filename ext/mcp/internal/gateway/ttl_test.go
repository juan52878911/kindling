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
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// EL TTL DE UNA MICROVM EFÍMERA A MEDIA LLAMADA.
//
// El daemon congela las máquinas cuyo TTL venció (expireTTL), contando desde
// que las creó o desde la última renovación: el tráfico HTTP no reinicia el
// reloj. Una acción efímera larga —semgrep sobre un repo grande pasa del
// minuto— se quedaba con la microVM congelada debajo: la del camino lento nace
// con un TTL fijo, y a una del fondo sacada cerca de su edad máxima le queda
// poco TTL.

// daemonTTL imita lo justo del daemon para el modo efímero: crear, destruir y
// renovar máquinas, y el vigilante que congela las que vencieron.
type daemonTTL struct {
	guestAddr string

	mu       sync.Mutex
	maquinas map[string]*api.Machine
	llamadas []string
	n        int
}

func (d *daemonTTL) handler() http.Handler {
	mux := http.NewServeMux()
	responder := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	anotar := func(r *http.Request) {
		d.mu.Lock()
		d.llamadas = append(d.llamadas, r.Method+" "+r.URL.Path)
		d.mu.Unlock()
	}
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		responder(w, api.Info{Version: "test", Capabilities: []string{"store", "renew"}})
	})
	mux.HandleFunc("GET /snapshots", func(w http.ResponseWriter, r *http.Request) {
		responder(w, []*api.Snapshot{{Name: "svc", Labels: map[string]string{api.LabelService: "svc"}}})
	})
	mux.HandleFunc("GET /machines", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		l := []*api.Machine{}
		for _, mc := range d.maquinas {
			c := *mc
			l = append(l, &c)
		}
		responder(w, l)
	})
	mux.HandleFunc("POST /machines", func(w http.ResponseWriter, r *http.Request) {
		anotar(r)
		var req api.RunRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.n++
		ahora := time.Now()
		// Ids de 8 caracteres o más, como los de verdad: el gateway los recorta
		// a 8 en sus registros.
		id := fmt.Sprintf("maquina%d", d.n)
		mc := &api.Machine{
			ID: id, Name: id, State: api.StateRunning,
			From: req.From, Labels: req.Labels, IP: "172.16.0.2",
			Forwards:   map[string]string{fmt.Sprint(GuestPort): d.guestAddr},
			TTLSeconds: req.TTLSeconds, TTLAt: &ahora, StartedAt: &ahora,
		}
		d.maquinas[mc.ID] = mc
		c := *mc
		responder(w, &c)
	})
	mux.HandleFunc("DELETE /machines/{id}", func(w http.ResponseWriter, r *http.Request) {
		anotar(r)
		d.mu.Lock()
		delete(d.maquinas, r.PathValue("id"))
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /machines/{id}/renew", func(w http.ResponseWriter, r *http.Request) {
		anotar(r)
		var req api.RenewRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		defer d.mu.Unlock()
		mc := d.maquinas[r.PathValue("id")]
		if mc == nil {
			http.NotFound(w, r)
			return
		}
		if req.TTLSeconds > 0 {
			mc.TTLSeconds = req.TTLSeconds
		}
		ahora := time.Now()
		mc.TTLAt = &ahora
		c := *mc
		responder(w, &c)
	})
	// esperarListo pregunta aquí cuando la máquina trae Forwards.
	mux.HandleFunc("/machines/{id}/guest", func(w http.ResponseWriter, r *http.Request) {
		responder(w, api.GuestResponse{Status: 200})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daemon falso: ruta no implementada "+r.URL.Path, http.StatusNotFound)
	})
	return mux
}

// vigilar es expireTTL del daemon: congela las que llevan corriendo más de su
// TTL según su reloj (TTLAt).
func (d *daemonTTL) vigilar() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, mc := range d.maquinas {
		if mc.State == api.StateRunning && mc.TTLSeconds > 0 && mc.TTLAt != nil &&
			time.Since(*mc.TTLAt) >= time.Duration(mc.TTLSeconds)*time.Second {
			mc.State = api.StateWarm
		}
	}
}

// algunaCongelada dice si el vigilante congeló alguna de las que siguen vivas.
func (d *daemonTTL) algunaCongelada() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, mc := range d.maquinas {
		if mc.State != api.StateRunning {
			return true
		}
	}
	return false
}

// envejecer deja a todas las máquinas con `queda` de TTL por delante.
func (d *daemonTTL) envejecer(queda time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, mc := range d.maquinas {
		t := time.Now().Add(queda - time.Duration(mc.TTLSeconds)*time.Second)
		mc.TTLAt = &t
	}
}

func (d *daemonTTL) visto(llamada string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, l := range d.llamadas {
		if l == llamada {
			n++
		}
	}
	return n
}

// conDaemonTTL levanta el invitado MCP falso, cuyo tools/call tarda `dura`, y
// el daemon falso con su vigilante en marcha. Si la máquina se congela a media
// llamada, el invitado corta la conexión: una microVM congelada no contesta.
func conDaemonTTL(t *testing.T, dura time.Duration) (*daemonTTL, *api.Client) {
	t.Helper()
	d := &daemonTTL{maquinas: map[string]*api.Machine{}}

	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := api.LeerCuerpo(r.Body, 1<<20)
		_ = json.Unmarshal(body, &rpc)
		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "initialize":
			w.Header().Set(SessionHeader, "sesion-falsa")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"serverInfo":{"name":"falso"}}}`, string(rpc.ID))
		case "tools/call":
			time.Sleep(dura)
			if d.algunaCongelada() {
				panic(http.ErrAbortHandler)
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`,
				string(rpc.ID))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(guest.Close)
	d.guestAddr = guest.Listener.Addr().String()

	// /tmp y no t.TempDir(): límite de sun_path en macOS (ver addr_test.go).
	dir, err := os.MkdirTemp("/tmp", "kmcp-ttl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	srv := &http.Server{Handler: d.handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// El vigilante del daemon pasa cada ~10 s; aquí más a menudo.
	fin, hecho := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(hecho)
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-fin:
				return
			case <-tk.C:
				d.vigilar()
			}
		}
	}()
	t.Cleanup(func() { close(fin); <-hecho })
	return d, api.NewClient(sock)
}

var herramientaLenta = &Tool{Service: "svc", Name: "lenta", Qualified: "svc.lenta"}

// Camino lento: la efímera nace con ephemeralTTL, y una llamada más larga que
// eso se congelaba a medias. Con un TTL de 1 s para no esperar dos minutos.
func TestEfimeraMasLargaQueSuTTLNoSeCongela(t *testing.T) {
	antes := ephemeralTTL
	ephemeralTTL = time.Second
	t.Cleanup(func() { ephemeralTTL = antes })

	d, client := conDaemonTTL(t, 2500*time.Millisecond)
	gw := New(client, 5*time.Minute, true, 0, "")

	if _, fault := gw.agg.callEphemeral(context.Background(), herramientaLenta, nil); fault != nil {
		t.Fatalf("la llamada falló (%s): la efímera se congeló a medias porque nadie renovó su TTL", fault.msg)
	}
	if d.visto("POST /machines/maquina1/renew") < 2 {
		t.Errorf("renovaciones de la efímera = %d; con un TTL de 1 s y 2,5 s de llamada hacía falta latir",
			d.visto("POST /machines/maquina1/renew"))
	}
}

// Camino rápido: una del fondo se crea con TTL 2×idle+2m y se retira a los
// 2×idle de edad, con la holgura de una vuelta del segador. Sacada justo antes,
// le queda poco TTL y una acción larga sobre ella se congelaba a media llamada.
func TestDelFondoCercaDeSuTTLNoSeCongela(t *testing.T) {
	d, client := conDaemonTTL(t, 600*time.Millisecond)
	gw := New(client, 5*time.Minute, true, 1, "")

	gw.FillPool(context.Background(), "svc", "svc")
	limite := time.Now().Add(5 * time.Second)
	for gw.PoolStats()["svc"] < 1 {
		if time.Now().After(limite) {
			t.Fatal("el fondo no llegó a precalentar la instancia")
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.envejecer(200 * time.Millisecond)

	if _, fault := gw.agg.callEphemeral(context.Background(), herramientaLenta, nil); fault != nil {
		t.Fatalf("la llamada falló (%s): la del fondo se congeló a medias porque no se renovó su TTL al sacarla", fault.msg)
	}
	if d.visto("POST /machines/maquina1/renew") == 0 {
		t.Error("no se renovó el TTL de la del fondo al sacarla")
	}
}
