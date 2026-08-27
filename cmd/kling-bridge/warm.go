package main

// Hijo CALIENTE sin ligar: el arranque del runtime se paga antes de congelar,
// no al despertar.
//
// EL PROBLEMA. Restaurar la microVM desde el dorado cuesta ~26 ms, pero la
// primera sesión pagaba además el arranque completo del runtime (~8 s de node
// resolviendo e importando), porque /reset —necesario para que el dorado no se
// congele en estado post-handshake— mataba a TODOS los hijos y el dorado se
// congelaba con cero procesos del servidor.
//
// LA SOLUCIÓN. /reset deja un hijo ARRANCADO pero SIN LIGAR a ninguna sesión:
// un servidor MCP de stdio que aún no ha recibido `initialize` no tiene estado
// de conversación, así que es tan limpio como uno recién lanzado. El dorado se
// congela con ese hijo ya listo dentro, y el primer `initialize` tras restaurar
// lo ADOPTA en vez de lanzar otro: el despertar deja de pagar el runtime.
//
// POR QUÉ LA ADOPCIÓN ES SEGURA. El protocolo permite pings ANTES del
// handshake ("The client SHOULD NOT send requests other than pings before the
// server has responded to the initialize request"), y los dos SDK oficiales
// (TypeScript y Python) los contestan sin tocar su máquina de estados: se
// comprobó contra ambos que ping → initialize → tools/list funciona idéntico a
// initialize a secas. Ese ping es lo ÚNICO que el hijo caliente recibe, y
// cumple dos papeles: es la señal de "el runtime ya arrancó" —sin él, el
// commit congelaría un node a medio boot y el despertar seguiría pagando el
// resto— y es la prueba de que el bucle JSON-RPC está vivo.
//
// EL DETERMINISMO MANDA. Si algo de esto no se cumple —el servidor no contesta
// el ping, lo contesta con error, o el hijo muere mientras espera— NO se
// adopta: se descarta y la sesión lanza un hijo nuevo, que es exactamente el
// comportamiento de siempre. Lento y correcto antes que rápido a veces.
//
// LO QUE NO HACE. En modo proxy (transport:http) no hace falta: /reset ya
// reinicia el hijo compartido y espera su puerto, así que ese dorado se congela
// caliente de serie. En modo navegador se DESACTIVA: el arranque perezoso de
// Chromium es deliberado (el dorado debe congelarse sin él), y un hijo con los
// session_args de CDP apuntando a un navegador parado no es adoptable.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// warmupPingID es el id JSON-RPC del ping de calentamiento. Con prefijo propio
// para que en un log se distinga de cualquier id de cliente; no puede chocar
// con los de las sesiones porque el ping se consume ANTES de ligar el hijo.
const warmupPingID = "kling-warmup"

// warmupTimeout acota la espera del pong. Mismo plazo que waitForPort en el
// modo proxy: generoso frente a un runtime lento, pero finito para que un
// servidor que no contesta pings no cuelgue el /reset del commit para siempre.
const warmupTimeout = 40 * time.Second

// warmChild es un proceso del servidor MCP arrancado y verificado pero SIN
// ligar a sesión. Su stdin solo ha visto el ping de calentamiento; su stdout
// se guarda tras el bufio.Reader del warmup para que ningún byte que el
// servidor emitiera después del pong se pierda al adoptarlo.
type warmChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	exitCh chan syscall.WaitStatus
}

// prewarm lanza un hijo con el entorno BASE (sin secretos de sesión: aún no hay
// sesión) y espera a que conteste el ping. Si no llega a estar listo, lo mata y
// devuelve error: mejor un dorado lento y correcto que uno congelado a medias.
func (b *bridge) prewarm() (*warmChild, error) {
	cmd := exec.Command(b.argv[0], b.argv[1:]...)
	cmd.Env = b.env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr // como en spawn: el log del servidor, a la consola serie

	exitCh, err := procReaper.startTracked(cmd)
	if err != nil {
		return nil, fmt.Errorf("could not start the MCP server to prewarm: %w", err)
	}
	w := &warmChild{cmd: cmd, stdin: stdin,
		out: bufio.NewReaderSize(stdout, 64<<10), exitCh: exitCh}
	log.Printf("prewarm: MCP server started (pid %d), waiting for its pong", cmd.Process.Pid)
	if err := w.waitReady(warmupTimeout); err != nil {
		w.discard()
		return nil, err
	}
	log.Printf("prewarm: MCP server ready (pid %d), unbound until the first initialize", cmd.Process.Pid)
	return w, nil
}

// waitReady envía el ping de calentamiento y espera el pong.
//
// Solo un RESULT vale. Un servidor que contesta el ping con un error JSON-RPC
// se sale del protocolo —el ping "MUST respond promptly"— y no hay forma de
// saber qué más hace mal: se descarta en vez de apostar a que su initialize
// posterior funcione. Es la regla de la casa: determinismo antes que velocidad.
func (w *warmChild) waitReady(timeout time.Duration) error {
	ping := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%q,\"method\":\"ping\"}\n", warmupPingID)
	if _, err := w.stdin.Write([]byte(ping)); err != nil {
		return fmt.Errorf("writing the warmup ping: %w", err)
	}

	// El lector corre aparte porque ReadString no admite plazo. Encuentra el
	// pong y TERMINA: a partir de ahí nadie lee w.out hasta que la adopción
	// arranque el readLoop de la sesión, y lo que quede en el búfer se conserva.
	got := make(chan error, 1)
	go func() {
		for {
			line, err := w.out.ReadString('\n')
			if t := strings.TrimSpace(line); t != "" {
				var msg struct {
					ID  json.RawMessage `json:"id"`
					Err json.RawMessage `json:"error"`
				}
				if json.Unmarshal([]byte(t), &msg) == nil && string(msg.ID) == `"`+warmupPingID+`"` {
					if len(msg.Err) != 0 && string(msg.Err) != "null" {
						got <- fmt.Errorf("the MCP server rejected the pre-initialize ping: %s", msg.Err)
					} else {
						got <- nil
					}
					return
				}
				// Cualquier otra línea se descarta: antes del handshake un
				// servidor no debería iniciar nada, y aún no hay sesión ni
				// flujo SSE al que entregarla.
			}
			if err != nil {
				got <- fmt.Errorf("the MCP server closed stdout before answering the warmup ping: %w", err)
				return
			}
		}
	}()

	select {
	case err := <-got:
		return err
	case <-time.After(timeout):
		// El lector sigue bloqueado en ReadString; el discard del llamante mata
		// al hijo, eso cierra la tubería y el lector termina solo.
		return fmt.Errorf("the MCP server did not answer the warmup ping within %s "+
			"(it may not support pings before initialize); prewarming disabled for this freeze", timeout)
	}
}

// alive dice si el hijo caliente sigue vivo. Dos comprobaciones: el canal del
// cosechador (que como PID 1 recoge las muertes en cuanto ocurren) y la señal
// cero por si el proceso desapareció sin pasar por él. Un caliente muerto NO se
// adopta jamás: la sesión pagaría el arranque igualmente, pero con un 502 antes.
func (w *warmChild) alive() bool {
	select {
	case ws := <-w.exitCh:
		// Se devuelve al canal (buffer 1, nadie más escribe: el cosechador ya
		// lo olvidó) para que el waitFor del discard recupere el estado.
		w.exitCh <- ws
		return false
	default:
	}
	return w.cmd.Process != nil && w.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// discard mata y recoge un hijo caliente que no se va a adoptar. Mismo baile
// que el close de una sesión: waitFor por si el cosechador se adelantó, y
// forget para no acumular entradas.
func (w *warmChild) discard() {
	_ = w.stdin.Close()
	if w.cmd.Process != nil {
		matarGrupo(w.cmd)
	}
	err := waitFor(w.cmd, w.exitCh)
	if w.cmd.Process != nil {
		procReaper.forget(w.cmd.Process.Pid)
	}
	if err != nil {
		log.Printf("prewarm: unbound MCP server discarded (%v)", err)
		return
	}
	log.Printf("prewarm: unbound MCP server discarded")
}

// replenishWarm deja el puente con UN hijo caliente sin ligar, arrancando uno
// si no lo hay o si el que había murió. Lo llama /reset justo antes de
// contestar: el 204 es la señal de "listo para congelar", y tiene que serlo
// también del hijo caliente, o el commit congelaría un runtime a medio boot.
func (b *bridge) replenishWarm() error {
	// En modo navegador no se precalienta: el dorado debe congelarse sin
	// Chromium (arranque perezoso deliberado), y un hijo lanzado con los
	// session_args de CDP hacia un navegador parado no es un hijo limpio.
	// La guarda por sessionArgs cubre también a quien los use en el futuro.
	if b.browser != nil || len(b.sessionArgs) > 0 {
		return nil
	}

	b.mu.Lock()
	if w := b.warm; w != nil {
		if w.alive() {
			// El que hay sigue limpio por construcción (nunca se le escribió
			// más que el ping): no hay nada que reponer.
			b.mu.Unlock()
			return nil
		}
		b.warm = nil
		go w.discard()
	}
	b.mu.Unlock()

	// El arranque va SIN el candado: tarda lo que tarde el runtime, y un
	// initialize que llegue mientras tanto debe poder crear su sesión fría.
	w, err := b.prewarm()
	if err != nil {
		return err
	}

	b.mu.Lock()
	if b.warm != nil && b.warm.alive() {
		// Otro /reset concurrente repuso antes. Con un caliente basta.
		b.mu.Unlock()
		w.discard()
		return nil
	}
	old := b.warm
	b.warm = w
	b.mu.Unlock()
	if old != nil {
		go old.discard()
	}
	return nil
}

// adoptWarmLocked entrega el hijo caliente a la sesión que nace, si hay uno y
// es adoptable. Se llama con b.mu tomado, desde spawn.
//
// Las condiciones son deliberadamente estrictas, porque un hijo adoptado tiene
// que ser INDISTINGUIBLE de uno recién lanzado para esta sesión:
//   - sin secretos MMDS: el caliente se lanzó con el entorno base, y el entorno
//     de un proceso no se puede cambiar después de exec. Si esta sesión trae
//     secretos, el caliente no los tiene y además ya no los tendrá nunca: se
//     descarta para no retener un proceso inadoptable.
//   - vivo: un caliente que murió esperando solo aportaría un 502.
func (b *bridge) adoptWarmLocked(id string, secrets bool) *session {
	w := b.warm
	if w == nil {
		return nil
	}
	b.warm = nil
	if secrets || !w.alive() {
		go w.discard()
		return nil
	}
	s := &session{
		id: id, cmd: w.cmd, stdin: w.stdin, lastUse: time.Now(),
		reqTimeout: b.reqTimeout,
		exitCh:     w.exitCh,
		pending:    map[string]chan json.RawMessage{},
		notes:      make(chan json.RawMessage, 32),
		done:       make(chan struct{}),
	}
	// El readLoop arranca sobre el MISMO bufio.Reader del warmup: si el
	// servidor emitió algo después del pong, sigue en el búfer y no se pierde.
	go s.readLoop(w.out)
	log.Printf("session %s: adopted the prewarmed MCP server (pid %d)", s.id[:8], w.cmd.Process.Pid)
	return s
}
