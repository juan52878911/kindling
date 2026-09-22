package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/juan52878911/kindling-mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
)

// cmdBuilder es el constructor de imágenes MCP que ejecuta el daemon de
// kindling, como root, a través de /usr/local/lib/kindling/builders/mcp:
//
//	kling-mcp builder mcp <directorio de trabajo>
//
// No es un comando para teclear (no está en el manifiesto). Lee request.json,
// lo valida y llama a 80-mcp-image.sh. Ver docs/api.md de kindling.
func cmdBuilder(args []string) error {
	if len(args) != 2 || args[0] != mcp.Builder {
		return fmt.Errorf("usage: kling-mcp builder mcp <workdir>  (the kindling daemon runs it)")
	}
	b, err := os.ReadFile(filepath.Join(args[1], "request.json"))
	if err != nil {
		return err
	}
	var req api.BuildImageRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return fmt.Errorf("request.json: %w", err)
	}
	r, err := mcp.FromAPI(req)
	if err != nil {
		return err
	}
	if err := mcp.ValidateBuild(r); err != nil {
		return err
	}
	script, err := imageScript()
	if err != nil {
		return err
	}
	cmd := exec.Command("bash", append([]string{script}, mcp.BuildScriptArgs(r)...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if r.Base != "" {
		cmd.Env = append(cmd.Env, "BASE_IMAGE="+r.Base)
	}
	if r.GrowMB > 0 {
		cmd.Env = append(cmd.Env, fmt.Sprintf("GROW=%d", r.GrowMB))
	}
	if b := bridgeBinary(); b != "" {
		cmd.Env = append(cmd.Env, "BRIDGE="+b)
	}
	return cmd.Run()
}

// imageScript localiza 80-mcp-image.sh: donde lo deja `make deploy`, o donde
// diga KLING_IMAGE_SCRIPT.
func imageScript() (string, error) {
	for _, p := range []string{
		os.Getenv("KLING_IMAGE_SCRIPT"),
		"/usr/local/lib/kindling/80-mcp-image.sh",
		"/usr/lib/kindling/80-mcp-image.sh",
	} {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("can't find 80-mcp-image.sh; deploy kindling-mcp with `make deploy` " +
		"or point to it with KLING_IMAGE_SCRIPT")
}

// bridgeBinary es el kling-bridge que se mete en las imágenes.
func bridgeBinary() string {
	for _, p := range []string{
		os.Getenv("KLING_BRIDGE"),
		"/usr/local/lib/kindling/kling-bridge",
	} {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}
