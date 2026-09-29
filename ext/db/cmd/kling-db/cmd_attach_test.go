package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/db/internal/dbstate"
	"github.com/juan52878911/kindling/pkg/api"
)

// escenaAttach: una copia lista (c1) y un agente en marcha con egress
// allowlist y sin dueño.
func escenaAttach(t *testing.T) (*testApp, *api.Machine, *api.Machine) {
	t.Helper()
	ta := newTestApp(t)
	if _, err := ta.up(context.Background(), "pg", "c1", 0, "local"); err != nil {
		t.Fatal(err)
	}
	ta.f.mu.Lock()
	cp := ta.f.find("c1") // la del falso: lo que los casos cambien, lo ve inspect
	ag := ta.f.newMachine("agente", map[string]string{})
	ag.Egress = "allowlist"
	ta.f.mu.Unlock()
	ta.out.Reset()
	ta.err.Reset()
	return ta, cp, ag
}

// attach entrega al agente una credencial hacia el ID de la copia, con la
// clave de la copia, sin TLS (SCRAM), con el dueño, y etiqueta al agente. La
// clave no sale por ninguna parte.
func TestAttach(t *testing.T) {
	ta, cp, ag := escenaAttach(t)
	ctx := context.Background()
	if err := ta.attach(ctx, "agente", "c1", attachOpts{owner: "local", env: "PGPASSWORD"}); err != nil {
		t.Fatal(err)
	}
	specs := ta.f.creds[ag.ID]
	if len(specs) != 1 {
		t.Fatalf("credenciales: %+v", ta.f.creds)
	}
	s := specs[0]
	pw, _ := dbstate.ReadPassword(cp.ID)
	if s.Type != "postgres" || s.UpstreamMachine != cp.ID || s.UpstreamOwner != "local" || s.UpstreamTLS != "disable" ||
		s.Upstream != "" || s.User != "app" || s.Database != "appdb" || s.Secret != pw || s.Env != "PGPASSWORD" ||
		s.Domain != "c1.db.internal" || s.Port != 5432 {
		t.Fatalf("spec: %+v", s)
	}
	if ag.Labels[labelOwner] != "local" {
		t.Fatalf("el agente no quedó etiquetado: %v", ag.Labels)
	}
	all := ta.out.String() + ta.err.String()
	if strings.Contains(all, pw) {
		t.Fatal("la clave salió por pantalla")
	}
	if !strings.Contains(all, "host=c1.db.internal") || !strings.Contains(all, "warning: the agent gets the application role") {
		t.Fatalf("salida: %s", all)
	}
	// Por argv no viaja nada de esto.
	for _, c := range ta.f.calls {
		if strings.Contains(strings.Join(c.args, " "), pw) {
			t.Fatalf("la clave en argv: %v", c.args)
		}
	}
}

// Con -role, la clave del rol (no la de la aplicación), y sin el aviso.
func TestAttachConRol(t *testing.T) {
	ta, cp, ag := escenaAttach(t)
	ctx := context.Background()
	if err := ta.attach(ctx, "agente", "c1", attachOpts{owner: "local", env: "PGPASSWORD", role: "agent"}); err == nil ||
		!strings.Contains(err.Error(), "no password for role agent") {
		t.Fatalf("sin clave del rol: %v", err)
	}
	if err := dbstate.WriteRolePassword(cp.ID, "agent", "clave-del-rol"); err != nil {
		t.Fatal(err)
	}
	if err := ta.attach(ctx, "agente", "c1", attachOpts{owner: "local", env: "RO_PASSWORD", role: "agent", host: "Crm.DB.internal."}); err != nil {
		t.Fatal(err)
	}
	s := ta.f.creds[ag.ID][0]
	if s.User != "agent" || s.Secret != "clave-del-rol" || s.Env != "RO_PASSWORD" || s.Domain != "crm.db.internal" {
		t.Fatalf("spec: %+v", s)
	}
	if strings.Contains(ta.err.String(), "warning") {
		t.Fatalf("aviso con -role: %s", ta.err.String())
	}
	if err := ta.attach(ctx, "agente", "c1", attachOpts{owner: "local", env: "X", role: "app"}); err == nil {
		t.Fatal("-role con el rol de la aplicación aceptado")
	}
}

// Lo que no se entrega: otro dueño, copia no lista o parada, la misma
// máquina, un agente sin allowlist o parado, nombres raros.
func TestAttachRechazos(t *testing.T) {
	casos := map[string]struct {
		mod  func(ta *testApp, cp, ag *api.Machine)
		opts attachOpts
		want string
	}{
		"agente de otro dueño": {mod: func(_ *testApp, _, ag *api.Machine) { ag.Labels[labelOwner] = "otro" }, want: `belongs to owner "otro"`},
		"copia de otro dueño":  {opts: attachOpts{owner: "otro"}, want: "belongs to owner"},
		"copia en preparing":   {mod: func(_ *testApp, cp, _ *api.Machine) { cp.Labels[labelState] = statePreparing }, want: "not ready"},
		"copia congelada":      {mod: func(_ *testApp, cp, _ *api.Machine) { cp.State = api.StateWarm }, want: "not running"},
		"agente sin allowlist": {mod: func(_ *testApp, _, ag *api.Machine) { ag.Egress = "internet" }, want: "egress allowlist"},
		"agente parado":        {mod: func(_ *testApp, _, ag *api.Machine) { ag.State = api.StateStopped }, want: "not running"},
		"env mala":             {opts: attachOpts{env: "pg password"}, want: "invalid -env"},
		"host malo":            {opts: attachOpts{host: "10.0.0.1"}, want: "invalid -host"},
		"base mala":            {opts: attachOpts{database: "app db"}, want: "invalid -database"},
		"daemon sin db-attach": {mod: func(ta *testApp, _, _ *api.Machine) {
			ta.f.credErr = errors.New("-upstream-tls disable needs -upstream")
		}, want: "update kindling"},
	}
	for nombre, c := range casos {
		t.Run(nombre, func(t *testing.T) {
			ta, cp, ag := escenaAttach(t)
			if c.mod != nil {
				c.mod(ta, cp, ag)
			}
			o := c.opts
			if o.owner == "" {
				o.owner = "local"
			}
			if o.env == "" {
				o.env = "PGPASSWORD"
			}
			err := ta.attach(context.Background(), "agente", "c1", o)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("esperaba %q: %v", c.want, err)
			}
			if len(ta.f.creds[ag.ID]) != 0 {
				t.Fatal("se entregó una credencial")
			}
		})
	}
	ta, _, _ := escenaAttach(t)
	if err := ta.attach(context.Background(), "c1", "c1", attachOpts{owner: "local", env: "PGPASSWORD"}); err == nil ||
		!strings.Contains(err.Error(), "same machine") {
		t.Fatalf("la misma máquina: %v", err)
	}
}

func TestDefaultAttachHost(t *testing.T) {
	for name, want := range map[string]string{
		"c1":          "c1.db.internal",
		"CRM_Demo.v2": "crm-demo-v2.db.internal",
		"---":         "db-00000000000a.db.internal",
		"pg-2f3a9c":   "pg-2f3a9c.db.internal",
	} {
		if got := defaultAttachHost(&api.Machine{ID: "00000000000a0000", Name: name}); got != want {
			t.Errorf("%q: %q, esperaba %q", name, got, want)
		}
	}
}

// detach retira la credencial del agente atada a ESA copia (por su id), y
// admite el id de una copia que ya no existe.
func TestDetach(t *testing.T) {
	ta, cp, ag := escenaAttach(t)
	ctx := context.Background()
	// Un agente sin dueño (o de otro) no se toca: attach lo habría etiquetado.
	if err := ta.detach(ctx, "agente", "c1", "local", "PGPASSWORD"); err == nil || !strings.Contains(err.Error(), "same owner") {
		t.Fatalf("detach de un agente sin dueño: %v", err)
	}
	if ag.Labels == nil {
		ag.Labels = map[string]string{}
	}
	ag.Labels[labelOwner] = "local"
	if err := ta.detach(ctx, "agente", "c1", "local", "PGPASSWORD"); err != nil {
		t.Fatal(err)
	}
	if len(ta.f.removed) != 1 || ta.f.removed[0] != ag.ID+" PGPASSWORD "+cp.ID {
		t.Fatalf("removed: %v", ta.f.removed)
	}
	if err := ta.detach(ctx, "agente", "c1", "otro", "PGPASSWORD"); err == nil {
		t.Fatal("detach de una copia de otro dueño aceptado")
	}
	if err := ta.detach(ctx, "agente", "no-existe", "local", "PGPASSWORD"); err == nil ||
		!strings.Contains(err.Error(), "kling machine credential -rm") {
		t.Fatalf("copia inexistente por nombre: %v", err)
	}
	if err := ta.detach(ctx, "agente", "00000000deadbeef", "local", "PGPASSWORD"); err != nil {
		t.Fatalf("copia inexistente por id: %v", err)
	}
	if got := ta.f.removed[len(ta.f.removed)-1]; got != ag.ID+" PGPASSWORD 00000000deadbeef" {
		t.Fatalf("removed: %v", got)
	}
}
