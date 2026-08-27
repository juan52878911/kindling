package main

import (
	"bufio"
	"context"
	"net/http/httptest"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeMCPArgv imita lo mínimo de un servidor MCP de stdio que el warmup
// necesita: contesta el ping de calentamiento y después hace de eco (como los
// tests de sesión, que usan `cat`: la respuesta llega con el id de la petición
// y el readLoop la casa).
var fakeMCPArgv = []string{"sh", "-c",
	`read line; printf '{"jsonrpc":"2.0","id":"kling-warmup","result":{}}\n'; exec cat`}

func testBridge(argv ...string) *bridge {
	if len(argv) == 0 {
		argv = fakeMCPArgv
	}
	return &bridge{argv: argv, idle: time.Minute, maxSessions: 4,
		reqTimeout: time.Minute, sessions: map[string]*session{}}
}

// El ciclo completo: prewarm espera el pong, spawn ADOPTA al caliente en vez de
// lanzar otro proceso, y la sesión adoptada conversa con normalidad (lo que
// quedara en el búfer del warmup incluido).
func TestPrewarmYAdopcion(t *testing.T) {
	b := testBridge()
	w, err := b.prewarm()
	if err != nil {
		t.Fatalf("prewarm falló con un servidor que sí contesta el ping: %v", err)
	}
	b.warm = w
	warmPid := w.cmd.Process.Pid

	s, created, err := b.resolve("", true)
	if err != nil || !created {
		t.Fatalf("resolve(initialize) falló: %v", err)
	}
	defer s.close()
	if s.cmd.Process.Pid != warmPid {
		t.Fatalf("la sesión lanzó un proceso nuevo (pid %d) en vez de adoptar al caliente (pid %d)",
			s.cmd.Process.Pid, warmPid)
	}
	if b.warm != nil {
		t.Error("el caliente sigue colgado del puente después de adoptarlo")
	}

	// La sesión adoptada tiene que hablar: el eco devuelve la petición con su
	// mismo id y el readLoop —que hereda el bufio.Reader del warmup— la casa.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := s.request(ctx, `"1"`, []byte(`{"jsonrpc":"2.0","id":"1","method":"tools/list"}`))
	if err != nil {
		t.Fatalf("la sesión adoptada no contesta: %v", err)
	}
	if len(resp) == 0 {
		t.Fatal("respuesta vacía de la sesión adoptada")
	}
}

// Un servidor que contesta el ping con un error JSON-RPC se sale del protocolo
// y no se puede dar por limpio: el prewarm debe rechazarlo, no congelarlo.
func TestPrewarmRechazaUnPongConError(t *testing.T) {
	b := testBridge("sh", "-c",
		`read line; printf '{"jsonrpc":"2.0","id":"kling-warmup","error":{"code":-32601,"message":"nope"}}\n'; exec cat`)
	if _, err := b.prewarm(); err == nil {
		t.Fatal("prewarm aceptó un servidor que rechaza el ping pre-initialize")
	}
}

// Un servidor que no contesta el ping agota el plazo y el prewarm falla limpio:
// el /reset sigue valiendo, solo que sin caliente.
func TestWaitReadyVenceSiNadieContesta(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	exitCh, err := procReaper.startTracked(cmd)
	if err != nil {
		t.Skipf("no puedo lanzar sleep aquí: %v", err)
	}
	w := &warmChild{cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout), exitCh: exitCh}
	defer w.discard()

	err = w.waitReady(150 * time.Millisecond)
	if err == nil {
		t.Fatal("waitReady dio por listo a un servidor que no contestó nada")
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("el error no explica qué pasó: %v", err)
	}
}

// Una sesión con secretos MMDS no puede adoptar: el caliente se lanzó con el
// entorno base y el entorno de un proceso no se cambia después de exec. Además
// el caliente se descarta, porque ya nunca va a llevar esos secretos.
func TestAdopcionVetadaConSecretosDeSesion(t *testing.T) {
	b := testBridge()
	w, err := b.prewarm()
	if err != nil {
		t.Fatalf("prewarm falló: %v", err)
	}
	b.warm = w

	b.mu.Lock()
	s := b.adoptWarmLocked("abcdef0123456789", true)
	b.mu.Unlock()
	if s != nil {
		t.Fatal("adoptó un caliente sin los secretos que la sesión exige")
	}
	if b.warm != nil {
		t.Error("el caliente inadoptable sigue retenido en vez de descartarse")
	}
	// El descarte corre aparte: se espera a que el proceso muera de verdad.
	for i := 0; i < 100 && w.alive(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if w.alive() {
		t.Error("el caliente descartado sigue vivo: proceso fugado")
	}
}

// Un caliente que murió esperando no se adopta: la sesión lanzaría su petición
// contra un proceso muerto y vería un 502 en vez de pagar el arranque y ya.
func TestAdopcionVetadaSiElCalienteMurio(t *testing.T) {
	b := testBridge()
	w, err := b.prewarm()
	if err != nil {
		t.Fatalf("prewarm falló: %v", err)
	}
	b.warm = w
	// La muerte se simula como la vería el puente en producción: el cosechador
	// (PID 1) recoge al hijo y deja su estado en el canal.
	_ = w.cmd.Process.Kill()
	w.exitCh <- syscall.WaitStatus(9)

	b.mu.Lock()
	s := b.adoptWarmLocked("abcdef0123456789", false)
	b.mu.Unlock()
	if s != nil {
		s.close()
		t.Fatal("adoptó un caliente muerto: la primera petición fallaría")
	}
	if b.warm != nil {
		t.Error("el caliente muerto sigue retenido")
	}
}

// /reset deja el puente sin sesiones pero CON un hijo caliente sin ligar, que
// es exactamente lo que el dorado debe congelar. Y el caliente no cuenta como
// sesión: con tope 1 y cero sesiones, el initialize siguiente entra (y adopta).
func TestResetDejaUnCalienteSinLigar(t *testing.T) {
	b := testBridge()
	b.maxSessions = 1
	s := newTestSession(t)
	b.sessions[s.id] = s

	rec := httptest.NewRecorder()
	b.handleReset(rec, httptest.NewRequest("POST", "/reset", nil))
	if rec.Code != 204 {
		t.Fatalf("reset devolvió %d", rec.Code)
	}
	if len(b.sessions) != 0 {
		t.Fatalf("quedaron %d sesiones tras el reset", len(b.sessions))
	}
	if b.warm == nil {
		t.Fatal("el reset no dejó ningún hijo caliente")
	}
	if !b.warm.alive() {
		t.Fatal("el caliente del reset está muerto")
	}
	warmPid := b.warm.cmd.Process.Pid

	// Tope 1 y cero sesiones contadas: si el caliente contara como sesión, este
	// initialize se rechazaría.
	s2, _, err := b.resolve("", true)
	if err != nil {
		t.Fatalf("el caliente cuenta contra el tope de sesiones: %v", err)
	}
	defer s2.close()
	if s2.cmd.Process.Pid != warmPid {
		t.Errorf("el initialize tras el reset no adoptó al caliente (pid %d != %d)",
			s2.cmd.Process.Pid, warmPid)
	}
}

// Dos resets seguidos no acumulan calientes ni matan al que ya está listo: el
// que hay sigue limpio por construcción y se conserva.
func TestResetConservaElCalienteQueYaHay(t *testing.T) {
	b := testBridge()
	rec := httptest.NewRecorder()
	b.handleReset(rec, httptest.NewRequest("POST", "/reset", nil))
	if b.warm == nil {
		t.Fatal("el primer reset no dejó caliente")
	}
	pid := b.warm.cmd.Process.Pid

	rec = httptest.NewRecorder()
	b.handleReset(rec, httptest.NewRequest("POST", "/reset", nil))
	if rec.Code != 204 || b.warm == nil {
		t.Fatalf("el segundo reset rompió el caliente (código %d)", rec.Code)
	}
	if b.warm.cmd.Process.Pid != pid {
		t.Errorf("el segundo reset relanzó el caliente (pid %d -> %d) sin necesidad",
			pid, b.warm.cmd.Process.Pid)
	}
	b.mu.Lock()
	w := b.warm
	b.warm = nil
	b.mu.Unlock()
	w.discard()
}

// En modo navegador no se precalienta: el dorado debe congelarse sin Chromium,
// y un hijo con los session_args de CDP hacia un navegador parado no es limpio.
func TestResetNoPrecalientaEnModoNavegador(t *testing.T) {
	b := testBridge()
	b.browser = &browserSpec{Sidecar: []string{"true"}}
	b.sessionArgs = []string{"--cdp-endpoint=ws://127.0.0.1:9222"}

	rec := httptest.NewRecorder()
	b.handleReset(rec, httptest.NewRequest("POST", "/reset", nil))
	if rec.Code != 204 {
		t.Fatalf("reset devolvió %d", rec.Code)
	}
	if b.warm != nil {
		t.Fatal("el reset precalentó un hijo en modo navegador")
	}
}

// El reaper de ociosas no ve al caliente: no vive en sessions, así que por
// mucho que pase el tiempo sin sesiones, sigue ahí para el primer initialize.
func TestElReaperNoCosechaAlCaliente(t *testing.T) {
	b := testBridge()
	b.idle = 20 * time.Millisecond
	w, err := b.prewarm()
	if err != nil {
		t.Fatalf("prewarm falló: %v", err)
	}
	b.warm = w
	defer w.discard()

	ctx, cancel := context.WithCancel(context.Background())
	go b.reapIdle(ctx)
	time.Sleep(150 * time.Millisecond) // varias vueltas del reaper
	cancel()

	if !w.alive() {
		t.Fatal("el reaper de ociosas mató al hijo caliente")
	}
}

// closeAll también se lleva al caliente: somos PID 1 y el apagado no debe dejar
// procesos sueltos dentro de la microVM.
func TestCloseAllDescartaElCaliente(t *testing.T) {
	b := testBridge()
	w, err := b.prewarm()
	if err != nil {
		t.Fatalf("prewarm falló: %v", err)
	}
	b.warm = w

	b.closeAll()
	if b.warm != nil {
		t.Error("closeAll dejó el caliente colgado del puente")
	}
	if w.alive() {
		t.Error("closeAll no mató al hijo caliente: proceso fugado")
	}
}

// El precalentamiento se puede desactivar por peticion, y eso NO debe romper el
// reset: sigue cerrando sesiones y contestando 204, solo que sin dejar caliente.
//
// Importa porque el hijo caliente vive dentro del dorado y lo engorda —39 MB a
// 120 MB, medido—: a escala de decenas de servicios el disco puede pesar mas que
// medio segundo de despertar, y quien lo tenga debe poder elegir.
func TestElPrecalentamientoSePuedeDesactivar(t *testing.T) {
	b := testBridge()

	// Con warm=0 no queda caliente, pero el reset es valido.
	rec := httptest.NewRecorder()
	b.handleReset(rec, httptest.NewRequest("POST", "/reset?warm=0", nil))
	if rec.Code != 204 {
		t.Fatalf("el reset debe seguir devolviendo 204, dio %d", rec.Code)
	}
	b.mu.Lock()
	caliente := b.warm
	b.mu.Unlock()
	if caliente != nil {
		t.Error("con warm=0 no debería quedar un hijo caliente")
	}

	// Sin el parametro, el comportamiento por defecto no cambia.
	rec = httptest.NewRecorder()
	b.handleReset(rec, httptest.NewRequest("POST", "/reset", nil))
	if rec.Code != 204 {
		t.Fatalf("204 esperado, dio %d", rec.Code)
	}
	b.mu.Lock()
	caliente = b.warm
	b.mu.Unlock()
	if caliente == nil {
		t.Error("por defecto sí debería precalentar")
	}
}
