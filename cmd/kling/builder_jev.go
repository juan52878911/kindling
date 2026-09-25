package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/jev"
	"github.com/juan52878911/kindling/pkg/jev/slots"
)

// CONSTRUCTOR "jev": una imagen con un modelo JEV (y, opcional, sus huecos)
// servida por kling-jev en el puerto del invitado (jev.GuestPort). Lo ejecuta
// el daemon como root en su host:
//
//	kling builder jev <directorio de trabajo>
//
// Es el mismo motor que el constructor "llm" (81-base-image.sh, ROOTFS_DIR +
// SERVICE), pero sin nada que descargar: el .jev es del tamaño de un fichero
// de configuración (cientos de KB a pocos MB) y viaja entero, en base64,
// dentro del spec — así construir la imagen no depende de que el daemon vea
// el disco de quien manda la petición, que puede estar al otro lado de un SSH
// (docs/jev-serverless.md). Antes de escribirlo en la imagen se comprueba que
// carga de verdad (jev.Load / slots.Load): un fichero corrupto no debe acabar
// congelado en un dorado que nadie puede arrancar.

// JEVSpec es el spec del constructor "jev".
type JEVSpec struct {
	// ModelB64 es el contenido del .jev, en base64.
	ModelB64 string `json:"model_b64"`
	// SlotsB64 es el .jevs opcional (huecos, docs/domotica.md), en base64.
	SlotsB64 string `json:"slots_b64,omitempty"`
}

func builderJEV(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		return err
	}
	var req api.BuildImageRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return fmt.Errorf("request.json: %w", err)
	}
	var spec JEVSpec
	if err := json.Unmarshal(req.Spec, &spec); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	if err := validateBase(req, BaseSpec{}); err != nil {
		return err
	}
	if spec.ModelB64 == "" {
		return fmt.Errorf("the jev builder needs model_b64 (kling jev deploy reads it from your -model file)")
	}
	modelBytes, err := base64.StdEncoding.DecodeString(spec.ModelB64)
	if err != nil {
		return fmt.Errorf("model_b64: %w", err)
	}
	m, err := jev.Load(bytes.NewReader(modelBytes))
	if err != nil {
		return fmt.Errorf("the decoded model does not load: %w", err)
	}
	var slotsBytes []byte
	if spec.SlotsB64 != "" {
		if slotsBytes, err = base64.StdEncoding.DecodeString(spec.SlotsB64); err != nil {
			return fmt.Errorf("slots_b64: %w", err)
		}
		if _, err := slots.Load(bytes.NewReader(slotsBytes)); err != nil {
			return fmt.Errorf("the decoded slots model does not load: %w", err)
		}
	}

	lib := envOr("KLING_LIB_DIR", "/usr/local/lib/kindling")
	agent := envOr("KLING_GUEST_AGENT", filepath.Join(lib, "kling-guest"))
	jevBin := envOr("KLING_JEV_AGENT", filepath.Join(lib, "kling-jev"))
	script := envOr("KLING_BASE_IMAGE_SCRIPT", filepath.Join(lib, "81-base-image.sh"))
	for _, p := range []string{agent, jevBin, script} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing %s (installed by `make deploy`)", p)
		}
	}

	rootfs := filepath.Join(dir, "rootfs")
	optDir := filepath.Join(rootfs, "opt", "jev")
	if err := os.MkdirAll(optDir, 0o755); err != nil {
		return err
	}
	if err := copyFile(jevBin, filepath.Join(optDir, "kling-jev")); err != nil {
		return fmt.Errorf("copying kling-jev: %w", err)
	}
	if err := os.Chmod(filepath.Join(optDir, "kling-jev"), 0o755); err != nil {
		return err
	}

	modelsDir := filepath.Join(rootfs, "models")
	if err := os.MkdirAll(modelsDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(modelsDir, "task.jev"), modelBytes, 0o644); err != nil {
		return err
	}
	args := []string{"-model", "/models/task.jev", "-listen", ":" + strconv.Itoa(jev.GuestPort)}
	if slotsBytes != nil {
		if err := os.WriteFile(filepath.Join(modelsDir, "task.jevs"), slotsBytes, 0o644); err != nil {
			return err
		}
		args = append(args, "-slots", "/models/task.jevs")
	}

	etc := filepath.Join(rootfs, "etc", "jev")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return err
	}
	runsh := "#!/bin/sh\nexec /opt/jev/kling-jev"
	for _, a := range args {
		runsh += " " + shQuote(a)
	}
	runsh += "\n"
	if err := os.WriteFile(filepath.Join(etc, "run.sh"), []byte(runsh), 0o755); err != nil {
		return err
	}
	manifest, _ := json.MarshalIndent(map[string]any{
		"labels": m.Labels, "buckets": m.Spec.Buckets, "port": jev.GuestPort,
		"slots": slotsBytes != nil,
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(etc, "model.json"), append(manifest, '\n'), 0o644); err != nil {
		return err
	}

	// La capa: el binario (unos 10 MB estáticos) más el modelo, con margen.
	grow := req.GrowMB
	if grow == 0 {
		st, _ := os.Stat(jevBin)
		grow = int((st.Size()+int64(len(modelBytes))+int64(len(slotsBytes)))>>20) + 24
	}
	base := req.Base
	if base == "" {
		base = "min"
	}
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		return err
	}
	cmd := exec.Command("bash", script)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(),
		"NAME="+req.Name, "BASE="+base, fmt.Sprintf("GROW=%d", grow),
		"PKGS=", "AGENT="+agent, "ENV_FILE="+envFile,
		"ROOTFS_DIR="+rootfs, "SERVICE=/etc/jev/run.sh")
	if err := cmd.Run(); err != nil {
		return err
	}
	fmt.Printf("jev model: %d label(s), %d bytes%s\n", len(m.Labels), len(modelBytes), func() string {
		if slotsBytes != nil {
			return fmt.Sprintf(" + slots (%d bytes)", len(slotsBytes))
		}
		return ""
	}())
	return nil
}

// shQuote entrecomilla para el /bin/sh de la imagen (busybox en Alpine), igual
// que 81-base-image.sh hace con las variables de entorno: los argumentos son
// rutas fijas que pone este constructor, no algo que escriba un usuario, pero
// entrecomillar es gratis y evita sorpresas si algún día dejan de serlo.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
