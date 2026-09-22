package main

// kling shell: una terminal interactiva dentro de una microVM.
//
// exec sirve para lanzar un comando y ver su salida; una shell necesita bytes en
// los dos sentidos hasta que alguien cuelga, y una terminal a cada lado. Este
// fichero pone la terminal local en modo crudo, la conecta a la sesión de
// tramas de pkg/api (ver shell.go allí) y se ocupa de que la terminal quede
// como estaba pase lo que pase: un `kling shell` que muere y deja la tty sin
// eco es lo primero que se recuerda de una herramienta.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/juan52878911/kindling/pkg/api"
)

// shellChunk es cuánto stdin va en cada trama. Más pequeño que ShellMaxFrame
// para que un pegado grande no se atasque detrás de una sola trama.
const shellChunk = 32 << 10

// errShellClosed es que la conexión se cortó sin que llegara ShellExit: el
// daemon o la máquina desaparecieron a media sesión.
var errShellClosed = errors.New("connection closed")

// cmdShell es `kling shell [-e K=V] [-w DIR] [-t TERM] <ref> [--] [cmd args...]`.
//
// Termina con el código de la shell remota, igual que exec: un `kling shell sb
// -- make test` sirve en un script tanto como fuera de él.
func cmdShell(args []string) (int, error) {
	fs := flag.NewFlagSet("shell", flag.ExitOnError)
	host := hostFlag(fs)
	var env stringsFlag
	fs.Var(&env, "e", "environment variable KEY=value (repeatable)")
	dir := fs.String("w", "", "working directory inside the machine")
	term := fs.String("t", "", "TERM seen by the program inside (default $TERM, or xterm)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: kling shell [-e K=V] [-w DIR] [-t TERM] <machine|sandbox> [--] [cmd [args...]]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fs.Usage()
		return 2, errors.New("missing machine")
	}
	ref, cmd := rest[0], rest[1:]
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	for _, kv := range env {
		if !strings.Contains(kv, "=") {
			return 2, fmt.Errorf("-e %q: use KEY=value", kv)
		}
	}
	if err := termUnsupported(); err != nil {
		return 1, err
	}
	if !isTerminal(os.Stdin.Fd()) || !isTerminal(os.Stdout.Fd()) {
		return 2, errors.New("kling shell needs a terminal on stdin and stdout; to feed input to a command use kling exec -i")
	}
	termName, err := resolveTerm(*term)
	if err != nil {
		return 2, err
	}
	// Sin tamaño, el daemon usa 24x80; no merece la pena fallar por eso.
	rows, cols, _ := winsize(os.Stdout.Fd())
	req := api.ShellRequest{Cmd: cmd, Dir: *dir, Env: env, Term: termName, Rows: rows, Cols: cols}

	// El contexto solo cubre el diálogo hasta el 101: después la conexión ya no
	// es del http.Client y cancelar no la cierra (ver api.Client.Shell).
	ctx, stop := ctxWithSignals()
	conn, err := api.NewClient(hostOf(*host)).Shell(ctx, ref, req)
	stop()
	if err != nil {
		if errors.Is(err, api.ErrShellUnsupported) {
			return 1, fmt.Errorf("%s does not support kling shell: the daemon or the image predate v0.7; rebuild the image or update the daemon", ref)
		}
		return 1, err
	}
	defer conn.Close()

	// El modo crudo se pone en stdin, que es donde importan ISIG/ICANON/ECHO;
	// OPOST va en la misma tty. Se pone DESPUÉS de conectar, para que un error
	// de conexión se imprima en una terminal normal.
	restore, err := makeRaw(os.Stdin.Fd())
	if err != nil {
		return 1, fmt.Errorf("raw mode: %w", err)
	}
	defer func() {
		// Primero restaurar, luego re-lanzar: el pánico se imprime en una
		// terminal legible y no se pierde.
		restore()
		if r := recover(); r != nil {
			panic(r)
		}
	}()

	done := make(chan struct{})
	defer close(done)
	sender := &shellSender{conn: conn}

	// Señales de fuera. SIGINT no llegará por Ctrl-C (ISIG está apagado; el
	// 0x03 viaja al PTY remoto), pero sí por un kill: en ese caso, como con
	// SIGTERM o SIGHUP, cerrar la conexión hace que el bucle principal salga
	// por el camino normal y la terminal se restaure en el defer. Nada de
	// os.Exit desde una goroutine: se saltaría los defers.
	var killedBy os.Signal
	var killedMu sync.Mutex
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
	defer signal.Stop(sigs)
	go func() {
		select {
		case s := <-sigs:
			killedMu.Lock()
			killedBy = s
			killedMu.Unlock()
			restore()
			conn.Close()
		case <-done:
		}
	}()

	// Cambios de tamaño: se vuelve a medir y se avisa al PTY remoto.
	winch := make(chan os.Signal, 1)
	notifyResize(winch)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-winch:
				if r, c, err := winsize(os.Stdout.Fd()); err == nil {
					_ = sender.send(api.ShellResize, api.ResizePayload(r, c))
				}
			case <-done:
				return
			}
		}
	}()

	// stdin hacia dentro. No se espera a esta goroutine al salir: un Read
	// sobre la tty no se puede desbloquear, y el proceso termina de todos modos.
	go func() { _ = pumpStdin(os.Stdin, sender.send) }()

	code, err := shellLoop(conn, os.Stdout)
	if errors.Is(err, errShellClosed) {
		killedMu.Lock()
		s := killedBy
		killedMu.Unlock()
		if s != nil {
			return 1, fmt.Errorf("terminated by %v", s)
		}
		return 1, fmt.Errorf("connection to %s closed", ref)
	}
	return code, err
}

// resolveTerm decide el TERM del programa de dentro. El de fuera es lo que
// hace que las teclas y los colores cuadren, pero viene del entorno del
// usuario y va a un JSON hacia el daemon: se limita a lo que un terminfo puede
// llamarse.
func resolveTerm(flagValue string) (string, error) {
	t := flagValue
	if t == "" {
		t = os.Getenv("TERM")
	}
	if t == "" {
		t = "xterm"
	}
	if !validTerm(t) {
		return "", fmt.Errorf("TERM %q: use letters, digits, '.', '-' and '_' (up to 64)", t)
	}
	return t, nil
}

func validTerm(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	for _, r := range t {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// shellSender serializa las escrituras: WriteFrame no es concurrente y aquí
// escriben stdin y los cambios de tamaño desde goroutines distintas.
type shellSender struct {
	mu   sync.Mutex
	conn *api.ShellConn
}

func (s *shellSender) send(t byte, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteFrame(t, p)
}

// pumpStdin manda lo que llega por r como tramas ShellData, de shellChunk como
// mucho. Termina en EOF o en el primer error de envío.
func pumpStdin(r io.Reader, send func(t byte, p []byte) error) error {
	buf := make([]byte, shellChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if serr := send(api.ShellData, buf[:n]); serr != nil {
				return serr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// frameReader es lo que necesita el bucle del lado que lee: en producción una
// *api.ShellConn; en los tests, un io.Pipe.
type frameReader interface {
	ReadFrame() (byte, []byte, error)
}

// shellLoop consume tramas hasta la última. Devuelve el código de salida de la
// shell remota; con ShellError, 1 y el texto; si la conexión se corta antes de
// ShellExit, 1 y errShellClosed. Las tramas que no van hacia fuera (resize,
// signal) o que no se conocen se ignoran, para que un daemon más nuevo pueda
// añadir tipos sin romper un CLI viejo.
func shellLoop(r frameReader, out io.Writer) (int, error) {
	for {
		t, p, err := r.ReadFrame()
		if err != nil {
			return 1, fmt.Errorf("%w: %v", errShellClosed, err)
		}
		switch t {
		case api.ShellData:
			if _, err := out.Write(p); err != nil {
				return 1, err
			}
		case api.ShellExit:
			code, err := api.ParseExit(p)
			if err != nil {
				return 1, err
			}
			return int(code), nil
		case api.ShellError:
			return 1, errors.New(string(p))
		case api.ShellPing:
		}
	}
}
