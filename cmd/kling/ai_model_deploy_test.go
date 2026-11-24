package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/aigw"
	"github.com/juan52878911/kindling/internal/dorado"
	"github.com/juan52878911/kindling/pkg/api"
)

const registroBase = `{
  "models": {
    "tickets": {"kind": "chispa", "path": "tickets.chispa"}
  },
  "tasks": {
    "triage": {"chispa": "tickets"}
  }
}
`

func escribeRegistro(t *testing.T, contenido string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ai.json")
	if err := os.WriteFile(p, []byte(contenido), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func leeModelos(t *testing.T, p string) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Models map[string]map[string]any `json:"models"`
	}
	if err := json.Unmarshal(b, &reg); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	return reg.Models
}

func opcionesDeploy(cfg string) modelDeployOptions {
	return modelDeployOptions{
		Name: "srv", Image: "srv-img", Port: 8090, Contract: "chispa-decide",
		Health: aigw.HealthConfig{Path: "/healthz"},
		Warmup: []dorado.Request{{Method: "POST", Path: "/v1/decide", Body: []byte(`{"q":"sintético"}`)}},
		VCPUs:  2, MemMiB: 512, Idle: "remove", MaxInflight: 16,
		Wait: 2 * time.Second, Config: cfg,
	}
}

func TestModelDeployEscribeBloque(t *testing.T) {
	cfg := escribeRegistro(t, registroBase)
	// Ya hay una v1 en el daemon (de otro registro, o borrada de éste): la
	// siguiente es la v2, nunca se pisa una.
	d := &daemonModelos{snaps: []*api.Snapshot{{Name: "srv-v1"}, {Name: "otro-v9"}}}
	c := d.cliente(t)
	r, err := modelDeploy(context.Background(), c, opcionesDeploy(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if r.Snapshot != "srv-v2" || r.Version != 2 || !r.Written {
		t.Fatalf("resultado: %+v", r)
	}
	d.mu.Lock()
	run, commit := d.runs[0], d.commits[0]
	llamadas := strings.Join(d.llamadas, "|")
	d.mu.Unlock()
	if run.Image != "srv-img" || run.CPUPct != 200 || run.Egress != "none" || run.Labels[api.LabelPorts] != "8090" ||
		run.Labels[dorado.LabelModel] != "srv" || run.Labels[dorado.LabelVersion] != "2" || run.Labels[api.LabelService] != "srv-v2" {
		t.Fatalf("run: %+v", run)
	}
	if commit.Name != "srv-v2" || commit.Replace {
		t.Fatalf("commit: %+v", commit)
	}
	if !strings.Contains(llamadas, "guest GET /healthz :8090|guest POST /v1/decide :8090|squeeze|commit srv-v2") {
		t.Fatalf("llamadas: %s", llamadas)
	}
	ms := leeModelos(t, cfg)
	b := ms["srv"]
	if b["kind"] != "http" || b["snapshot"] != "srv-v2" || b["port"] != float64(8090) || b["contract"] != "chispa-decide" ||
		b["idle"] != "remove" || b["max_inflight"] != float64(16) {
		t.Fatalf("bloque: %v", b)
	}
	if h, _ := b["health"].(map[string]any); h["path"] != "/healthz" {
		t.Fatalf("health: %v", b["health"])
	}
	// Lo demás del registro sigue igual, con su ruta relativa intacta.
	if ms["tickets"]["path"] != "tickets.chispa" {
		t.Fatalf("tickets: %v", ms["tickets"])
	}
	raw, _ := os.ReadFile(cfg)
	if !strings.Contains(string(raw), `"triage"`) {
		t.Fatalf("se perdieron las tareas:\n%s", raw)
	}
	if st, _ := os.Stat(cfg); st.Mode().Perm() != 0o600 {
		t.Fatalf("modo %v: un ai.json con tokens es 0600 y debe seguir así", st.Mode().Perm())
	}
}

// Un modelo que ya está en el registro: se hace la versión nueva, pero ai.json
// no cambia hasta `kling ai model roll`.
func TestModelDeployModeloExistente(t *testing.T) {
	orig := `{"models": {"srv": {"kind": "http", "snapshot": "srv-v1", "port": 8090, "contract": "chispa-decide"}}}`
	cfg := escribeRegistro(t, orig)
	d := &daemonModelos{snaps: []*api.Snapshot{{Name: "srv-v1"}}}
	r, err := modelDeploy(context.Background(), d.cliente(t), opcionesDeploy(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if r.Written || r.Snapshot != "srv-v2" {
		t.Fatalf("resultado: %+v", r)
	}
	if b, _ := os.ReadFile(cfg); string(b) != orig {
		t.Fatalf("ai.json cambió:\n%s", b)
	}
}

func TestModelDeployCreaRegistro(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "sub", "ai.json")
	d := &daemonModelos{}
	r, err := modelDeploy(context.Background(), d.cliente(t), opcionesDeploy(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if r.Snapshot != "srv-v1" || leeModelos(t, cfg)["srv"]["snapshot"] != "srv-v1" {
		t.Fatalf("%+v %v", r, leeModelos(t, cfg))
	}
	if st, _ := os.Stat(cfg); st.Mode().Perm() != 0o600 {
		t.Fatalf("un registro nuevo nace 0600: %v", st.Mode().Perm())
	}
}

// Lo que se puede rechazar sin cargar nada se rechaza antes de tocar el daemon.
func TestModelDeployRechazaAntes(t *testing.T) {
	cfgVon := `{"models": {"srv": {"kind": "von", "snapshot": "von-smol"}}}`
	for nombre, caso := range map[string]struct {
		reg string
		mod func(*modelDeployOptions)
		msg string
	}{
		"kind distinto": {cfgVon, func(*modelDeployOptions) {}, "not an http backend"},
		"contrato":      {registroBase, func(o *modelDeployOptions) { o.Contract = "grpc" }, "-contract"},
		// El gateway no acepta un bloque kind http con chispa-classify: lo
		// que escribiría deploy no cargaría.
		"chispa-classify": {registroBase, func(o *modelDeployOptions) { o.Contract = "chispa-classify" }, "kling ai chispa deploy"},
		"raw sin rutas":   {registroBase, func(o *modelDeployOptions) { o.Contract = "raw" }, "-route"},
		"idle":            {registroBase, func(o *modelDeployOptions) { o.Idle = "sleep" }, "-idle"},
		"nombre":          {registroBase, func(o *modelDeployOptions) { o.Name = "Srv" }, "model name"},
		"env secreto":     {registroBase, func(o *modelDeployOptions) { o.Env = []string{"OPENAI_API_KEY=sk"} }, "-allow-env"},
		"health timeout":  {registroBase, func(o *modelDeployOptions) { o.Health.Timeout = "pronto" }, "-health-timeout"},
	} {
		cfg := escribeRegistro(t, caso.reg)
		d := &daemonModelos{}
		o := opcionesDeploy(cfg)
		caso.mod(&o)
		_, err := modelDeploy(context.Background(), d.cliente(t), o)
		if err == nil || !strings.Contains(err.Error(), caso.msg) {
			t.Fatalf("%s: err = %v, quería %q", nombre, err, caso.msg)
		}
		d.mu.Lock()
		if len(d.runs) != 0 {
			t.Fatalf("%s: arrancó una máquina", nombre)
		}
		d.mu.Unlock()
		if b, _ := os.ReadFile(cfg); string(b) != caso.reg {
			t.Fatalf("%s: ai.json cambió", nombre)
		}
	}
}

// El dorado que no se hace no deja bloque en el registro.
func TestModelDeployWarmupFallaNoEscribe(t *testing.T) {
	cfg := escribeRegistro(t, registroBase)
	d := &daemonModelos{estado: map[string]int{"/v1/decide": 400}}
	if _, err := modelDeploy(context.Background(), d.cliente(t), opcionesDeploy(cfg)); err == nil {
		t.Fatal("un calentamiento 400 debe fallar")
	}
	if b, _ := os.ReadFile(cfg); string(b) != registroBase {
		t.Fatalf("ai.json cambió:\n%s", b)
	}
}

const registroInquilinos = `{
  "tenants": [{"name": "acme", "token": "acme-token-0123456789"}, {"name": "globex", "token": "globex-token-012345678"}],
  "models": {}
}
`

// P4: -tenant y -alias escriben tenants y alias en el bloque; un inquilino que
// no está en ai.json falla antes de fabricar.
func TestDeployEscribeTenants(t *testing.T) {
	cfg := escribeRegistro(t, registroInquilinos)
	d := &daemonModelos{}
	o := opcionesDeploy(cfg)
	o.Name, o.Tenants, o.Alias = "chispa-acme", []string{"acme"}, "chispa"
	if _, err := modelDeploy(context.Background(), d.cliente(t), o); err != nil {
		t.Fatal(err)
	}
	b := leeModelos(t, cfg)["chispa-acme"]
	if ts, _ := b["tenants"].([]any); len(ts) != 1 || ts[0] != "acme" || b["alias"] != "chispa" || b["shared"] != nil {
		t.Fatalf("bloque: %v", b)
	}
	// shared, cuando se pide, también.
	o.Name, o.Tenants, o.Shared = "chispa-todos", []string{"acme", "globex"}, true
	if _, err := modelDeploy(context.Background(), d.cliente(t), o); err != nil {
		t.Fatal(err)
	}
	if b := leeModelos(t, cfg)["chispa-todos"]; b["shared"] != true {
		t.Fatalf("bloque: %v", b)
	}
	for nombre, mod := range map[string]func(*modelDeployOptions){
		"desconocido":  func(o *modelDeployOptions) { o.Tenants = []string{"initech"} },
		"repetido":     func(o *modelDeployOptions) { o.Tenants = []string{"acme", "acme"} },
		"alias malo":   func(o *modelDeployOptions) { o.Tenants, o.Alias = []string{"acme"}, "Chispa!" },
		"alias=nombre": func(o *modelDeployOptions) { o.Tenants, o.Alias = []string{"acme"}, o.Name },
	} {
		cfg := escribeRegistro(t, registroInquilinos)
		d := &daemonModelos{}
		o := opcionesDeploy(cfg)
		o.Name = "chispa-x"
		mod(&o)
		if _, err := modelDeploy(context.Background(), d.cliente(t), o); err == nil {
			t.Fatalf("%s: debería fallar", nombre)
		}
		d.mu.Lock()
		if len(d.runs) != 0 {
			t.Fatalf("%s: arrancó una máquina antes de validar", nombre)
		}
		d.mu.Unlock()
	}
	// Sin inquilinos en ai.json, -tenant no tiene a quién referirse.
	cfg = escribeRegistro(t, registroBase)
	o = opcionesDeploy(cfg)
	o.Tenants = []string{"acme"}
	d = &daemonModelos{}
	if _, err := modelDeploy(context.Background(), d.cliente(t), o); err == nil || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("err %v", err)
	}
}

// C9-10: tras fabricar, deploy pide al daemon que traiga el dorado a la caché
// de página; un daemon anterior (404) o sin la capacidad (501) no es un fallo.
func TestDeployPrecalientaYToleraDaemonViejo(t *testing.T) {
	for _, cod := range []int{200, 404, 405, 501, 500} {
		cfg := escribeRegistro(t, registroBase)
		d := &daemonModelos{warmCod: cod}
		r, err := modelDeploy(context.Background(), d.cliente(t), opcionesDeploy(cfg))
		if err != nil {
			t.Fatalf("%d: un precalentado que no se puede no tumba el despliegue: %v", cod, err)
		}
		d.mu.Lock()
		llamadas := strings.Join(d.llamadas, "|")
		calentados := d.calentados
		d.mu.Unlock()
		if !strings.Contains(llamadas, "commit srv-v1|") || !strings.Contains(llamadas, "warm srv-v1") || !r.Written {
			t.Fatalf("%d: llamadas %s", cod, llamadas)
		}
		if cod == 200 && (len(calentados) != 1 || calentados[0] != "srv-v1") {
			t.Fatalf("calentados: %v", calentados)
		}
	}
}

// C9-15: un bloque idle remove (el defecto de kind http) nunca congela
// réplicas en diferencial, así que su plantilla pide no espejar el dorado.
func TestDeployEtiquetaSinEspejoConIdleRemove(t *testing.T) {
	for idle, quiero := range map[string]string{"remove": api.GoldenMirrorOff, "": api.GoldenMirrorOff, "freeze": "", "pause": ""} {
		cfg := escribeRegistro(t, registroBase)
		d := &daemonModelos{}
		o := opcionesDeploy(cfg)
		o.Idle = idle
		if _, err := modelDeploy(context.Background(), d.cliente(t), o); err != nil {
			t.Fatal(err)
		}
		d.mu.Lock()
		got := d.runs[0].Labels[api.LabelGoldenMirror]
		d.mu.Unlock()
		if got != quiero {
			t.Fatalf("idle %q: %s = %q, quería %q", idle, api.LabelGoldenMirror, got, quiero)
		}
	}
	// Un modelo que ya está: manda el idle de su bloque, no la bandera.
	cfg := escribeRegistro(t, `{"models": {"srv": {"kind": "http", "snapshot": "srv-v1", "port": 8090, "contract": "chispa-decide", "idle": "freeze"}}}`)
	d := &daemonModelos{snaps: []*api.Snapshot{{Name: "srv-v1"}}}
	if _, err := modelDeploy(context.Background(), d.cliente(t), opcionesDeploy(cfg)); err != nil {
		t.Fatal(err)
	}
	if got := d.runs[0].Labels[api.LabelGoldenMirror]; got != "" {
		t.Fatalf("el bloque dice freeze: %q", got)
	}
}

// P2, P3 y P5: las banderas nuevas llegan al fabricante.
func TestDeployOpcionesDelFabricante(t *testing.T) {
	cfg := escribeRegistro(t, registroBase)
	d := &daemonModelos{}
	o := opcionesDeploy(cfg)
	o.Wait, o.ModelBytes = 0, 1900<<20
	o.AllowDomains = []string{"huggingface.co"}
	o.WarmupKey = true
	o.Warmup = []dorado.Request{{Method: "POST", Path: "/v1/decide", Headers: map[string]string{"Authorization": "Bearer " + dorado.WarmupKeyPlaceholder}}}
	if _, err := modelDeploy(context.Background(), d.cliente(t), o); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	run := d.runs[0]
	d.mu.Unlock()
	if run.Egress != "allowlist" || len(run.AllowDomains) != 1 {
		t.Fatalf("run: %+v", run)
	}
	conClave := false
	for _, kv := range run.Env {
		conClave = conClave || strings.HasPrefix(kv, dorado.EnvWarmupKeySHA256+"=")
	}
	if !conClave {
		t.Fatalf("env: %v", run.Env)
	}
}

// P5: -credential lee la credencial de un fichero JSON que solo lee su dueño:
// la clave nunca va en una bandera.
func TestDeployCredencialFichero(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l3.json")
	if err := os.WriteFile(p, []byte(`{"domain":"l3.kindling.internal","env":"L3_TOKEN","secret":"s3cr3t","allow":["POST /v1/chat/completions"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := leerCredencial(p)
	if err != nil || c.Secret != "s3cr3t" || c.Env != "L3_TOKEN" || len(c.Allow) != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := leerCredencial(p); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("un fichero con la clave legible por otros debería rechazarse: %v", err)
	}
	_ = os.WriteFile(p, []byte(`{"domain":"x","env":"Y","secret":"z","otro":1}`), 0o600)
	_ = os.Chmod(p, 0o600)
	if _, err := leerCredencial(p); err == nil {
		t.Fatal("un campo desconocido debería rechazarse")
	}
}
