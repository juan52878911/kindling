package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// CONSTRUCTORES DEL NÚCLEO.
//
// El daemon construye imágenes ejecutando, como root, un constructor instalado
// en /usr/local/lib/kindling/builders/<nombre> (ver docs/api.md). El del núcleo
// es "base": una capa sobre una imagen base con paquetes del sistema y el agente
// de invitado genérico, kling-guest, como PID 1. Es lo que lleva la imagen de
// herramientas de `kling volume populate`, y la base de un sandbox.
//
// El fichero instalado es un envoltorio de dos líneas que llama aquí:
//
//	kling builder base <directorio de trabajo>
//
// Todo se valida en Go antes de que llegue a un shell que corre como root.

// BaseSpec es el spec del constructor "base".
type BaseSpec struct {
	// Packages son paquetes del gestor de la base (apk en Alpine, apt en la base
	// glibc).
	Packages []string `json:"packages,omitempty"`
	// Env son variables KEY=VALUE que el entrypoint exporta antes de arrancar el
	// agente.
	Env []string `json:"env,omitempty"`
}

var (
	reBuildName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	reBuildPkg  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]*$`)
	// El valor no puede llevar saltos de línea ni NUL: se escribe una línea por
	// variable y el script la entrecomilla para sh.
	reBuildEnv = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[^\x00\r\n]*$`)
)

func cmdBuilder(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: kling builder base|llm <workdir>  (the daemon runs it; see docs/api.md)")
	}
	switch args[0] {
	case "base":
		return builderBase(args[1])
	case "llm":
		return builderLLM(args[1])
	default:
		return fmt.Errorf("unknown builder %q", args[0])
	}
}

// validateBase comprueba la petición del constructor base.
func validateBase(req api.BuildImageRequest, spec BaseSpec) error {
	if !reBuildName.MatchString(req.Name) {
		return fmt.Errorf("invalid image name %q", req.Name)
	}
	if req.Base != "" && !reBuildName.MatchString(req.Base) {
		return fmt.Errorf("invalid base %q", req.Base)
	}
	if req.GrowMB < 0 || req.GrowMB > 16384 {
		return fmt.Errorf("grow_mb out of range: %d", req.GrowMB)
	}
	if len(spec.Packages) > 64 {
		return fmt.Errorf("too many packages (%d, max 64)", len(spec.Packages))
	}
	for _, p := range spec.Packages {
		if !reBuildPkg.MatchString(p) {
			return fmt.Errorf("invalid package name %q", p)
		}
	}
	for _, kv := range spec.Env {
		if !reBuildEnv.MatchString(kv) {
			return fmt.Errorf("invalid env entry %q: use KEY=value, one line", kv)
		}
	}
	return nil
}

func builderBase(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		return err
	}
	var req api.BuildImageRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return fmt.Errorf("request.json: %w", err)
	}
	var spec BaseSpec
	if len(req.Spec) > 0 {
		if err := json.Unmarshal(req.Spec, &spec); err != nil {
			return fmt.Errorf("spec: %w", err)
		}
	}
	if err := validateBase(req, spec); err != nil {
		return err
	}
	lib := envOr("KLING_LIB_DIR", "/usr/local/lib/kindling")
	agent := envOr("KLING_GUEST_AGENT", filepath.Join(lib, "kling-guest"))
	script := envOr("KLING_BASE_IMAGE_SCRIPT", filepath.Join(lib, "81-base-image.sh"))
	for _, p := range []string{agent, script} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("missing %s (installed by `make deploy`)", p)
		}
	}
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, []byte(strings.Join(spec.Env, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	base := req.Base
	if base == "" {
		base = "min"
	}
	cmd := exec.Command("bash", script)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(),
		"NAME="+req.Name, "BASE="+base, fmt.Sprintf("GROW=%d", req.GrowMB),
		"PKGS="+strings.Join(spec.Packages, " "), "AGENT="+agent, "ENV_FILE="+envFile)
	return cmd.Run()
}
