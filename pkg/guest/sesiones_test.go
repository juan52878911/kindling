package guest

import (
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func syscallKill0(pid int) error { return syscall.Kill(pid, 0) }

func execSes(t *testing.T, ses string, cmd ...string) (string, string, api.ExecEvent) {
	t.Helper()
	so, se, fin, _ := streamExec(t, api.ExecRequest{Cmd: cmd, Session: ses})
	return so, se, fin
}

func TestSesionPersisteCwdYEntornoEntreExecs(t *testing.T) {
	srv := servidorProcs(t)
	dir := t.TempDir()
	var s api.Session
	if c := llamar(t, srv, http.MethodPost, "/sessions", api.SessionRequest{ID: "dev", Env: []string{"BASE=uno"}}, &s); c != http.StatusCreated {
		t.Fatalf("crear: %d", c)
	}

	// cd y export de un exec los ve el siguiente.
	_, _, fin := execSes(t, "dev", "sh", "-c", "cd "+dir+" && export NUEVA=dos && unset BASE")
	if fin.Exit == nil || *fin.Exit != 0 {
		t.Fatalf("final %+v", fin)
	}
	so, _, _ := execSes(t, "dev", "sh", "-c", `pwd; echo "$BASE-$NUEVA-${BASE-sin}"`)
	if so != dir+"\n-dos-sin\n" {
		t.Fatalf("segundo exec vio %q", so)
	}
	// argv sin shell también corre en el cwd de la sesión.
	so, _, _ = execSes(t, "dev", "pwd")
	if strings.TrimSpace(so) != dir {
		t.Fatalf("pwd = %q", so)
	}

	var got api.Session
	llamar(t, srv, http.MethodGet, "/sessions/dev", nil, &got)
	if got.Dir != dir || got.Execs != 3 || !contiene(got.Env, "NUEVA=dos") || !contiene(got.Unset, "BASE") || contiene(got.Env, "BASE=uno") {
		t.Fatalf("estado de la sesión %+v", got)
	}
	// Otra sesión (o ninguna) no lo ve: el estado no se filtra.
	so, _, _ = streamExecSolo(t, api.ExecRequest{Cmd: []string{"sh", "-c", `echo "${NUEVA-nada}"`}})
	if so != "nada\n" {
		t.Fatalf("sin sesión vio %q", so)
	}
}

func streamExecSolo(t *testing.T, req api.ExecRequest) (string, string, api.ExecEvent) {
	so, se, fin, _ := streamExec(t, req)
	return so, se, fin
}

func contiene(l []string, x string) bool {
	for _, v := range l {
		if v == x {
			return true
		}
	}
	return false
}

func TestSesionConservaElCodigoDeSalidaYSobreviveAExit(t *testing.T) {
	servidorProcs(t)
	crearSesion(api.SessionRequest{ID: "c"})
	_, _, fin := execSes(t, "c", "sh", "-c", "export K=v; exit 5")
	if fin.Exit == nil || *fin.Exit != 5 {
		t.Fatalf("código %+v", fin)
	}
	so, _, _ := execSes(t, "c", "sh", "-c", "echo $K")
	if so != "v\n" {
		t.Fatalf("tras exit 5 la sesión vio %q", so)
	}
}

func TestSesionNoMezclaStdoutDelUsuarioConElEstado(t *testing.T) {
	servidorProcs(t)
	crearSesion(api.SessionRequest{ID: "o"})
	so, se, _ := execSes(t, "o", "sh", "-c", "echo salida; echo error >&2")
	if so != "salida\n" || se != "error\n" {
		t.Fatalf("stdout %q stderr %q", so, se)
	}
}

func TestSesionInexistenteOCaducadaEsError(t *testing.T) {
	servidorProcs(t)
	_, _, fin := execSes(t, "fantasma", "true")
	if !strings.Contains(fin.Error, "no session") {
		t.Fatalf("final %+v", fin)
	}

	crearSesion(api.SessionRequest{ID: "corta", TTLSeconds: 1})
	base := ahora()
	ahora = func() time.Time { return base.Add(2 * time.Second) }
	defer func() { ahora = time.Now }()
	_, _, fin = execSes(t, "corta", "true")
	if !strings.Contains(fin.Error, "no session") {
		t.Fatalf("una sesión caducada se usó: %+v", fin)
	}
}

func TestSesionCadaExecRenuevaElPlazo(t *testing.T) {
	servidorProcs(t)
	crearSesion(api.SessionRequest{ID: "viva", TTLSeconds: 10})
	base := time.Now()
	defer func() { ahora = time.Now }()
	for i := 1; i <= 3; i++ {
		ahora = func() time.Time { return base.Add(time.Duration(i) * 8 * time.Second) }
		if _, ok := usarSesion("viva"); !ok {
			t.Fatalf("caducó en el paso %d pese a usarse", i)
		}
	}
}

func TestSesionTopeYValidacion(t *testing.T) {
	srv := servidorProcs(t)
	for nombre, c := range map[string]struct {
		body any
		want int
	}{
		"id raro":    {api.SessionRequest{ID: "a/b"}, 400},
		"env sin =":  {api.SessionRequest{Env: []string{"MAL"}}, 400},
		"ttl enorme": {api.SessionRequest{TTLSeconds: api.SessionMaxTTL + 1}, 400},
	} {
		if got := llamar(t, srv, http.MethodPost, "/sessions", c.body, nil); got != c.want {
			t.Errorf("%s: %d, want %d", nombre, got, c.want)
		}
	}
	if c := llamar(t, srv, http.MethodPost, "/sessions", api.SessionRequest{ID: "r"}, nil); c != 201 {
		t.Fatalf("crear r: %d", c)
	}
	if c := llamar(t, srv, http.MethodPost, "/sessions", api.SessionRequest{ID: "r"}, nil); c != 409 {
		t.Fatalf("id repetido: %d, want 409", c)
	}
	for i := 0; i < api.SessionMax-1; i++ {
		crearSesion(api.SessionRequest{})
	}
	if c := llamar(t, srv, http.MethodPost, "/sessions", api.SessionRequest{}, nil); c != http.StatusTooManyRequests {
		t.Errorf("sesión de más: %d, want 429", c)
	}
	if c := llamar(t, srv, http.MethodDelete, "/sessions/r", nil, nil); c != http.StatusNoContent {
		t.Errorf("borrar: %d", c)
	}
	if c := llamar(t, srv, http.MethodDelete, "/sessions/r", nil, nil); c != http.StatusNotFound {
		t.Errorf("borrar dos veces: %d", c)
	}
}

func TestProcesoDeFondoHeredaLaSesionSinModificarla(t *testing.T) {
	srv := servidorProcs(t)
	dir := t.TempDir()
	crearSesion(api.SessionRequest{ID: "bg", Dir: dir, Env: []string{"QUIEN=sesion"}})
	p := lanzar(t, srv, api.ProcessStartRequest{Session: "bg", Cmd: []string{"sh", "-c", `pwd; echo $QUIEN; cd /; export QUIEN=otro`}})
	esperar(t, srv, p.PID)
	out, _ := salida(t, srv, p.PID, "")
	if out != dir+"\nsesion\n" {
		t.Fatalf("salida %q", out)
	}
	var s api.Session
	llamar(t, srv, http.MethodGet, "/sessions/bg", nil, &s)
	if s.Dir != dir || s.Execs != 0 {
		t.Fatalf("un proceso de fondo cambió la sesión: %+v", s)
	}
}

func TestGuionDeCitaArgvYRespetaShellDashC(t *testing.T) {
	if g := guionDe([]string{"sh", "-c", "cd /tmp && ls"}); g != "cd /tmp && ls" {
		t.Errorf("sh -c = %q", g)
	}
	if g := guionDe([]string{"echo", "it's", "a b"}); g != `'echo' 'it'\''s' 'a b'` {
		t.Errorf("argv = %q", g)
	}
}
