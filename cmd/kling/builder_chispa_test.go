package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/train"
)

// tinyChispa entrena un modelo mínimo y devuelve sus bytes .chispa, para probar el
// constructor sin depender de un fichero externo.
func tinyChispa(t *testing.T) []byte {
	t.Helper()
	cfg := train.Config{Spec: chispa.DefaultSpec(), Seed: 1}
	cfg.Spec.Buckets = 1 << 10
	exs := []chispa.Example{
		{Text: "fix crash", Label: "fix"}, {Text: "fix bug", Label: "fix"},
		{Text: "add feature", Label: "feat"}, {Text: "add option", Label: "feat"},
	}
	res, err := train.Train(exs, exs, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := res.Model.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeRequest(t *testing.T, dir string, req api.BuildImageRequest) {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "request.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBuilderChispaMissingModel(t *testing.T) {
	dir := t.TempDir()
	writeRequest(t, dir, api.BuildImageRequest{Name: "chispa-x", Builder: "chispa", Spec: json.RawMessage(`{}`)})
	err := builderChispa(dir)
	if err == nil || !strings.Contains(err.Error(), "model_b64") {
		t.Fatalf("want a model_b64 error, got %v", err)
	}
}

func TestBuilderChispaInvalidModel(t *testing.T) {
	dir := t.TempDir()
	spec, _ := json.Marshal(ChispaSpec{ModelB64: base64.StdEncoding.EncodeToString([]byte("not a chispa file"))})
	writeRequest(t, dir, api.BuildImageRequest{Name: "chispa-x", Builder: "chispa", Spec: spec})
	err := builderChispa(dir)
	if err == nil || !strings.Contains(err.Error(), "does not load") {
		t.Fatalf("want a load error, got %v", err)
	}
}

func TestBuilderChispaInvalidBase64(t *testing.T) {
	dir := t.TempDir()
	spec, _ := json.Marshal(ChispaSpec{ModelB64: "not base64!!"})
	writeRequest(t, dir, api.BuildImageRequest{Name: "chispa-x", Builder: "chispa", Spec: spec})
	if err := builderChispa(dir); err == nil {
		t.Fatal("want an error for invalid base64")
	}
}

func TestBuilderChispaMissingAgent(t *testing.T) {
	dir := t.TempDir()
	spec, _ := json.Marshal(ChispaSpec{ModelB64: base64.StdEncoding.EncodeToString(tinyChispa(t))})
	writeRequest(t, dir, api.BuildImageRequest{Name: "chispa-x", Builder: "chispa", Spec: spec})
	t.Setenv("KLING_LIB_DIR", filepath.Join(t.TempDir(), "nowhere"))
	err := builderChispa(dir)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("want a missing-agent error, got %v", err)
	}
}

// TestBuilderChispaLayout prueba el constructor entero con un script de imagen
// base falso (sin montar loopback ni chroot: eso es del script, no de Go) y
// comprueba que la capa que arma en rootfs/ es la que 81-base-image.sh espera:
// el binario, el modelo, el .chispas opcional y el run.sh que los enlaza.
func TestBuilderChispaLayout(t *testing.T) {
	lib := t.TempDir()
	agent := filepath.Join(lib, "kling-guest")
	chispaBin := filepath.Join(lib, "kling-chispa")
	for _, p := range []string{agent, chispaBin} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(lib, "fake-base-image.sh")
	// El script falso solo copia su entorno a un fichero, para que el test
	// compruebe NAME/BASE/GROW/ROOTFS_DIR/SERVICE sin montar nada de verdad.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > \"$KLING_BUILD_DIR/env.dump\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KLING_LIB_DIR", lib)
	t.Setenv("KLING_GUEST_AGENT", agent)
	t.Setenv("KLING_CHISPA_AGENT", chispaBin)
	t.Setenv("KLING_BASE_IMAGE_SCRIPT", script)

	dir := t.TempDir()
	t.Setenv("KLING_BUILD_DIR", dir) // el fake script lo usa; el daemon real también lo pone
	modelBytes := tinyChispa(t)
	spec, _ := json.Marshal(ChispaSpec{ModelB64: base64.StdEncoding.EncodeToString(modelBytes)})
	writeRequest(t, dir, api.BuildImageRequest{Name: "chispa-commits", Builder: "chispa", Spec: spec})

	if err := builderChispa(dir); err != nil {
		t.Fatalf("builderChispa: %v", err)
	}

	envDump, err := os.ReadFile(filepath.Join(dir, "env.dump"))
	if err != nil {
		t.Fatalf("the fake script did not run: %v", err)
	}
	env := string(envDump)
	for _, want := range []string{"NAME=chispa-commits", "BASE=min", "SERVICE=/etc/chispa/run.sh"} {
		if !strings.Contains(env, want) {
			t.Errorf("script env missing %q:\n%s", want, env)
		}
	}
	rootfs := filepath.Join(dir, "rootfs")
	if _, err := os.Stat(filepath.Join(rootfs, "opt", "chispa", "kling-chispa")); err != nil {
		t.Errorf("kling-chispa not copied into the layer: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(rootfs, "models", "task.chispa"))
	if err != nil || string(got) != string(modelBytes) {
		t.Errorf("model not written as-is into the layer: %v", err)
	}
	run, err := os.ReadFile(filepath.Join(rootfs, "etc", "chispa", "run.sh"))
	if err != nil || !strings.Contains(string(run), "/opt/chispa/kling-chispa") || !strings.Contains(string(run), "/models/task.chispa") {
		t.Errorf("run.sh does not launch kling-chispa with the model: %v\n%s", err, run)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "models", "task.chispas")); err == nil {
		t.Error("no slots were given: task.chispas should not exist")
	}
}
