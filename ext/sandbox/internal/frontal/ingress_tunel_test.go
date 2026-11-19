package frontal

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

// bancoTunel monta el gateway contra un daemon falso que, según anunciaTunel,
// anuncia tcp-tunnel y sirve el upgrade kling-tcp/1 hacia los servicios del test.
type bancoTunel struct {
	*bancoIngreso
	mu       sync.Mutex
	tuneles  int
	puertos  []string
	eco      *httptest.Server
	cuerpoOK chan int
}

func montarTunel(t *testing.T, anuncia bool, mod func(*Opciones)) *bancoTunel {
	t.Helper()
	f := nuevoFalso(t)
	guest := servicioInvitado(t)
	bt := &bancoTunel{cuerpoOK: make(chan int, 4)}
	// Servicio que devuelve el cuerpo tal cual (binario, grande).
	bt.eco = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b)
	}))
	t.Cleanup(bt.eco.Close)
	puertoDe := func(s *httptest.Server) int {
		_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
		n, _ := strconv.Atoi(p)
		return n
	}
	backend := map[int]int{3000: puertoDe(guest), 3001: puertoDe(bt.eco)}
	f.ing.backend = backend
	sb := f.sembrar(&api.Machine{Labels: map[string]string{LabelTenant: "alice"}})

	base := f.rutas()
	m := http.NewServeMux()
	m.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		i := api.Info{Version: "fake"}
		if anuncia {
			i.Capabilities = []string{api.CapabilityTCPTunnel}
		}
		writeJSON(w, 200, i)
	})
	m.HandleFunc("GET /machines/{ref}/tcp", func(w http.ResponseWriter, r *http.Request) {
		if !anuncia {
			http.NotFound(w, r)
			return
		}
		pn, _ := strconv.Atoi(r.URL.Query().Get("port"))
		if !f.puertoExpuesto(r.PathValue("ref"), pn) {
			failDaemon(w, 403, fmt.Errorf("port %d is not published: list it in its %s label", pn, api.LabelPorts))
			return
		}
		real, ok := backend[pn]
		if !ok {
			failDaemon(w, 502, fmt.Errorf("nothing listens on %d", pn))
			return
		}
		dst, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(real)))
		if err != nil {
			failDaemon(w, 502, err)
			return
		}
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			dst.Close()
			return
		}
		bt.mu.Lock()
		bt.tuneles++
		bt.puertos = append(bt.puertos, r.URL.Query().Get("port"))
		bt.mu.Unlock()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + api.TCPProto + "\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		go func() { io.Copy(dst, rw); dst.Close() }()
		io.Copy(c, dst)
		c.Close()
	})
	m.Handle("/", base)
	dir, err := os.MkdirTemp("", "tn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "t.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	srvD := &httptest.Server{Listener: ln, Config: &http.Server{Handler: m}}
	srvD.Start()
	t.Cleanup(srvD.Close)

	o := Opciones{
		Hosts:   hosts.Nuevo(map[string]string{"a": sock}),
		Tenants: []Tenant{{Nombre: "alice", Token: tokenAlice}, {Nombre: "bob", Token: tokenBob}},
		Log:     testLog(t),
		Ingreso: IngresoOpciones{Dominio: dominioTest, Esquema: "http", Secreto: []byte("secreto-de-prueba-123")},
	}
	if mod != nil {
		mod(&o)
	}
	s, err := Nuevo(o)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	bt.bancoIngreso = &bancoIngreso{f: f, srv: srv, s: s, sb: sb, id: "a/" + sb.ID, guest: guest, puertoG: puertoDe(guest)}
	return bt
}

func (b *bancoTunel) nTuneles() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tuneles
}

func TestIngresoTunelHTTPyBinario(t *testing.T) {
	b := montarTunel(t, true, nil)
	cab := map[string]string{"Authorization": "Bearer " + tokenAlice}
	resp, body := b.get(t, 3000, "/x?y=2", cab)
	if resp.StatusCode != 200 || !strings.Contains(body, "path=/x?y=2") || strings.Contains(body, "Bearer") {
		t.Fatalf("HTTP por túnel: %d %q", resp.StatusCode, body)
	}
	if b.nTuneles() == 0 || b.f.ing.publicadas != 0 || b.f.ing.guestVisto != nil {
		t.Fatalf("debía ir por el túnel: túneles=%d publicadas=%d", b.nTuneles(), b.f.ing.publicadas)
	}
	// Cuerpo binario (no UTF-8) de 2 MiB: /guest lo rechazaría.
	bin := make([]byte, 2<<20)
	for i := range bin {
		bin[i] = byte(i*7 + 0xff)
	}
	req, _ := http.NewRequestWithContext(context.Background(), "POST", b.srv.URL+"/", bytes.NewReader(bin))
	req.Host = b.host(3001)
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Equal(got, bin) {
		t.Fatalf("eco binario: %d, %d bytes de %d", res.StatusCode, len(got), len(bin))
	}
	// Nadie escucha en el puerto: 502, no cuelgue.
	if r, _ := b.get(t, 4000, "/", cab); r.StatusCode != 502 {
		t.Fatalf("puerto cerrado = %d, quería 502", r.StatusCode)
	}
}

func upgradeWS(t *testing.T, b *bancoTunel, puerto int) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(b.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nAuthorization: Bearer %s\r\n\r\n", b.host(puerto), tokenAlice)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c, br, resp
}

func TestIngresoTunelWebSocket(t *testing.T) {
	b := montarTunel(t, true, nil)
	c, br, resp := upgradeWS(t, b, 3000)
	if resp.StatusCode != 101 {
		t.Fatalf("apretón: %d", resp.StatusCode)
	}
	for _, msg := range []string{"uno", "\x00\xff\xfe dos"} {
		fmt.Fprint(c, msg)
		buf := make([]byte, len("eco:")+len(msg))
		if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "eco:"+msg {
			t.Fatalf("eco de %q = %q, %v", msg, buf, err)
		}
	}
	if b.nTuneles() != 1 || b.f.ing.publicadas != 0 {
		t.Fatalf("túneles=%d publicadas=%d", b.nTuneles(), b.f.ing.publicadas)
	}
}

func TestIngresoTunelTopes(t *testing.T) {
	b := montarTunel(t, true, func(o *Opciones) { o.Ingreso.MaxCabeceras = 512; o.Ingreso.MaxConexiones = 1 })
	cab := map[string]string{"Authorization": "Bearer " + tokenAlice}
	if r, _ := b.get(t, 3000, "/", map[string]string{"Authorization": "Bearer " + tokenAlice, "X-Grande": strings.Repeat("x", 600)}); r.StatusCode != 431 {
		t.Fatalf("cabeceras grandes = %d, quería 431", r.StatusCode)
	}
	c, _, resp := upgradeWS(t, b, 3000)
	if resp.StatusCode != 101 {
		t.Fatalf("apretón: %d", resp.StatusCode)
	}
	if r, _ := b.get(t, 3000, "/", cab); r.StatusCode != 429 {
		t.Fatalf("segunda conexión = %d, quería 429", r.StatusCode)
	}
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if r, _ := b.get(t, 3000, "/", cab); r.StatusCode == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("el hueco no se liberó al cerrar el WebSocket del túnel")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIngresoSinCapacidadNoUsaTunel(t *testing.T) {
	// Sin tcp-tunnel: camino de siempre (publicar en 127.0.0.1 si es local).
	b := montarTunel(t, false, nil)
	cab := map[string]string{"Authorization": "Bearer " + tokenAlice}
	if r, body := b.get(t, 3000, "/", cab); r.StatusCode != 200 || !strings.Contains(body, "path=/") {
		t.Fatalf("sin capacidad: %d %q", r.StatusCode, body)
	}
	if b.nTuneles() != 0 || b.f.ing.publicadas != 1 {
		t.Fatalf("túneles=%d publicadas=%d", b.nTuneles(), b.f.ing.publicadas)
	}
	// Con SoloGuest (host remoto) manda /guest aunque el host anuncie el túnel.
	b2 := montarTunel(t, true, func(o *Opciones) { o.Ingreso.SoloGuest = true })
	if r, body := b2.get(t, 3000, "/p", cab); r.StatusCode != 201 || body != "via guest /p" {
		t.Fatalf("SoloGuest: %d %q", r.StatusCode, body)
	}
	if b2.nTuneles() != 0 {
		t.Fatal("SoloGuest no debe abrir túneles")
	}
}

// declararPuerto une a kling.ports sin pisar lo que había y respeta el tope.
func TestIngresoDeclaraPuertoSinPisar(t *testing.T) {
	b := montarTunel(t, true, nil)
	b.f.mu.Lock()
	b.f.maquinas[b.sb.ID].Labels[api.LabelPorts] = "9000, 5432"
	b.f.mu.Unlock()
	cab := map[string]string{"Authorization": "Bearer " + tokenAlice}
	if r, body := b.get(t, 3000, "/", cab); r.StatusCode != 200 {
		t.Fatalf("%d %q", r.StatusCode, body)
	}
	b.f.mu.Lock()
	got := b.f.maquinas[b.sb.ID].Labels[api.LabelPorts]
	b.f.mu.Unlock()
	if got != "9000,5432,3000" {
		t.Fatalf("kling.ports = %q, quería 9000,5432,3000", got)
	}
	// Al tope: no se declara otro puerto.
	var ps []string
	for i := 0; i < maxPuertosDeclarados; i++ {
		ps = append(ps, strconv.Itoa(20000+i))
	}
	b.f.mu.Lock()
	b.f.maquinas[b.sb.ID].Labels[api.LabelPorts] = strings.Join(ps, ",")
	b.f.mu.Unlock()
	if r, _ := b.get(t, 3001, "/", cab); r.StatusCode == 200 {
		t.Fatal("pasó con kling.ports lleno")
	}
}
