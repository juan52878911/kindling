package frontal

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// R4-8: el env de la creación vale para todo, no solo para la sesión "default".
func bancoEntorno(t *testing.T) (*falso, string, func(string, string, any) int) {
	t.Helper()
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base", Env: map[string]string{"TOKEN_X": "abc"}})
	pide2 := func(m, ruta string, cuerpo any) int {
		r := pide(t, srv, tokenAlice, m, "/v1/sandboxes/"+sb.ID+ruta, cuerpo)
		defer r.Body.Close()
		return r.StatusCode
	}
	return f, sb.ID, pide2
}

func TestEntornoExecSinSesionUsaLaPorDefecto(t *testing.T) {
	f, _, pide2 := bancoEntorno(t)
	if c := pide2("POST", "/exec?wait=1", api.ExecRequest{Cmd: []string{"env"}}); c != 200 {
		t.Fatalf("exec: %d", c)
	}
	f.mu.Lock()
	got := f.ultimoExec.Session
	f.mu.Unlock()
	if got != "default" {
		t.Errorf("un exec sin sesión corrió con session=%q, quería \"default\" (el env de la creación)", got)
	}
	// Una sesión explícita manda.
	pide2("POST", "/exec?wait=1", api.ExecRequest{Cmd: []string{"env"}, Session: "otra"})
	f.mu.Lock()
	got = f.ultimoExec.Session
	f.mu.Unlock()
	if got != "otra" {
		t.Errorf("session explícita pisada: %q", got)
	}
}

func TestEntornoProcesoDeFondo(t *testing.T) {
	f, _, pide2 := bancoEntorno(t)
	if c := pide2("POST", "/processes", api.ProcessStartRequest{Cmd: []string{"sleep", "1"}}); c != 201 {
		t.Fatalf("processes: %d", c)
	}
	f.procs.mu.Lock()
	got := f.procs.ultimoProc.Session
	f.procs.mu.Unlock()
	if got != "default" {
		t.Errorf("el proceso de fondo corrió con session=%q", got)
	}
}

func TestEntornoShellLeeLaSesion(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base", Env: map[string]string{"TOKEN_X": "abc"}})
	c, resp := abrirShell(t, srv, tokenAlice, sb.ID)
	if resp.StatusCode != 101 {
		t.Fatalf("shell: %d", resp.StatusCode)
	}
	defer c.conn.Close()
	f.mu.Lock()
	env := strings.Join(f.ultimoShell.Env, ",")
	f.mu.Unlock()
	if !strings.Contains(env, "TOKEN_X=abc") {
		t.Errorf("la shell no vio el env de la creación: %q", env)
	}
}

func TestEntornoCodigoLoLee(t *testing.T) {
	f := nuevoFalso(t)
	d := ponerReal(t, f)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base", Env: map[string]string{"TOKEN_X": "abc"}})
	b := &bancoCodigo{srv: srv, d: d, base: "/v1/sandboxes/" + sb.ID}
	b.contexto(t)
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.arranques) == 0 || d.arranques[0].Session != "default" {
		t.Errorf("el contexto de código arrancó sin la sesión del env: %+v", d.arranques)
	}
}

// Si la sesión caducó (24 h sin uso) el sandbox sigue sirviendo: se corre sin ella.
func TestEntornoSesionCaducadaNoRompeElExec(t *testing.T) {
	f, _, pide2 := bancoEntorno(t)
	f.mu.Lock()
	f.sesionesEstrictas = true
	f.mu.Unlock()
	f.procs.mu.Lock()
	delete(f.procs.sesiones, "default")
	f.procs.mu.Unlock()
	if c := pide2("POST", "/exec?wait=1", api.ExecRequest{Cmd: []string{"env"}}); c != 200 {
		t.Errorf("exec con la sesión por defecto caducada: %d, quería 200 sin sesión", c)
	}
}

// Sin env al crear, nada cambia: el exec sin sesión sigue sin sesión.
func TestEntornoSinEnvNoInventaSesion(t *testing.T) {
	f := nuevoFalso(t)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	r := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/exec?wait=1", api.ExecRequest{Cmd: []string{"env"}})
	r.Body.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ultimoExec.Session != "" {
		t.Errorf("session=%q sin haber pedido env", f.ultimoExec.Session)
	}
}
