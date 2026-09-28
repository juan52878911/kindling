package machine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

const (
	credSecreto  = "sk_live_LA_CLAVE_QUE_NO_DEBE_VERSE"
	credSecreto2 = "sk_live_LA_CLAVE_ROTADA"
)

// capturarRegistro sustituye knet.SetCredentials por un registro en memoria:
// en el Mac no hay veth donde escuchar, y lo que se prueba aquí es lo que el
// manager le entrega, no el proxy (que tiene sus tests en pkg/credproxy).
func capturarRegistro(t *testing.T) *[][]credproxy.Credential {
	t.Helper()
	var got [][]credproxy.Credential
	prevSin := sinProxyDeCredenciales
	sinProxyDeCredenciales = false
	t.Cleanup(func() { sinProxyDeCredenciales = prevSin })
	prev := registrarCredenciales
	registrarCredenciales = func(_ *knet.Net, creds []credproxy.Credential) error {
		got = append(got, append([]credproxy.Credential(nil), creds...))
		return nil
	}
	t.Cleanup(func() { registrarCredenciales = prev })
	return &got
}

// El almacén: ida y vuelta, solo root, atado a su máquina, y vacío = sin fichero.
func TestAlmacenDeCredenciales(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(m.dir("m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	creds := []credproxy.Credential{{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-aa", Secret: credSecreto}}
	if err := m.guardarCredenciales("m1", creds); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(m.credPath("m1"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("fichero: %v, permisos %v", err, fi.Mode().Perm())
	}
	raw, _ := os.ReadFile(m.credPath("m1"))
	if bytes.Contains(raw, []byte(credSecreto)) || bytes.Contains(raw, []byte("api.example.com")) {
		t.Fatal("el almacén está en claro")
	}
	back, err := m.cargarCredenciales("m1")
	if err != nil || len(back) != 1 || !reflect.DeepEqual(back[0], creds[0]) {
		t.Fatalf("vuelta: %+v, %v", back, err)
	}

	// Copiado al directorio de otra máquina no descifra: el id es dato autenticado.
	if err := os.MkdirAll(m.dir("m2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.credPath("m2"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.cargarCredenciales("m2"); err == nil || !strings.Contains(err.Error(), "can't decrypt") {
		t.Fatalf("el almacén de m1 valió para m2: %v", err)
	}

	// Sin credenciales, sin fichero; y sin fichero, nil sin error.
	if err := m.guardarCredenciales("m1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.credPath("m1")); !os.IsNotExist(err) {
		t.Fatalf("el fichero vacío debería borrarse: %v", err)
	}
	if back, err := m.cargarCredenciales("m1"); err != nil || back != nil {
		t.Fatalf("sin fichero: %v, %v", back, err)
	}
}

// SetCredentials guarda cifrado, pone los marcadores en MMDS (y solo los
// marcadores), entrega el juego completo al proxy, fusiona por variable
// conservando el marcador al rotar, y NO marca HasSecrets.
func TestSetCredentialsGuardaFusionaYRota(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	got := capturarRegistro(t)
	falso := nuevoFcFalso(t)
	mc := m.addForTest("m1")
	m.mu.Lock()
	mc.Egress = string(knet.EgressAllowlist)
	m.socket["m1"] = falso.Sock
	m.mu.Unlock()
	if err := os.MkdirAll(m.dir("m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	out, err := m.SetCredentials(ctx, "m1", []api.CredentialSpec{
		{Domain: "API.Example.com", Env: "KEY", Secret: credSecreto},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.HasSecrets {
		t.Error("una credencial no debe marcar HasSecrets: el marcador no es un secreto")
	}
	if len(out.CredentialDomains) != 1 || out.CredentialDomains[0] != "api.example.com" {
		t.Errorf("dominios %v", out.CredentialDomains)
	}
	if len(*got) != 1 || len((*got)[0]) != 1 || (*got)[0][0].Secret != credSecreto {
		t.Fatalf("al proxy llegó %+v", *got)
	}
	ph := (*got)[0][0].Placeholder
	if !strings.HasPrefix(ph, credproxy.PlaceholderPrefix) {
		t.Fatalf("marcador %q", ph)
	}
	patches := falso.llamadasA("PATCH", "/mmds")
	if len(patches) != 1 {
		t.Fatalf("PATCH /mmds: %d llamadas", len(patches))
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(patches[0].Cuerpo, &doc); err != nil || doc.Env["KEY"] != ph {
		t.Fatalf("MMDS recibió %s (%v), quería env.KEY=%s", patches[0].Cuerpo, err, ph)
	}
	if bytes.Contains(patches[0].Cuerpo, []byte(credSecreto)) {
		t.Fatal("la clave real viajó a MMDS")
	}

	// Segunda entrega: otra variable nueva y la primera ROTADA. El marcador de
	// KEY se conserva (el proceso del invitado ya lo tiene en su entorno).
	out, err = m.SetCredentials(ctx, "m1", []api.CredentialSpec{
		{Domain: "api.example.com", Env: "KEY", Secret: credSecreto2},
		{Domain: "b.example.org", Env: "ORG", Secret: "org-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 || len((*got)[1]) != 2 {
		t.Fatalf("segunda entrega al proxy: %+v", *got)
	}
	porEnv := map[string]credproxy.Credential{}
	for _, c := range (*got)[1] {
		porEnv[c.Env] = c
	}
	if porEnv["KEY"].Placeholder != ph || porEnv["KEY"].Secret != credSecreto2 {
		t.Errorf("rotación: %+v (marcador original %s)", porEnv["KEY"], ph)
	}
	if porEnv["ORG"].Placeholder == ph || porEnv["ORG"].Domain != "b.example.org" {
		t.Errorf("nueva: %+v", porEnv["ORG"])
	}
	if strings.Join(out.CredentialDomains, ",") != "api.example.com,b.example.org" {
		t.Errorf("dominios %v", out.CredentialDomains)
	}
	// El almacén tiene el juego completo y actual.
	back, err := m.cargarCredenciales("m1")
	if err != nil || len(back) != 2 {
		t.Fatalf("almacén: %+v, %v", back, err)
	}

	// Ni el estado en disco ni el evento llevan la clave.
	m.persistirYa()
	st, _ := os.ReadFile(m.statePath())
	if bytes.Contains(st, []byte(credSecreto)) || bytes.Contains(st, []byte(credSecreto2)) || bytes.Contains(st, []byte(ph)) {
		t.Fatal("state.json lleva la clave o el marcador")
	}
	if !bytes.Contains(st, []byte("api.example.com")) {
		t.Error("state.json debería llevar los dominios")
	}
}

// Lo que se rechaza, y con un motivo que señala al arreglo.
func TestSetCredentialsRechazaLoQueNoVale(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	capturarRegistro(t)
	falso := nuevoFcFalso(t)
	mc := m.addForTest("m1")
	m.mu.Lock()
	m.socket["m1"] = falso.Sock
	m.mu.Unlock()
	ctx := context.Background()
	spec := []api.CredentialSpec{{Domain: "api.example.com", Env: "KEY", Secret: "x"}}

	if _, err := m.SetCredentials(ctx, "m1", spec); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("sin allowlist: %v", err)
	}
	m.mu.Lock()
	mc.Egress = string(knet.EgressAllowlist)
	m.mu.Unlock()
	for nombre, s := range map[string][]api.CredentialSpec{
		"env en minúsculas": {{Domain: "api.example.com", Env: "key", Secret: "x"}},
		"env repetida":      {{Domain: "a.example.com", Env: "KEY", Secret: "x"}, {Domain: "b.example.com", Env: "KEY", Secret: "y"}},
		"comodín":           {{Domain: "*.example.com", Env: "KEY", Secret: "x"}},
		"sin clave":         {{Domain: "api.example.com", Env: "KEY"}},
		"nada":              {},
	} {
		if _, err := m.SetCredentials(ctx, "m1", s); err == nil {
			t.Errorf("%s: debería rechazarse", nombre)
		}
	}
	if _, err := os.Stat(m.credPath("m1")); !os.IsNotExist(err) {
		t.Error("un rechazo no debe dejar almacén")
	}
}

// Reentregar: sin cliente (reconcile) solo rehace el lado del host; con él
// (thaw) repone además los marcadores en MMDS. Sin almacén no hace nada.
func TestReentregarCredenciales(t *testing.T) {
	m := newTestManager(t)
	got := capturarRegistro(t)
	falso := nuevoFcFalso(t)
	mc := &api.Machine{ID: "m1", Name: "m1", NetIndex: 3}
	if err := os.MkdirAll(m.dir("m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if n, err := m.reentregarCredenciales(ctx, mc, nil); n != 0 || err != nil {
		t.Fatalf("sin almacén: %d, %v", n, err)
	}
	creds := []credproxy.Credential{
		{Env: "KEY", Domain: "api.example.com", Placeholder: "kling-cred-aa", Secret: credSecreto},
		{Env: "ORG", Domain: "api.example.com", Placeholder: "kling-cred-bb", Secret: "org"},
	}
	if err := m.guardarCredenciales("m1", creds); err != nil {
		t.Fatal(err)
	}
	if n, err := m.reentregarCredenciales(ctx, mc, nil); n != 2 || err != nil {
		t.Fatalf("reconcile: %d, %v", n, err)
	}
	if len(*got) != 1 || len((*got)[0]) != 2 || len(falso.llamadasA("PATCH", "/mmds")) != 0 {
		t.Fatalf("sin cliente no debe tocar MMDS: %d registros, %d PATCH", len(*got), len(falso.llamadasA("PATCH", "/mmds")))
	}
	if n, err := m.reentregarCredenciales(ctx, mc, falso.cliente()); n != 2 || err != nil {
		t.Fatalf("thaw: %d, %v", n, err)
	}
	patches := falso.llamadasA("PATCH", "/mmds")
	if len(patches) != 1 || !bytes.Contains(patches[0].Cuerpo, []byte(`"KEY":"kling-cred-aa"`)) ||
		!bytes.Contains(patches[0].Cuerpo, []byte(`"ORG":"kling-cred-bb"`)) {
		t.Fatalf("MMDS tras el thaw: %+v", patches)
	}

	// Firecracker rechaza el PATCH si el almacén MMDS no existe aún (tras un
	// restore sin MMDS previo): se cae a PUT.
	falso.fallar("PATCH", "/mmds", 400, "The MMDS data store is not initialized.")
	if n, err := m.reentregarCredenciales(ctx, mc, falso.cliente()); n != 2 || err != nil {
		t.Fatalf("con PATCH rechazado: %d, %v", n, err)
	}
	if len(falso.llamadasA("PUT", "/mmds")) != 1 {
		t.Error("debería haber caído a PUT /mmds")
	}
}

// Credenciales de plantilla: exigen allowlist, se fusionan por variable, se
// listan como dominios en el snapshot, y -clear las quita. Un `kling mcp import`
// rehace el snapshot: el almacén vive aparte y sobrevive.
func TestCredencialesDePlantilla(t *testing.T) {
	m := newTestManager(t)
	prevSin := sinProxyDeCredenciales
	sinProxyDeCredenciales = false
	t.Cleanup(func() { sinProxyDeCredenciales = prevSin })
	escribirSnapshot(t, m, "svc", api.Snapshot{Egress: "none"})
	escribirSnapshot(t, m, "svc-al", api.Snapshot{Egress: "allowlist", AllowDomains: []string{"example.org"}})
	spec := []api.CredentialSpec{{Domain: "API.Example.com", Env: "KEY", Secret: credSecreto}}

	if _, err := m.SetSnapshotCredentials("no-existe", spec, false); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("plantilla inexistente: %v", err)
	}
	if _, err := m.SetSnapshotCredentials("svc", spec, false); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("sin allowlist debería negarse y decirlo: %v", err)
	}
	snap, err := m.SetSnapshotCredentials("svc-al", spec, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(snap.CredentialDomains, ",") != "api.example.com" {
		t.Errorf("dominios %v", snap.CredentialDomains)
	}
	raw, _ := os.ReadFile(m.credSnapPath("svc-al"))
	if bytes.Contains(raw, []byte(credSecreto)) {
		t.Fatal("el almacén de plantilla está en claro")
	}
	fi, _ := os.Stat(m.credSnapPath("svc-al"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("permisos %v", fi.Mode().Perm())
	}
	// Fusión: rotar KEY y añadir ORG.
	snap, err = m.SetSnapshotCredentials("svc-al", []api.CredentialSpec{
		{Domain: "api.example.com", Env: "KEY", Secret: credSecreto2},
		{Domain: "b.example.org", Env: "ORG", Secret: "org"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(snap.CredentialDomains, ",") != "api.example.com,b.example.org" {
		t.Errorf("dominios tras fusionar %v", snap.CredentialDomains)
	}
	specs, err := m.cargarCredencialesPlantilla("svc-al")
	if err != nil || len(specs) != 2 || specs[0].Env != "KEY" || specs[0].Secret != credSecreto2 {
		t.Fatalf("almacén: %+v, %v", specs, err)
	}
	// Se ve también en el listado, y el meta.json no lo lleva.
	for _, s := range m.Snapshots() {
		if s.Name == "svc-al" && len(s.CredentialDomains) != 2 {
			t.Errorf("Snapshots() no anota los dominios: %v", s.CredentialDomains)
		}
	}
	meta, _ := os.ReadFile(m.snapDir("svc-al") + "/meta.json")
	if bytes.Contains(meta, []byte("credential")) {
		t.Error("los dominios no deben persistirse en meta.json")
	}
	// Rehacer el snapshot (lo que hace `kling mcp import`) no se lleva las claves.
	escribirSnapshot(t, m, "svc-al", api.Snapshot{Egress: "allowlist"})
	if s, err := m.Snapshot("svc-al"); err != nil || len(s.CredentialDomains) != 2 {
		t.Errorf("tras rehacer el snapshot: %v, %v", s, err)
	}
	// -clear.
	snap, err = m.SetSnapshotCredentials("svc-al", nil, true)
	if err != nil || len(snap.CredentialDomains) != 0 {
		t.Fatalf("clear: %v, %v", snap, err)
	}
	if _, err := os.Stat(m.credSnapPath("svc-al")); !os.IsNotExist(err) {
		t.Error("clear debería borrar el almacén")
	}
}

// A2: el error de runFrom cuando el egress pedido no alcanza para las
// credenciales de la plantilla debe listar sus dominios concretos, no un
// "<its domains>" que obligue a ir a buscarlos a otro sitio.
func TestRunFromCredencialesErrorListaDominios(t *testing.T) {
	m := newTestManager(t)
	prevSin := sinProxyDeCredenciales
	sinProxyDeCredenciales = false
	t.Cleanup(func() { sinProxyDeCredenciales = prevSin })
	escribirSnapshot(t, m, "svc-al", api.Snapshot{Egress: "allowlist", AllowDomains: []string{"example.org"}})
	spec := []api.CredentialSpec{{Domain: "api.example.com", Env: "KEY", Secret: credSecreto}}
	if _, err := m.SetSnapshotCredentials("svc-al", spec, false); err != nil {
		t.Fatal(err)
	}

	// Egress explícito "none": no debe heredar del snapshot, así que la
	// comprobación de credenciales tiene que negarse y decir qué dominios hacen
	// falta.
	_, err := m.runFrom(context.Background(), api.RunRequest{From: "svc-al", Egress: "none"})
	if err == nil {
		t.Fatal("se esperaba un error: egress none con credenciales de plantilla")
	}
	if !strings.Contains(err.Error(), "-allow example.org") {
		t.Errorf("el error no lista los dominios concretos: %v", err)
	}
	if strings.Contains(err.Error(), "its domains") {
		t.Errorf("el error sigue diciendo <its domains> en vez de listarlos: %v", err)
	}
}

// entregarCredenciales es lo que runFrom usa con las de plantilla: marcadores
// nuevos por instancia, almacén propio de la máquina y MMDS con solo marcadores.
func TestEntregarCredencialesDePlantillaAUnaInstancia(t *testing.T) {
	m := newTestManager(t)
	got := capturarRegistro(t)
	falso := nuevoFcFalso(t)
	if err := os.MkdirAll(m.dir("i1"), 0o755); err != nil {
		t.Fatal(err)
	}
	specs := []api.CredentialSpec{{Domain: "api.example.com", Env: "KEY", Secret: credSecreto}}
	creds, nuevas, err := m.entregarCredenciales(context.Background(), "i1", knet.Plan(1, "i1"), falso.cliente(), specs)
	if err != nil || nuevas != 1 || len(creds) != 1 {
		t.Fatalf("%+v %d %v", creds, nuevas, err)
	}
	if len(*got) != 1 || (*got)[0][0].Placeholder != creds[0].Placeholder {
		t.Fatalf("al proxy: %+v", *got)
	}
	back, _ := m.cargarCredenciales("i1")
	if len(back) != 1 || !reflect.DeepEqual(back[0], creds[0]) {
		t.Fatalf("almacén de la instancia: %+v", back)
	}
	// Una segunda instancia recibe OTRO marcador: son por instancia.
	if err := os.MkdirAll(m.dir("i2"), 0o755); err != nil {
		t.Fatal(err)
	}
	creds2, _, err := m.entregarCredenciales(context.Background(), "i2", knet.Plan(2, "i2"), falso.cliente(), specs)
	if err != nil || creds2[0].Placeholder == creds[0].Placeholder {
		t.Fatalf("segunda instancia: %+v, %v", creds2, err)
	}
}

// Un almacén escrito antes de que existiera Allow (sin la clave en el JSON) se
// sigue leyendo, con Allow vacío: todo permitido, como entonces. Lo mismo el
// de una plantilla.
func TestAlmacenDeAntesDeAllowSeSigueLeyendo(t *testing.T) {
	m := newTestManager(t)
	if err := os.MkdirAll(m.dir("m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	// El struct tal y como era, sellado igual que lo hacía el daemon.
	type credencialV1 struct{ Env, Domain, Placeholder, Secret string }
	sellado, err := m.sellar([]credencialV1{{"KEY", "api.example.com", "kling-cred-aa", credSecreto}}, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if err := escribirSellado(m.credPath("m1"), sellado); err != nil {
		t.Fatal(err)
	}
	back, err := m.cargarCredenciales("m1")
	if err != nil || len(back) != 1 || back[0].Secret != credSecreto || back[0].Allow != nil {
		t.Fatalf("almacén v1: %+v, %v", back, err)
	}
	if err := credproxy.ValidarCredenciales(back); err != nil {
		t.Fatalf("una credencial v1 debería seguir siendo válida: %v", err)
	}

	type specV1 struct {
		Domain string `json:"domain"`
		Env    string `json:"env"`
		Secret string `json:"secret"`
	}
	sellado, err = m.sellar([]specV1{{"api.example.com", "KEY", credSecreto}}, "snapshot:svc")
	if err != nil {
		t.Fatal(err)
	}
	if err := escribirSellado(m.credSnapPath("svc"), sellado); err != nil {
		t.Fatal(err)
	}
	specs, err := m.cargarCredencialesPlantilla("svc")
	if err != nil || len(specs) != 1 || specs[0].Allow != nil || specs[0].Secret != credSecreto {
		t.Fatalf("plantilla v1: %+v, %v", specs, err)
	}
}

// Allow viaja de la API al proxy y al almacén, normalizado; rotar sin él lo
// quita (va con la clave); uno mal formado se rechaza sin tocar nada.
func TestSetCredentialsConAllow(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	got := capturarRegistro(t)
	falso := nuevoFcFalso(t)
	mc := m.addForTest("m1")
	m.mu.Lock()
	mc.Egress = string(knet.EgressAllowlist)
	m.socket["m1"] = falso.Sock
	m.mu.Unlock()
	if err := os.MkdirAll(m.dir("m1"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for _, mal := range [][]string{{"GET v1"}, {"GET /v1/../admin"}, {"CONNECT /"}, {"GET /v1/**/x"}} {
		if _, err := m.SetCredentials(ctx, "m1", []api.CredentialSpec{
			{Domain: "api.example.com", Env: "KEY", Secret: credSecreto, Allow: mal},
		}); err == nil {
			t.Errorf("Allow %q debería rechazarse", mal)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("un rechazo no debe llegar al proxy: %+v", *got)
	}

	if _, err := m.SetCredentials(ctx, "m1", []api.CredentialSpec{
		{Domain: "api.example.com", Env: "KEY", Secret: credSecreto, Allow: []string{"get /v1/balance", "POST /v1/files/**"}},
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /v1/balance", "POST /v1/files/**"}
	if len(*got) != 1 || !reflect.DeepEqual((*got)[0][0].Allow, want) {
		t.Fatalf("al proxy llegó %+v", *got)
	}
	back, err := m.cargarCredenciales("m1")
	if err != nil || !reflect.DeepEqual(back[0].Allow, want) {
		t.Fatalf("almacén: %+v, %v", back, err)
	}

	// Rotar sin Allow: la credencial queda sin restricciones, con el mismo marcador.
	if _, err := m.SetCredentials(ctx, "m1", []api.CredentialSpec{
		{Domain: "api.example.com", Env: "KEY", Secret: credSecreto2},
	}); err != nil {
		t.Fatal(err)
	}
	if c := (*got)[1][0]; c.Allow != nil || c.Secret != credSecreto2 || c.Placeholder != (*got)[0][0].Placeholder {
		t.Errorf("rotación: %+v", c)
	}
}

// Las credenciales de plantilla guardan Allow y cada instancia lo recibe.
func TestCredencialesDePlantillaConAllow(t *testing.T) {
	m := newTestManager(t)
	got := capturarRegistro(t)
	escribirSnapshot(t, m, "svc", api.Snapshot{Egress: "allowlist"})
	if _, err := m.SetSnapshotCredentials("svc", []api.CredentialSpec{
		{Domain: "api.example.com", Env: "KEY", Secret: credSecreto, Allow: []string{"get /v1/*"}},
	}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetSnapshotCredentials("svc", []api.CredentialSpec{
		{Domain: "api.example.com", Env: "ORG", Secret: "x", Allow: []string{"GET /v1/../x"}},
	}, false); err == nil {
		t.Error("un Allow inválido en plantilla debería rechazarse")
	}
	specs, err := m.cargarCredencialesPlantilla("svc")
	if err != nil || len(specs) != 1 || !reflect.DeepEqual(specs[0].Allow, []string{"GET /v1/*"}) {
		t.Fatalf("almacén de plantilla: %+v, %v", specs, err)
	}
	falso := nuevoFcFalso(t)
	if err := os.MkdirAll(m.dir("i1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.entregarCredenciales(context.Background(), "i1", knet.Plan(1, "i1"), falso.cliente(), specs); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual((*got)[0][0].Allow, []string{"GET /v1/*"}) {
		t.Errorf("la instancia recibió %+v", (*got)[0][0])
	}
}
