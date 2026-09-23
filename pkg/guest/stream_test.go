package guest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// streamExec llama al handler y devuelve la salida por flujo y el evento final.
func streamExec(t *testing.T, req api.ExecRequest) (stdout, stderr string, final api.ExecEvent, events int) {
	t.Helper()
	body, _ := json.Marshal(req)
	rec := httptest.NewRecorder()
	StreamExecHandler(nil)(rec, httptest.NewRequest(http.MethodPost, "/exec/stream", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var so, se strings.Builder
	sc := bufio.NewScanner(rec.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev api.ExecEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad event %q: %v", sc.Text(), err)
		}
		events++
		switch {
		case ev.Stream == "stdout":
			so.Write(ev.Data)
		case ev.Stream == "stderr":
			se.Write(ev.Data)
		default:
			final = ev
		}
	}
	return so.String(), se.String(), final, events
}

func TestStreamExecSeparaFlujosYDevuelveElCodigo(t *testing.T) {
	so, se, fin, _ := streamExec(t, api.ExecRequest{Cmd: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}})
	if so != "out\n" || se != "err\n" {
		t.Fatalf("stdout %q stderr %q", so, se)
	}
	if fin.Exit == nil || *fin.Exit != 3 || fin.Error != "" {
		t.Fatalf("final %+v", fin)
	}
}

func TestStreamExecStdinEntornoYDirectorio(t *testing.T) {
	dir := t.TempDir()
	so, _, fin, _ := streamExec(t, api.ExecRequest{
		Cmd:   []string{"sh", "-c", `read x; echo "$x $FOO $(pwd -P)"`},
		Stdin: []byte("hola\n"), Env: []string{"FOO=bar"}, Dir: dir,
	})
	if fin.Exit == nil || *fin.Exit != 0 {
		t.Fatalf("final %+v", fin)
	}
	if !strings.HasPrefix(so, "hola bar /") || !strings.HasSuffix(so, filepath.Base(dir)+"\n") {
		t.Fatalf("stdout %q", so)
	}
}

func TestStreamExecTruncaSinMatarAlProceso(t *testing.T) {
	// 200 KiB por stdout con un tope de 10 KiB: se cortan los datos, no el
	// proceso, que termina con 0 y lo dice después.
	so, _, fin, _ := streamExec(t, api.ExecRequest{
		Cmd:            []string{"sh", "-c", "head -c 204800 /dev/zero; echo fin >&2"},
		MaxOutputBytes: 10 << 10,
	})
	if len(so) != 10<<10 {
		t.Fatalf("stdout %d bytes, want %d", len(so), 10<<10)
	}
	if fin.Exit == nil || *fin.Exit != 0 || !fin.Truncated {
		t.Fatalf("final %+v", fin)
	}
}

func TestStreamExecPlazoMataAlGrupo(t *testing.T) {
	start := time.Now()
	// El sleep hijo de sh sostiene la tubería: si solo se matara a sh, Wait
	// esperaría los 30 s.
	_, _, fin, _ := streamExec(t, api.ExecRequest{Cmd: []string{"sh", "-c", "sleep 30; echo nunca"}, TimeoutSeconds: 1})
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %s: the timeout didn't kill the group", d)
	}
	if !fin.TimedOut || fin.Exit == nil || *fin.Exit == 0 {
		t.Fatalf("final %+v", fin)
	}
}

func TestStreamExecNoEjecutableEsError(t *testing.T) {
	_, _, fin, _ := streamExec(t, api.ExecRequest{Cmd: []string{"/no/existe"}})
	if fin.Error == "" || fin.Exit != nil {
		t.Fatalf("final %+v", fin)
	}
}

func TestStreamExecRechazaLimites(t *testing.T) {
	for _, req := range []api.ExecRequest{
		{},
		{Cmd: []string{"true"}, TimeoutSeconds: 3601},
		{Cmd: []string{"true"}, MaxOutputBytes: api.ExecMaxOutput + 1},
		{Cmd: []string{"true"}, Stdin: make([]byte, api.ExecMaxStdin+1)},
	} {
		body, _ := json.Marshal(req)
		rec := httptest.NewRecorder()
		StreamExecHandler(nil)(rec, httptest.NewRequest(http.MethodPost, "/exec/stream", bytes.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%+v: status %d, want 400", req.TimeoutSeconds, rec.Code)
		}
	}
}
