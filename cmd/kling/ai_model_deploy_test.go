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
