package aigw

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// ficheroSecreto escribe value en dir/name con permisos perm y devuelve la ruta.
func ficheroSecreto(t *testing.T, dir, name, value string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(value), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
	return p
}

// modeloConSecretos es un bloque http con dorado y los secretos dados.
func modeloConSecretos(idle string, secrets ...SecretConfig) *ModelConfig {
	return &ModelConfig{Kind: KindHTTP, Contract: ContractChispaDecide, Snapshot: "chispa-acme", Port: 8090,
		Idle: idle, Secrets: secrets}
}

func TestConfigSecretsValida(t *testing.T) {
	dir := t.TempDir()
	bueno := ficheroSecreto(t, dir, "ok.key", "s3cr3t", 0o600)
	abierto := ficheroSecreto(t, dir, "abierto.key", "s3cr3t", 0o644)
	cred := func(mod func(*SecretConfig)) SecretConfig {
		s := SecretConfig{Name: "l3", Deliver: DeliverCredential, File: bueno, Env: "L3_API_KEY",
			Domain: "l3.kindling.internal", Allow: []string{"POST /v1/chat/completions"}}
		if mod != nil {
			mod(&s)
		}
		return s
	}
	mmds := SecretConfig{Name: "licencia", Deliver: DeliverMMDS, File: bueno}
	casos := map[string]*ModelConfig{
		"deliver desconocido":    modeloConSecretos("", SecretConfig{Name: "x", Deliver: "env", File: bueno}),
		"fichero relativo":       modeloConSecretos("", SecretConfig{Name: "x", Deliver: DeliverMMDS, File: "ok.key"}),
		"fichero no 0600":        modeloConSecretos("", SecretConfig{Name: "x", Deliver: DeliverMMDS, File: abierto}),
		"fichero que no está":    modeloConSecretos("", SecretConfig{Name: "x", Deliver: DeliverMMDS, File: filepath.Join(dir, "no")}),
		"mmds con freeze":        modeloConSecretos(IdleFreeze, mmds),
		"mmds con env":           modeloConSecretos("", SecretConfig{Name: "x", Deliver: DeliverMMDS, File: bueno, Env: "X"}),
		"credential sin allow":   modeloConSecretos("", cred(func(s *SecretConfig) { s.Allow = nil })),
		"credential sin domain":  modeloConSecretos("", cred(func(s *SecretConfig) { s.Domain = "" })),
		"credential sin env":     modeloConSecretos("", cred(func(s *SecretConfig) { s.Env = "" })),
		"allow mal escrito":      modeloConSecretos("", cred(func(s *SecretConfig) { s.Allow = []string{"/v1/x"} })),
		"upstream sin puerto":    modeloConSecretos("", cred(func(s *SecretConfig) { s.Upstream = "127.0.0.1" })),
		"nombre inválido":        modeloConSecretos("", SecretConfig{Name: "Lic/1", Deliver: DeliverMMDS, File: bueno}),
		"nombre repetido":        modeloConSecretos("", mmds, mmds),
		"env repetido":           modeloConSecretos("", cred(nil), cred(func(s *SecretConfig) { s.Name = "otro" })),
		"secrets en un upstream": {Kind: KindHTTP, Contract: ContractOpenAI, Upstream: "http://10.0.0.5:8000", Secrets: []SecretConfig{mmds}},
		"secrets en kind von":    {Kind: KindVON, Snapshot: "von-smol", Secrets: []SecretConfig{mmds}},
	}
	for nombre, m := range casos {
		if errs := validateSecrets("m", m); len(errs) == 0 {
			t.Errorf("%s: aceptado", nombre)
		}
	}
	for nombre, m := range map[string]*ModelConfig{
		"credential":        modeloConSecretos("", cred(nil)),
		"credential+mmds":   modeloConSecretos(IdlePause, cred(func(s *SecretConfig) { s.Upstream = "127.0.0.1:8080" }), mmds),
		"mmds con remove":   modeloConSecretos(IdleRemove, mmds),
		"sin secrets (hoy)": {Kind: KindVON, Snapshot: "von-smol"},
	} {
		if errs := validateSecrets("m", m); len(errs) != 0 {
			t.Errorf("%s: rechazado: %v", nombre, errs)
		}
	}
	// Ningún error repite el contenido del fichero.
	for _, m := range casos {
		for _, err := range validateSecrets("m", m) {
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("un error de validación lleva el valor del secreto: %v", err)
			}
		}
	}
}

// registroConSecretos escribe un ai.json con el bloque "acme" y sus secretos.
func registroConSecretos(t *testing.T, p string, secrets []SecretConfig) {
	t.Helper()
	escribirRegistro(t, p, map[string]any{"acme": map[string]any{
		"kind": "http", "contract": "chispa-decide", "snapshot": "chispa-acme", "port": 8090,
		"idle": "remove", "secrets": secrets,
	}}, nil)
}

// gwConDaemon arma un gateway con ai.json en p contra el daemon d.
func gwConDaemon(t *testing.T, d *daemonCiclo, p string) *Gateway {
	t.Helper()
	g, err := New(Options{Client: d.serve(t), ConfigPath: p})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// P1 clase B: el gateway ata la credencial al dorado y a las réplicas vivas
// al arrancar; rotar el fichero y recargar la vuelve a atar sin dorado nuevo,
// y recargar sin cambios no llama al daemon.
func TestReloadRotaCredencial(t *testing.T) {
	dir := t.TempDir()
	f := ficheroSecreto(t, dir, "l3.token", "clave-uno", 0o600)
	p := filepath.Join(dir, "ai.json")
	registroConSecretos(t, p, []SecretConfig{{Name: "l3", Deliver: DeliverCredential, File: f, Env: "L3_API_KEY",
		Domain: "l3.kindling.internal", Allow: []string{"POST /v1/chat/completions"}}})
	viva := maquina("viva", "chispa-acme", "default", api.StateRunning)
	d := &daemonCiclo{machines: []*api.Machine{viva}}
	g := gwConDaemon(t, d, p)
	ctx := context.Background()
	g.aplicarSecretos(ctx)
	snap, maq := d.credenciales()
	if len(snap) != 1 || snap[0] != "chispa-acme:L3_API_KEY=clave-uno" {
		t.Fatalf("credenciales del dorado = %v", snap)
	}
	if len(maq) != 1 || maq[0] != "viva:L3_API_KEY=clave-uno" {
		t.Fatalf("credenciales de las réplicas vivas = %v", maq)
	}
	if _, err := g.Reload(); err != nil {
		t.Fatal(err)
	}
	if s, m := d.credenciales(); len(s) != 1 || len(m) != 1 {
		t.Fatalf("recargar sin cambiar el fichero volvió a atar: %v %v", s, m)
	}
	ficheroSecreto(t, dir, "l3.token", "clave-dos", 0o600)
	if _, err := g.Reload(); err != nil {
		t.Fatal(err)
	}
	s, m := d.credenciales()
	if len(s) != 2 || s[1] != "chispa-acme:L3_API_KEY=clave-dos" || len(m) != 2 || m[1] != "viva:L3_API_KEY=clave-dos" {
		t.Fatalf("tras rotar: dorado %v, réplicas %v", s, m)
	}
}

// P1 clase C: rotar un secreto en claro retira las réplicas del dorado (no
// reciben peticiones nuevas y se borran al quedarse sin trabajo, ver
// scheduler.Retire); sin cambio no se retira nada. El primer arranque no
// retira: no hay réplicas de una generación anterior conocida.
func TestRotacionMMDSDrena(t *testing.T) {
	dir := t.TempDir()
	f := ficheroSecreto(t, dir, "lic.key", "lic-uno", 0o600)
	p := filepath.Join(dir, "ai.json")
	registroConSecretos(t, p, []SecretConfig{{Name: "licencia", Deliver: DeliverMMDS, File: f}})
	d := &daemonCiclo{}
	g := gwConDaemon(t, d, p)
	var mu sync.Mutex
	var retirados []string
	g.retirar = func(_ context.Context, snap string) {
		mu.Lock()
		retirados = append(retirados, snap)
		mu.Unlock()
	}
	g.aplicarSecretos(context.Background())
	if _, err := g.Reload(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(retirados) != 0 {
		t.Fatalf("se retiró %v sin rotar nada", retirados)
	}
	mu.Unlock()
	ficheroSecreto(t, dir, "lic.key", "lic-dos", 0o600)
	if _, err := g.Reload(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(retirados, ",") != "chispa-acme" {
		t.Fatalf("retirados tras rotar = %v; quería chispa-acme", retirados)
	}
}

// AfterRestore del gateway: pone los secretos en claro en el MMDS de la
// réplica y corre sus ganchos; marca la máquina. Si los ganchos fallan, el
// error no lleva el valor aunque el invitado lo repita en su detalle. Sin
// secretos mmds, no llama al daemon (clases A y B: 0 ms).
func TestAfterRestoreEntregaSinFiltrar(t *testing.T) {
	dir := t.TempDir()
	f := ficheroSecreto(t, dir, "lic.key", "lic-secreta\n", 0o600)
	p := filepath.Join(dir, "ai.json")
	registroConSecretos(t, p, []SecretConfig{{Name: "licencia", Deliver: DeliverMMDS, File: f}})
	d := &daemonCiclo{machines: []*api.Machine{maquina("r1", "chispa-acme", "default", api.StateRunning)}}
	g := gwConDaemon(t, d, p)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mc := &api.Machine{ID: "r1"}
	if err := g.entregarSecretos(ctx, "chispa-acme", mc); err != nil {
		t.Fatalf("entregar: %v", err)
	}
	if !mc.HasSecrets {
		t.Error("la réplica no quedó marcada HasSecrets")
	}
	if got := d.mmdsDe("r1"); got != `{"secrets":{"licencia":"lic-secreta"}}` {
		t.Errorf("MMDS = %s", got)
	}
	d.mu.Lock()
	d.hooksFallan = "guest said: lic-secreta is invalid"
	d.mu.Unlock()
	err := g.entregarSecretos(ctx, "chispa-acme", &api.Machine{ID: "r1"})
	if err == nil {
		t.Fatal("unos ganchos que fallan no dieron error")
	}
	if strings.Contains(err.Error(), "lic-secreta") {
		t.Errorf("el error lleva el valor del secreto: %v", err)
	}
	// Un dorado sin secretos mmds: ninguna llamada.
	antes := d.llamadas()
	if err := g.entregarSecretos(ctx, "von-smol", &api.Machine{ID: "r2"}); err != nil {
		t.Fatal(err)
	}
	if d.llamadas() != antes {
		t.Error("sin secretos mmds, AfterRestore llamó al daemon")
	}
}

// credencialesTexto resume una lista de credenciales como "ref:ENV=valor".
func credencialesTexto(ref string, cs []api.CredentialSpec) []string {
	var out []string
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s:%s=%s", ref, c.Env, c.Secret))
	}
	return out
}

// mmdsJSON compacta el documento MMDS recibido.
func mmdsJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
