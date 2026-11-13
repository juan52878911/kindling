package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// invitadoStreams es un puente falso que mantiene abierto cada GET de sesión
// hasta que el cliente o el gateway lo suelten, y cuenta cuántos hay abiertos.
type invitadoStreams struct {
	invitadoRepetidor
	abiertos atomic.Int32
	llegados chan struct{}
}

func (v *invitadoStreams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		v.invitadoRepetidor.ServeHTTP(w, r)
		return
	}
	v.abiertos.Add(1)
	defer v.abiertos.Add(-1)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	v.llegados <- struct{}{}
	<-r.Context().Done()
}

// M8: una sesión tiene un stream SSE como mucho. N GET seguidos dejan abierto
// solo el último; los anteriores acaban en vez de acumularse.
func TestUnStreamSSEPorSesion(t *testing.T) {
	inv := &invitadoStreams{llegados: make(chan struct{}, 8)}
	srvInv := httptest.NewServer(inv)
	defer srvInv.Close()
	sock := daemonDosMaquinas(t, "eco", strings.TrimPrefix(srvInv.URL, "http://"))
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	srv := httptest.NewServer(gw.Handler(""))
	defer srv.Close()

	pedirHTTP := func(metodo, sid, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(metodo, srv.URL+"/mcp/eco", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if sid != "" {
			req.Header.Set(SessionHeader, sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r := pedirHTTP("POST", "", cuerpoInit)
	_ = r.Body.Close()
	sid := r.Header.Get(SessionHeader)
	if r.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize: %d", r.StatusCode)
	}

	const n = 4
	var streams []*http.Response
	for i := 0; i < n; i++ {
		s := pedirHTTP("GET", sid, "")
		defer func() { _ = s.Body.Close() }()
		if s.StatusCode != 200 {
			t.Fatalf("GET %d de sesión: %d", i, s.StatusCode)
		}
		select {
		case <-inv.llegados:
		case <-time.After(5 * time.Second):
			t.Fatalf("el stream %d no llegó al invitado", i)
		}
		streams = append(streams, s)
	}
	// Los n-1 primeros terminan: el cliente ve su fin.
	for i, s := range streams[:n-1] {
		fin := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, s.Body); close(fin) }()
		select {
		case <-fin:
		case <-time.After(5 * time.Second):
			t.Fatalf("el stream %d sigue abierto tras abrir otro de la misma sesión", i)
		}
	}
	plazo := time.Now().Add(5 * time.Second)
	for inv.abiertos.Load() != 1 && time.Now().Before(plazo) {
		time.Sleep(10 * time.Millisecond)
	}
	if a := inv.abiertos.Load(); a != 1 {
		t.Fatalf("%d streams abiertos en el invitado; quería 1", a)
	}
}

// M8: el GET del stream de una sesión aislada con su máquina congelada no la
// descongela: el cliente recibe 405 y sigue por POST, que sí la despierta.
func TestStreamDeSesionAisladaCongeladaNoDescongela(t *testing.T) {
	vs, addrs := invitadosFalsos(t, 2)
	muerto := httptest.NewServer(vs[0])
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

	rec := pedir(t, h, "GET", "/mcp/notas", sa, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET de la sesión congelada: %d %s; quería 405", rec.Code, rec.Body)
	}
	if d.vio("POST /machines/m1/thaw") {
		t.Fatal("el GET del stream descongeló la máquina de la sesión")
	}
	// La sesión sigue: el POST siguiente la despierta.
	if rec := pedir(t, h, "POST", "/mcp/notas", sa, cuerpoCall); rec.Code != 200 || soy(rec) != "A" {
		t.Fatalf("POST tras el GET: %d %s", rec.Code, rec.Body)
	}
}

// M9: en /mcp/_all solo el initialize abre conversación; cualquier otra
// petición sin sesión recibe 400 y no deja ninguna acuñada.
func TestAgregadorSoloAcunaEnInitialize(t *testing.T) {
	_, addrs := invitadosFalsos(t, 1)
	sock := daemonDosMaquinas(t, "eco", addrs[0])
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	h := gw.Handler("")
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"eco.echo"}}`,
	} {
		rec := pedir(t, h, "POST", "/mcp/_all", "", body)
		if rec.Code != http.StatusBadRequest || rec.Header().Get(SessionHeader) != "" {
			t.Fatalf("%s sin sesión: %d (sesión %q); quería 400 sin sesión", body, rec.Code, rec.Header().Get(SessionHeader))
		}
	}
	gw.agg.mu.Lock()
	n := len(gw.agg.sessions)
	gw.agg.mu.Unlock()
	if n != 0 {
		t.Fatalf("quedan %d conversaciones acuñadas sin initialize", n)
	}
	if rec := pedir(t, h, "POST", "/mcp/_all", "", cuerpoInit); rec.Code != 200 || rec.Header().Get(SessionHeader) == "" {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
}

// M9: un nombre de servicio que no existe no entra en la popularidad
// persistida; uno que responde, sí.
func TestPopularidadSoloConServicioValido(t *testing.T) {
	_, addrs := invitadosFalsos(t, 1)
	sock := daemonDosMaquinas(t, "eco", addrs[0])
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	popu := t.TempDir() + "/pop.json"
	gw.SetPopularityFile(popu)
	h := gw.Handler("")
	if rec := pedir(t, h, "POST", "/mcp/fantasma", "", cuerpoInit); rec.Code < 400 {
		t.Fatalf("servicio inexistente: %d", rec.Code)
	}
	if rec := pedir(t, h, "POST", "/mcp/eco", "", cuerpoInit); rec.Code != 200 {
		t.Fatalf("initialize de eco: %d %s", rec.Code, rec.Body)
	}
	gw.Drain(t.Context())
	b, err := os.ReadFile(popu)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "fantasma") || !strings.Contains(string(b), `"eco"`) {
		t.Fatalf("popularidad persistida: %s; quería eco y no fantasma", b)
	}
}
