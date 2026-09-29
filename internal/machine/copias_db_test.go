package machine

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

const (
	idAgente = "aaaaaaaaaaaaaaa1"
	idCopia  = "cccccccccccccc01"
	idOtra   = "cccccccccccccc02"
)

// escenaModeloA: un agente y una copia lista del mismo dueño, los dos en
// marcha y con red.
func escenaModeloA(t *testing.T) *Manager {
	t.Helper()
	m := newTestManager(t)
	m.bus = events.New()
	m.mu.Lock()
	m.byID[idAgente] = &api.Machine{ID: idAgente, Name: "agente", State: api.StateRunning, NetIndex: 3,
		Egress: string(knet.EgressAllowlist), Labels: map[string]string{api.LabelDBOwner: "local"}}
	m.byID[idCopia] = &api.Machine{ID: idCopia, Name: "copia", State: api.StateRunning, NetIndex: 7,
		Labels: map[string]string{api.LabelDBGolden: "pg", api.LabelDBOwner: "local",
			api.LabelDBState: api.DBStateReady, api.LabelPorts: "5432"}}
	m.mu.Unlock()
	return m
}

// La puerta del modelo A, caso a caso. La dirección solo sale con todo en
// orden, y se pregunta cada vez: lo que cambia entre dos llamadas cuenta.
func TestResolverCopia(t *testing.T) {
	casos := []struct {
		nombre string
		mod    func(m *Manager)
		owner  string
		port   int
		error  string
	}{
		{nombre: "todo en orden"},
		{nombre: "copia parada", mod: func(m *Manager) { m.byID[idCopia].State = api.StateStopped }, error: "not running"},
		{nombre: "copia congelada", mod: func(m *Manager) { m.byID[idCopia].State = api.StateWarm }, error: "not running"},
		{nombre: "copia pausada", mod: func(m *Manager) { m.byID[idCopia].State = api.StatePaused }, error: "not running"},
		{nombre: "copia en preparing", mod: func(m *Manager) { m.byID[idCopia].Labels[api.LabelDBState] = "preparing" }, error: "not ready"},
		{nombre: "no es una copia", mod: func(m *Manager) { delete(m.byID[idCopia].Labels, api.LabelDBGolden) }, error: "not a kling db copy"},
		{nombre: "copia de otro dueño", mod: func(m *Manager) { m.byID[idCopia].Labels[api.LabelDBOwner] = "otro" }, error: "owner mismatch"},
		{nombre: "agente de otro dueño", mod: func(m *Manager) { m.byID[idAgente].Labels[api.LabelDBOwner] = "otro" }, error: "owner mismatch"},
		{nombre: "agente sin dueño", mod: func(m *Manager) { delete(m.byID[idAgente].Labels, api.LabelDBOwner) }, error: "owner mismatch"},
		{nombre: "la credencial dice otro dueño", owner: "otro", error: "owner mismatch"},
		// Mismo kling.db.owner, distinto inquilino (kling.owner, authz).
		{nombre: "copia de otro inquilino", mod: func(m *Manager) { m.byID[idCopia].Labels[api.LabelOwner] = "b" }, error: "tenant mismatch"},
		{nombre: "mismo inquilino", mod: func(m *Manager) {
			m.byID[idCopia].Labels[api.LabelOwner] = "a"
			m.byID[idAgente].Labels[api.LabelOwner] = "a"
		}},
		{nombre: "puerto no expuesto", port: 22, error: "does not expose port 22"},
		{nombre: "agente borrado", mod: func(m *Manager) { delete(m.byID, idAgente) }, error: "no longer exists"},
		{
			// La copia se borró y otra máquina (otro ID) ocupa su índice de
			// red y hasta su nombre, lista y del mismo dueño: la credencial
			// va por ID y no la alcanza.
			nombre: "otro ID con el mismo índice de red",
			mod: func(m *Manager) {
				viejo := m.byID[idCopia]
				delete(m.byID, idCopia)
				nueva := *viejo
				nueva.ID = idOtra
				nueva.Labels = map[string]string{api.LabelDBGolden: "pg", api.LabelDBOwner: "local",
					api.LabelDBState: api.DBStateReady, api.LabelPorts: "5432"}
				m.byID[idOtra] = &nueva
			},
			error: "doesn't exist",
		},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			m := escenaModeloA(t)
			if c.mod != nil {
				m.mu.Lock()
				c.mod(m)
				m.mu.Unlock()
			}
			owner, port := "local", 5432
			if c.owner != "" {
				owner = c.owner
			}
			if c.port != 0 {
				port = c.port
			}
			addr, err := m.resolverCopia(idAgente)(idCopia, owner, port)
			if c.error != "" {
				if err == nil || !strings.Contains(err.Error(), c.error) {
					t.Fatalf("esperaba %q, llegó addr=%q err=%v", c.error, addr, err)
				}
				return
			}
			if !modeloAPosible {
				if !errors.Is(err, errModeloASoloLinux) {
					t.Fatalf("en esta plataforma no hay modelo A: %q %v", addr, err)
				}
				return
			}
			want := net.JoinHostPort(knet.Plan(7, idCopia).NSIP, "5432")
			if err != nil || addr != want {
				t.Fatalf("addr=%q err=%v, esperaba %s", addr, err, want)
			}
		})
	}
}

// Ni un nombre, ni una IP, ni la propia máquina.
func TestResolverCopiaIDs(t *testing.T) {
	m := escenaModeloA(t)
	r := m.resolverCopia(idAgente)
	for _, id := range []string{"copia", "172.30.0.30", "172.30.0.30:5432", idCopia[:12], idAgente} {
		if addr, err := r(id, "local", 5432); err == nil {
			t.Errorf("%q resolvió a %q", id, addr)
		}
	}
}

// El attach comprueba lo mismo al entregar; con la copia parada no se entrega
// nada (ni almacén ni proxy).
func TestSetCredentialsUpstreamMachine(t *testing.T) {
	m := escenaModeloA(t)
	got := capturarRegistro(t)
	falso := nuevoFcFalso(t)
	m.mu.Lock()
	m.socket[idAgente] = falso.Sock
	m.mu.Unlock()
	spec := func() api.CredentialSpec {
		return api.CredentialSpec{Type: credproxy.KindPostgres, Domain: "copia.db.internal", Env: "PGPASSWORD",
			Secret: "clave-de-la-copia", User: "app", Database: "appdb",
			UpstreamMachine: idCopia, UpstreamOwner: "local", UpstreamTLS: credproxy.UpstreamTLSDisable}
	}
	ctx := context.Background()

	m.mu.Lock()
	m.byID[idCopia].State = api.StateWarm
	m.mu.Unlock()
	if _, err := m.SetCredentials(ctx, idAgente, []api.CredentialSpec{spec()}); err == nil {
		t.Fatal("attach a una copia congelada aceptado")
	}
	if len(*got) != 0 {
		t.Fatal("se entregó algo al proxy")
	}
	m.mu.Lock()
	m.byID[idCopia].State = api.StateRunning
	m.mu.Unlock()

	// Una IP en upstream_machine, o upstream y upstream_machine a la vez: no.
	for _, mod := range []func(*api.CredentialSpec){
		func(s *api.CredentialSpec) { s.UpstreamMachine = "172.30.0.30" },
		func(s *api.CredentialSpec) { s.Upstream = "127.0.0.1:55432" },
		func(s *api.CredentialSpec) { s.UpstreamTLS = "" },
	} {
		s := spec()
		mod(&s)
		if _, err := m.SetCredentials(ctx, idAgente, []api.CredentialSpec{s}); err == nil {
			t.Errorf("aceptada: %+v", s)
		}
	}

	_, err := m.SetCredentials(ctx, idAgente, []api.CredentialSpec{spec()})
	if !modeloAPosible {
		if !errors.Is(err, errModeloASoloLinux) {
			t.Fatalf("en esta plataforma el attach debe fallar claro: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0][0].UpstreamMachine != idCopia || (*got)[0][0].UpstreamOwner != "local" {
		t.Fatalf("entregado: %+v", *got)
	}
	back, err := m.cargarCredenciales(idAgente)
	if err != nil || len(back) != 1 || back[0].UpstreamMachine != idCopia {
		t.Fatalf("almacén: %+v %v", back, err)
	}
}

// Las plantillas no llevan upstream_machine.
func TestSnapshotCredentialsSinUpstreamMachine(t *testing.T) {
	err := sinUpstreamMaquina([]api.CredentialSpec{{Env: "PGPASSWORD", UpstreamMachine: idCopia}})
	if err == nil || !strings.Contains(err.Error(), "not for templates") {
		t.Fatalf("%v", err)
	}
}

// invalidaciones apunta lo que el manager manda cortar.
type invalidaciones struct {
	mu      sync.Mutex
	copias  []string
	agentes []string
}

func espiarInvalidaciones(t *testing.T) *invalidaciones {
	t.Helper()
	inv := &invalidaciones{}
	pc, pa := invalidarCopia, invalidarAgente
	invalidarCopia = func(id string) int {
		inv.mu.Lock()
		defer inv.mu.Unlock()
		inv.copias = append(inv.copias, id)
		return 1
	}
	invalidarAgente = func(n *knet.Net) int {
		inv.mu.Lock()
		defer inv.mu.Unlock()
		inv.agentes = append(inv.agentes, n.NS)
		return 0
	}
	t.Cleanup(func() { invalidarCopia, invalidarAgente = pc, pa })
	return inv
}

// Parar, borrar o reetiquetar (en lo que importa) la copia corta sus
// sesiones; una etiqueta cualquiera no.
func TestInvalidacionAlCambiarLaCopia(t *testing.T) {
	m := escenaModeloA(t)
	inv := espiarInvalidaciones(t)

	if err := m.SetLabels(idCopia, map[string]string{"otra": "x"}); err != nil {
		t.Fatal(err)
	}
	if len(inv.copias) != 0 {
		t.Fatalf("una etiqueta ajena invalidó: %v", inv.copias)
	}
	if err := m.SetLabels(idCopia, map[string]string{api.LabelDBState: api.DBStateReady}); err != nil {
		t.Fatal(err)
	}
	if len(inv.copias) != 0 {
		t.Fatalf("reponer el mismo valor invalidó: %v", inv.copias)
	}
	if err := m.SetLabels(idCopia, map[string]string{api.LabelDBState: "preparing"}); err != nil {
		t.Fatal(err)
	}
	if len(inv.copias) != 1 || inv.copias[0] != idCopia || len(inv.agentes) != 1 {
		t.Fatalf("preparing no invalidó: %+v", inv)
	}
	if err := m.SetLabels(idAgente, map[string]string{api.LabelDBOwner: "otro"}); err != nil {
		t.Fatal(err)
	}
	if len(inv.agentes) != 2 || inv.agentes[1] != knet.Plan(3, idAgente).NS {
		t.Fatalf("el cambio de dueño del agente no cortó sus sesiones: %+v", inv)
	}

	if _, err := m.Stop(idCopia); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(idCopia); err != nil {
		t.Fatal(err)
	}
	if n := len(inv.copias); n != 4 || inv.copias[2] != idCopia || inv.copias[3] != idCopia {
		t.Fatalf("Stop y Remove: %v", inv.copias)
	}
}

// detach: quita la credencial del almacén y del proxy, y solo si va a la
// máquina pedida.
func TestRemoveCredential(t *testing.T) {
	m := escenaModeloA(t)
	got := capturarRegistro(t)
	falso := nuevoFcFalso(t)
	m.mu.Lock()
	m.socket[idAgente] = falso.Sock
	m.mu.Unlock()
	creds := []credproxy.Credential{
		{Env: "PGPASSWORD", Domain: "copia.db.internal", Placeholder: "kling-cred-a", Secret: "s1", Kind: credproxy.KindPostgres,
			Port: 5432, User: "app", Database: "appdb", UpstreamMachine: idCopia, UpstreamOwner: "local", UpstreamTLS: credproxy.UpstreamTLSDisable},
		{Env: "API_KEY", Domain: "api.example.com", Placeholder: "kling-cred-b", Secret: "s2"},
	}
	if err := m.guardarCredenciales(idAgente, creds); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := m.RemoveCredential(ctx, idAgente, "PGPASSWORD", idOtra); err == nil || !strings.Contains(err.Error(), "does not go to") {
		t.Fatalf("otra máquina: %v", err)
	}
	if _, err := m.RemoveCredential(ctx, idAgente, "NADA", ""); err == nil || !strings.Contains(err.Error(), "has no credential") {
		t.Fatalf("variable inexistente: %v", err)
	}
	if _, err := m.RemoveCredential(ctx, idAgente, "bad env", ""); err == nil {
		t.Fatal("variable inválida aceptada")
	}
	out, err := m.RemoveCredential(ctx, idAgente, "PGPASSWORD", idCopia)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.CredentialDomains) != 1 || out.CredentialDomains[0] != "api.example.com" {
		t.Fatalf("dominios: %v", out.CredentialDomains)
	}
	back, _ := m.cargarCredenciales(idAgente)
	if len(back) != 1 || back[0].Env != "API_KEY" {
		t.Fatalf("almacén: %+v", back)
	}
	if len(*got) != 1 || len((*got)[0]) != 1 || (*got)[0][0].Env != "API_KEY" {
		t.Fatalf("al proxy: %+v", *got)
	}
}
