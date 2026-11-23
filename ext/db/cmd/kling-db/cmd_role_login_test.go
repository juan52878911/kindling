package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
)

// loginKling añade al falso de kling db role lo que toca -login: los
// atributos del rol en pg_authid (LOGIN y verificador) y las sesiones que se
// cortan.
type loginKling struct {
	*roleKling
	login      map[string]bool
	verifier   map[string]string
	terminated []string // SQL de cada corte de sesiones
	// failAlter: el ALTER ROLE de -login se aplica pero la orden falla (se
	// cortó la conexión); failRestore: el ALTER de deshacer falla.
	failAlter, failRestore bool
}

func newLoginKling(rk *roleKling) *loginKling {
	return &loginKling{roleKling: rk, login: map[string]bool{}, verifier: map[string]string{}}
}

func (k *loginKling) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "exec" || stdin == nil {
		return k.roleKling.Run(ctx, stdin, args...)
	}
	in, _ := io.ReadAll(stdin)
	sql := string(in)
	name := between(sql, "rolname = '", "'")
	switch {
	case strings.Contains(sql, "rolsuper OR rolreplication"):
		return []byte("f\n"), nil
	case strings.Contains(sql, "rolcanlogin"):
		out := map[bool]string{true: "t", false: "f"}[k.login[name]] + "|2026-11-23 10:00:00.5+00"
		if strings.Contains(sql, "coalesce(rolpassword") {
			out += "|" + k.verifier[name]
		}
		return []byte(out + "\n"), nil
	case strings.Contains(sql, "LOGIN PASSWORD '") && strings.Contains(sql, "rolpassword = '"):
		q := between(sql, "ALTER ROLE \"", "\"")
		k.login[q], k.verifier[q] = true, between(sql, "PASSWORD '", "'")
		if k.failAlter {
			return nil, io.ErrUnexpectedEOF
		}
		return []byte("t\n"), nil
	case strings.Contains(sql, "ALTER ROLE"):
		if k.failRestore {
			return nil, io.ErrClosedPipe
		}
		q := between(sql, "ALTER ROLE \"", "\"")
		switch {
		case strings.Contains(sql, "\" NOLOGIN"):
			k.login[q] = false
		case strings.Contains(sql, "\" LOGIN"):
			k.login[q] = true
		}
		if strings.Contains(sql, "PASSWORD NULL") {
			k.verifier[q] = ""
		} else if v := between(sql, "PASSWORD '", "'"); v != "" {
			k.verifier[q] = v
		}
		if strings.Contains(sql, "pg_terminate_backend") {
			k.terminated = append(k.terminated, sql)
		}
		return []byte("0\n"), nil
	}
	return k.roleKling.Run(ctx, strings.NewReader(sql), args...)
}

// loginApp: una copia lista con el rol "web" de las migraciones, sin LOGIN ni
// clave.
func loginApp(t *testing.T) (*testApp, *loginKling, string) {
	t.Helper()
	ta, rk, id := roleApp(t)
	rk.roles["web"] = &fakeRole{}
	lk := newLoginKling(rk)
	ta.app.k = lk
	return ta, lk, id
}

func noRolePassword(t *testing.T, id, role string) {
	t.Helper()
	if _, err := dbstate.ReadRolePassword(id, role); !errors.Is(err, dbstate.ErrNoPassword) {
		t.Errorf("the host still has a password for role %s: %v", role, err)
	}
}

// Si -login falla después del ALTER ROLE, deshacer no deja el rol con LOGIN y
// con el verificador de una clave que este host acaba de borrar: lo devuelve
// como estaba y corta las sesiones abiertas desde entonces.
func TestRoleLoginFallidoDevuelveElRol(t *testing.T) {
	ta, lk, id := loginApp(t)
	lk.badHBA = true
	if err := ta.roleLogin(context.Background(), "c1", defaultOwner, "web"); err == nil {
		t.Fatal("login with a bad pg_hba.conf worked")
	}
	if lk.login["web"] || lk.verifier["web"] != "" {
		t.Errorf("after the undo the role has login=%v verifier=%q", lk.login["web"], lk.verifier["web"])
	}
	if len(lk.terminated) == 0 {
		t.Error("the undo did not cut the sessions of the role")
	}
	if lk.hba["web"] {
		t.Error("the pg_hba.conf line stayed")
	}
	noRolePassword(t, id, "web")
}

// Lo mismo si el ALTER se aplicó pero la orden no contestó.
func TestRoleLoginAlterSinRespuestaDevuelveElRol(t *testing.T) {
	ta, lk, _ := loginApp(t)
	lk.failAlter = true
	if err := ta.roleLogin(context.Background(), "c1", defaultOwner, "web"); err == nil {
		t.Fatal("login worked")
	}
	if lk.login["web"] || lk.verifier["web"] != "" {
		t.Errorf("after the undo the role has login=%v verifier=%q", lk.login["web"], lk.verifier["web"])
	}
}

// Si deshacer falla, se dice: el rol puede haberse quedado con LOGIN.
func TestRoleLoginAvisaSiNoPuedeDeshacer(t *testing.T) {
	ta, lk, _ := loginApp(t)
	lk.badHBA, lk.failRestore = true, true
	err := ta.roleLogin(context.Background(), "c1", defaultOwner, "web")
	if err == nil || !strings.Contains(err.Error(), "could not be undone") {
		t.Fatalf("err = %v", err)
	}
}

// -login -rm revoca de verdad: además de la línea de pg_hba.conf y la clave
// del host, el rol vuelve a como estaba (sin LOGIN ni verificador) y se cortan
// las sesiones que abrió esa clave.
func TestRoleLoginRmRevoca(t *testing.T) {
	ta, lk, id := loginApp(t)
	ctx := context.Background()
	if err := ta.roleLogin(ctx, "c1", defaultOwner, "web"); err != nil {
		t.Fatal(err)
	}
	if !lk.login["web"] || lk.verifier["web"] == "" || !lk.hba["web"] {
		t.Fatalf("login did not take: login=%v verifier=%q hba=%v", lk.login["web"], lk.verifier["web"], lk.hba["web"])
	}
	if err := ta.roleLoginRemove(ctx, "c1", defaultOwner, "web"); err != nil {
		t.Fatal(err)
	}
	if lk.login["web"] || lk.verifier["web"] != "" {
		t.Errorf("after -rm the role has login=%v verifier=%q", lk.login["web"], lk.verifier["web"])
	}
	if len(lk.terminated) != 1 || !strings.Contains(lk.terminated[0], "backend_start >= ") {
		t.Errorf("sessions cut: %q", lk.terminated)
	}
	if lk.hba["web"] {
		t.Error("the pg_hba.conf line stayed")
	}
	noRolePassword(t, id, "web")
	if d, err := dbstate.CopyDir(id); err == nil {
		if ents, _ := os.ReadDir(d); len(ents) > 0 {
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), "web.") {
					t.Errorf("left behind for role web: %s", e.Name())
				}
			}
		}
	}
}
