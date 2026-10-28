package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/oci/ocitest"
	"github.com/juan52878911/kindling/pkg/api"
)

// Con Isolate el constructor se parte en pull (baja y deja isolate.json, sin
// abrir ningún tar) y flatten (sin red, con el digest fijado y los blobs en su
// caché): la imagen que sale es byte a byte la de siempre.
func TestBuildOCIFases(t *testing.T) {
	e := newOCITest(t)
	_, idx := e.reg.ImageConfig("amd64", map[string]any{"Cmd": []string{"/bin/sh"}},
		ocitest.TarGz(alpineLike()), ocitest.TarGz([]ocitest.File{{Name: "etc/extra", Body: "x"}}))
	e.reg.Tag("1", idx)
	spec := OCISpec{Ref: e.reg.Host() + "/library/app:1", Arch: "amd64"}
	if _, log, err := e.build("app", spec); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	want, err := os.ReadFile(filepath.Join(e.root, "images", "app.ext4"))
	if err != nil {
		t.Fatal(err)
	}

	// Fase pull: con isolate en el spec, sin imagen y con el plan.
	spec.Isolate = true
	pullRoot := t.TempDir()
	t.Setenv("KLING_ROOT", pullRoot)
	t.Setenv(ociPhaseEnv, ociPhasePull)
	sb, _ := json.Marshal(spec)
	req, _ := json.Marshal(api.BuildImageRequest{Name: "app", Builder: "oci", Spec: sb})
	os.WriteFile(filepath.Join(e.work, "request.json"), req, 0o600)
	var plog bytes.Buffer
	if err := buildOCI(context.Background(), e.work, &plog); err != nil {
		t.Fatalf("pull: %v\n%s", err, plog.String())
	}
	if _, err := os.Stat(filepath.Join(pullRoot, "images", "app.ext4")); err == nil {
		t.Fatal("the pull phase wrote an image")
	}
	var plan IsolatePlan
	pb, err := os.ReadFile(filepath.Join(e.work, isolateFile))
	if err != nil || json.Unmarshal(pb, &plan) != nil {
		t.Fatalf("no plan: %v", err)
	}
	if len(plan.Blobs) < 3 || plan.Epoch != 1790000000 {
		t.Errorf("plan: %+v", plan)
	}

	// Fase flatten: una raíz nueva con solo los blobs; la red está cerrada.
	e.reg.Close()
	flat := t.TempDir()
	for _, d := range plan.Blobs {
		h := strings.TrimPrefix(d, "sha256:")
		b, err := os.ReadFile(filepath.Join(pullRoot, "cache", "oci", "sha256", h))
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(flat, "cache", "oci", "sha256"), 0o755)
		os.WriteFile(filepath.Join(flat, "cache", "oci", "sha256", h), b, 0o644)
	}
	t.Setenv("KLING_ROOT", flat)
	t.Setenv(ociPhaseEnv, ociPhaseFlat)
	t.Setenv(ociPinnedEnv, plan.Digest)
	t.Setenv("KLING_OCI_NO_REUSE", "1")
	if _, log, err := e.build("app", spec); err != nil {
		t.Fatalf("flatten: %v\n%s", err, log)
	}
	got, err := os.ReadFile(filepath.Join(flat, "images", "app.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the isolated image (%d bytes) differs from the direct one (%d)", len(got), len(want))
	}

	// Sin digest fijado, o con uno que no está en la caché, no hay imagen.
	t.Setenv(ociPinnedEnv, "")
	if _, _, err := e.build("app", spec); err == nil {
		t.Error("flatten without a pinned digest worked")
	}
	t.Setenv(ociPinnedEnv, "sha256:"+strings.Repeat("0", 64))
	if _, _, err := e.build("app", spec); err == nil {
		t.Error("flatten with a digest that isn't cached worked")
	}
}
