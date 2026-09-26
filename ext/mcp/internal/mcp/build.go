package mcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
)

// EMPAQUETAR UN SERVIDOR MCP COMO IMAGEN.
//
// El daemon de kindling no sabe empaquetar servidores MCP: ejecuta, como root,
// el constructor que se le pida. El de kindling-mcp se llama "mcp" y es
// `kling-mcp builder mcp` (instalado en /usr/local/lib/kindling/builders/mcp).
// Recibe la petición con el spec de abajo, la valida aquí —es lo único que
// separa el socket del daemon de un `apk add` en un chroot de root— y llama a
// scripts/80-mcp-image.sh.

// Builder es el nombre del constructor de kindling-mcp en el daemon.
const Builder = "mcp"

// BuildRequest es lo que se empaqueta: los campos que tenía la petición de
// imagen del daemon hasta v0.5.
type BuildRequest struct {
	Name     string   `json:"-"`
	Base     string   `json:"-"`
	GrowMB   int      `json:"-"`
	Packages []string `json:"packages,omitempty"` // apk (o apt en la base glibc)
	NPM      []string `json:"npm,omitempty"`
	PIP      []string `json:"pip,omitempty"`
	Env      []string `json:"env,omitempty"` // KEY=value horneadas en el entrypoint
	Cmd      []string `json:"cmd"`           // lo que arranca el servidor MCP
	Bundle   bool     `json:"bundle,omitempty"`
}

// API convierte la petición en la del daemon, con el constructor "mcp".
func (r BuildRequest) API() api.BuildImageRequest {
	spec, _ := json.Marshal(r)
	return api.BuildImageRequest{Name: r.Name, Base: r.Base, GrowMB: r.GrowMB, Builder: Builder, Spec: spec}
}

// FromAPI reconstruye la petición a partir de la que recibe el constructor.
func FromAPI(req api.BuildImageRequest) (BuildRequest, error) {
	var r BuildRequest
	if len(req.Spec) > 0 {
		if err := json.Unmarshal(req.Spec, &r); err != nil {
			return r, fmt.Errorf("spec: %w", err)
		}
	}
	r.Name, r.Base, r.GrowMB = req.Name, req.Base, req.GrowMB
	return r, nil
}

var (
	// Nombre de imagen: es un componente de ruta y un nombre de servicio.
	reName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	// Paquete de apk.
	reAPK = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]*$`)
	// Los nombres de PyPI admiten punto, guion y guion bajo, y el especificador
	// de versión va con ==, >= o ~=. Nada de eso puede empezar por guion:
	// acabaría en `pip install $PIP` sin comillas, donde un valor que empieza por
	// guion es un flag.
	rePIP = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*((==|>=|<=|~=|!=)[a-zA-Z0-9._*+-]+)?$`)
	// Paquete de npm, con ámbito y versión opcionales.
	reNPM = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*(@[a-zA-Z0-9._~+-]+)?$`)
	// Variable de entorno horneada: KEY=value. El valor, cualquier cosa MENOS
	// saltos de línea y NUL: el script la escribe en el entrypoint entrecomillada,
	// así que el resto viaja inerte, pero un salto de línea partiría otras cosas.
	reEnv = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[^\x00\r\n]*$`)
)

// ValidateBuild comprueba la petición antes de que nada llegue a un shell.
func ValidateBuild(r BuildRequest) error {
	if !reName.MatchString(r.Name) {
		return fmt.Errorf("invalid name %q: lowercase, digits, hyphen and underscore, up to 64", r.Name)
	}
	if r.Base != "" && !reName.MatchString(r.Base) {
		return fmt.Errorf("invalid base image %q", r.Base)
	}
	for _, p := range r.Packages {
		if !reAPK.MatchString(p) {
			return fmt.Errorf("invalid apk package %q", p)
		}
	}
	for _, p := range r.NPM {
		if !reNPM.MatchString(p) {
			return fmt.Errorf("invalid npm package %q", p)
		}
	}
	for _, p := range r.PIP {
		if !rePIP.MatchString(p) {
			return fmt.Errorf("invalid pip package %q", p)
		}
	}
	for _, e := range r.Env {
		if !reEnv.MatchString(e) {
			return fmt.Errorf("invalid environment variable %q: expected KEY=value", e)
		}
	}
	if len(r.Cmd) == 0 {
		return fmt.Errorf("missing the command that starts the MCP server")
	}
	for _, a := range r.Cmd {
		if strings.ContainsAny(a, "\x00\n\r") {
			return fmt.Errorf("the command can't contain newlines or null bytes")
		}
	}
	if r.GrowMB < 0 || r.GrowMB > 8192 {
		return fmt.Errorf("growth out of range: %d MB", r.GrowMB)
	}
	return nil
}

// BuildScriptArgs traduce la petición a los argumentos de 80-mcp-image.sh.
func BuildScriptArgs(req BuildRequest) []string {
	args := []string{"stdio", req.Name}
	if len(req.Packages) > 0 {
		args = append(args, "-p", strings.Join(req.Packages, " "))
	}
	if len(req.NPM) > 0 {
		args = append(args, "-n", strings.Join(req.NPM, " "))
	}
	if len(req.PIP) > 0 {
		args = append(args, "-P", strings.Join(req.PIP, " "))
	}
	for _, e := range req.Env {
		args = append(args, "-e", e)
	}
	if req.Bundle {
		args = append(args, "-bundle")
	}
	args = append(args, "--")
	return append(args, req.Cmd...)
}
