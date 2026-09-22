package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// pipeFrames es un frameReader sobre un io.Pipe: el test escribe tramas por un
// extremo y shellLoop las lee por el otro, sin terminal ni daemon.
type pipeFrames struct{ r io.Reader }

func (p pipeFrames) ReadFrame() (byte, []byte, error) { return api.ReadFrame(p.r) }

// feed escribe las tramas en una goroutine y luego hace con el escritor lo
// que se le diga (cerrarlo, o cerrarlo con error).
func feed(t *testing.T, frames [][2]any, end func(*io.PipeWriter)) frameReader {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		for _, f := range frames {
			if err := api.WriteFrame(pw, f[0].(byte), f[1].([]byte)); err != nil {
				return
			}
		}
		end(pw)
	}()
	return pipeFrames{pr}
}

func TestShellLoopDataAndExit(t *testing.T) {
	var out bytes.Buffer
	r := feed(t, [][2]any{
		{api.ShellData, []byte("hola ")},
		{api.ShellPing, []byte(nil)},
		{api.ShellData, []byte("mundo\r\n")},
		{api.ShellExit, api.ExitPayload(7)},
	}, func(w *io.PipeWriter) { w.Close() })
	code, err := shellLoop(r, &out)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if code != 7 {
		t.Errorf("code = %d, want 7", code)
	}
	if out.String() != "hola mundo\r\n" {
		t.Errorf("out = %q", out.String())
	}
}

func TestShellLoopExitZero(t *testing.T) {
	r := feed(t, [][2]any{{api.ShellExit, api.ExitPayload(0)}}, func(w *io.PipeWriter) { w.Close() })
	if code, err := shellLoop(r, io.Discard); code != 0 || err != nil {
		t.Errorf("got %d, %v", code, err)
	}
}

func TestShellLoopError(t *testing.T) {
	var out bytes.Buffer
	r := feed(t, [][2]any{
		{api.ShellData, []byte("antes")},
		{api.ShellError, []byte("no pty for you")},
	}, func(w *io.PipeWriter) { w.Close() })
	code, err := shellLoop(r, &out)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if err == nil || !strings.Contains(err.Error(), "no pty for you") {
		t.Errorf("err = %v", err)
	}
	if errors.Is(err, errShellClosed) {
		t.Error("a ShellError is not a dropped connection")
	}
	if out.String() != "antes" {
		t.Errorf("out = %q", out.String())
	}
}

// Una conexión que se corta sin ShellExit no puede devolver 0: quien lo use en
// un script tiene que enterarse de que la shell no terminó.
func TestShellLoopClosedWithoutExit(t *testing.T) {
	for name, end := range map[string]func(*io.PipeWriter){
		"eof":   func(w *io.PipeWriter) { w.Close() },
		"error": func(w *io.PipeWriter) { w.CloseWithError(errors.New("reset by peer")) },
	} {
		t.Run(name, func(t *testing.T) {
			r := feed(t, [][2]any{{api.ShellData, []byte("x")}, {api.ShellPing, []byte(nil)}}, end)
			code, err := shellLoop(r, io.Discard)
			if code == 0 {
				t.Error("code 0 for a dropped connection")
			}
			if !errors.Is(err, errShellClosed) {
				t.Errorf("err = %v, want errShellClosed", err)
			}
		})
	}
}

// Una trama truncada a mitad (el daemon murió escribiendo) también es un corte.
func TestShellLoopTruncatedFrame(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte{api.ShellData, 0, 0, 0, 10, 'a', 'b'})
		pw.Close()
	}()
	code, err := shellLoop(pipeFrames{pr}, io.Discard)
	if code == 0 || !errors.Is(err, errShellClosed) {
		t.Errorf("got %d, %v", code, err)
	}
}

func TestShellLoopUnknownFrameIgnored(t *testing.T) {
	var out bytes.Buffer
	r := feed(t, [][2]any{
		{byte(200), []byte("future")},
		{api.ShellResize, api.ResizePayload(1, 2)}, // no va hacia fuera; se ignora
		{api.ShellData, []byte("ok")},
		{api.ShellExit, api.ExitPayload(3)},
	}, func(w *io.PipeWriter) { w.Close() })
	code, err := shellLoop(r, &out)
	if code != 3 || err != nil || out.String() != "ok" {
		t.Errorf("got %d, %v, %q", code, err, out.String())
	}
}

// stdin se trocea a shellChunk: nunca una trama mayor, y los bytes llegan
// enteros y en orden.
func TestPumpStdinChunks(t *testing.T) {
	in := bytes.Repeat([]byte("k"), 2*shellChunk+123)
	var sizes []int
	var got bytes.Buffer
	err := pumpStdin(bytes.NewReader(in), func(typ byte, p []byte) error {
		if typ != api.ShellData {
			t.Errorf("frame type %d", typ)
		}
		sizes = append(sizes, len(p))
		got.Write(p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range sizes {
		if n > shellChunk || n > api.ShellMaxFrame {
			t.Errorf("frame of %d bytes", n)
		}
	}
	if !bytes.Equal(got.Bytes(), in) {
		t.Error("stdin arrived altered")
	}
	if len(sizes) != 3 {
		t.Errorf("%d frames, want 3: %v", len(sizes), sizes)
	}
}

func TestPumpStdinStopsOnSendError(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	err := pumpStdin(strings.NewReader(strings.Repeat("x", 3*shellChunk)), func(byte, []byte) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Errorf("err %v after %d calls", err, calls)
	}
}

func TestValidTerm(t *testing.T) {
	for in, want := range map[string]bool{
		"xterm":                        true,
		"xterm-256color":               true,
		"screen.linux":                 true,
		"tmux_x":                       true,
		"":                             false,
		"xterm;rm -rf /":               false,
		"a b":                          false,
		"ñ":                            false,
		strings.Repeat("x", 64):        true,
		strings.Repeat("x", 65):        false,
		"xterm\n":                      false,
		"../../usr/share/terminfo/x/y": false,
	} {
		if got := validTerm(in); got != want {
			t.Errorf("validTerm(%q) = %v", in, got)
		}
	}
}

func TestResolveTerm(t *testing.T) {
	t.Setenv("TERM", "")
	if got, err := resolveTerm(""); got != "xterm" || err != nil {
		t.Errorf("empty env: %q, %v", got, err)
	}
	t.Setenv("TERM", "screen-256color")
	if got, _ := resolveTerm(""); got != "screen-256color" {
		t.Errorf("from env: %q", got)
	}
	if got, _ := resolveTerm("vt100"); got != "vt100" {
		t.Errorf("flag wins: %q", got)
	}
	if _, err := resolveTerm("bad term"); err == nil {
		t.Error("invalid TERM accepted")
	}
	t.Setenv("TERM", "bad$env")
	if _, err := resolveTerm(""); err == nil {
		t.Error("invalid $TERM accepted")
	}
}
