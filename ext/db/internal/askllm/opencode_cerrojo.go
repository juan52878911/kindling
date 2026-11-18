package askllm

// Cerrojo de opencode: que el modelo no tenga NINGUNA herramienta.
//
// Por defecto el agente de `opencode run` lo tiene todo en allow (bash, edit,
// webfetch, los MCP del usuario...), así que cortar al ver un evento de
// herramienta llega tarde: ese evento sale cuando la herramienta ya corrió.
// Lo que de verdad lo impide es la configuración de opencode:
//
//  1. Un agente propio (agenteAsk) en OPENCODE_CONFIG_CONTENT, la capa de
//     configuración de más precedencia, con "*": deny y todas las
//     herramientas a false. opencode no le pasa al modelo las herramientas
//     denegadas: ni las propias ni las de los MCP.
//  2. Como las capas se fusionan clave a clave, primero se lee la configuración
//     resuelta (`opencode debug config`) y se deniega también cada clave de
//     permiso o herramienta que traiga el usuario, y se apaga cada servidor MCP
//     que tenga, para que ni arranquen.
//  3. Antes de lanzar al modelo se comprueba con `opencode debug agent` cómo
//     queda el agente de verdad: ninguna herramienta activa y tras la última
//     regla "*": deny no hay nada que permita. Esto pilla además las capas que
//     opencode aplique después de la nuestra. Si no, o si este opencode no sabe
//     contestar, se falla cerrado sin llamar al modelo.
//
// El corte por evento de opencode.go se queda como detección: no es la barrera.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// agenteAsk es el agente de opencode que usa kling db ask.
const agenteAsk = "kling-db-ask"

const maxDebugOut = 4 << 20

// errCerrojo envuelve los fallos al cerrar opencode: nunca se reintentan.
var errCerrojo = errors.New("could not confirm that opencode runs without tools; refusing to send the prompt (use -provider anthropic, or update opencode)")

// configResuelta es lo que se lee de `opencode debug config`.
type configResuelta struct {
	Permission json.RawMessage            `json:"permission"`
	Tools      map[string]json.RawMessage `json:"tools"`
	MCP        map[string]json.RawMessage `json:"mcp"`
	Agent      map[string]struct {
		Permission json.RawMessage            `json:"permission"`
		Tools      map[string]json.RawMessage `json:"tools"`
	} `json:"agent"`
}

// agenteResuelto es lo que se lee de `opencode debug agent`.
type agenteResuelto struct {
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	Permission []struct {
		Permission string `json:"permission"`
		Action     string `json:"action"`
		Pattern    string `json:"pattern"`
	} `json:"permission"`
	Tools map[string]bool `json:"tools"`
}

// configCerrada devuelve el contenido de OPENCODE_CONFIG_CONTENT ya
// comprobado con este opencode. Cualquier duda acaba en errCerrojo.
func (o *OpenCode) configCerrada(ctx context.Context) (string, error) {
	base, err := construirConfig(nil)
	if err != nil {
		return "", err
	}
	raw, err := o.debug(ctx, base, "config")
	if err != nil {
		return "", fmt.Errorf("%w: reading opencode's configuration: %v", errCerrojo, err)
	}
	var res configResuelta
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("%w: opencode's configuration is not the expected JSON", errCerrojo)
	}
	final, err := construirConfig(&res)
	if err != nil {
		return "", err
	}
	// Se comprueba la configuración exacta con la que se lanzará el modelo
	// (cuesta un arranque más de opencode, frente a un modelo que tarda decenas
	// de segundos).
	raw, err = o.debug(ctx, final, "agent", agenteAsk)
	if err != nil {
		return "", fmt.Errorf("%w: reading the %s agent: %v", errCerrojo, agenteAsk, err)
	}
	if err := comprobarAgente(raw); err != nil {
		return "", fmt.Errorf("%w: %v", errCerrojo, err)
	}
	return final, nil
}

// construirConfig arma la configuración que lo deniega todo. Con res, deniega
// además cada clave que ya trae la configuración del usuario y apaga sus MCP.
func construirConfig(res *configResuelta) (string, error) {
	perm := map[string]string{"*": "deny"}
	tools := map[string]bool{"*": false}
	mcp := map[string]map[string]bool{}
	if res != nil {
		permUsuario := []json.RawMessage{res.Permission}
		herrUsuario := []map[string]json.RawMessage{res.Tools}
		if a, ok := res.Agent[agenteAsk]; ok {
			permUsuario = append(permUsuario, a.Permission)
			herrUsuario = append(herrUsuario, a.Tools)
		}
		for _, p := range permUsuario {
			var m map[string]json.RawMessage
			// Un permiso que sea un texto suelto ("allow") lo sustituye el mapa.
			if len(p) > 0 && json.Unmarshal(p, &m) == nil {
				for k := range m {
					perm[k] = "deny"
				}
			}
		}
		for _, h := range herrUsuario {
			for k := range h {
				tools[k] = false
			}
		}
		for k := range res.MCP {
			mcp[k] = map[string]bool{"enabled": false}
		}
	}
	cfg := map[string]any{
		"permission": perm,
		"tools":      tools,
		"agent": map[string]any{
			agenteAsk: map[string]any{
				"mode":        "primary",
				"description": "kling db ask: text only, no tools",
				"permission":  perm,
				"tools":       tools,
			},
		},
	}
	if len(mcp) > 0 {
		cfg["mcp"] = mcp
	}
	b, err := json.Marshal(cfg)
	return string(b), err
}

// comprobarAgente acepta el agente solo si no le queda ninguna herramienta.
func comprobarAgente(raw []byte) error {
	var a agenteResuelto
	if err := json.Unmarshal(raw, &a); err != nil {
		return errors.New("the agent description is not the expected JSON")
	}
	if a.Name != agenteAsk {
		return fmt.Errorf("opencode resolved agent %q instead of %q", clip(sinControl(a.Name), 40), agenteAsk)
	}
	// Un subagente no se puede elegir con --agent: opencode usaría otro.
	if a.Mode != "primary" && a.Mode != "all" {
		return fmt.Errorf("the %s agent has mode %q", agenteAsk, clip(sinControl(a.Mode), 20))
	}
	if len(a.Tools) == 0 {
		return errors.New("opencode did not list the agent's tools")
	}
	var activas []string
	for k, v := range a.Tools {
		if v {
			activas = append(activas, clip(sinControl(k), 40))
		}
	}
	if len(activas) > 0 {
		sort.Strings(activas)
		return fmt.Errorf("tools still enabled: %s", strings.Join(activas, ", "))
	}
	// opencode aplica la última regla que case: tras el "*" deny final no
	// puede quedar nada que no sea deny. external_directory no es una
	// herramienta (solo dice qué rutas puede tocar una), así que no cuenta.
	ultimo := -1
	for i, r := range a.Permission {
		if r.Permission == "*" && r.Pattern == "*" && r.Action == "deny" {
			ultimo = i
		}
	}
	if ultimo < 0 {
		return errors.New("the agent has no deny-all rule")
	}
	for _, r := range a.Permission[ultimo+1:] {
		if r.Action != "deny" && r.Permission != "external_directory" {
			return fmt.Errorf("a later rule sets %q to %q", clip(sinControl(r.Permission), 40), clip(sinControl(r.Action), 10))
		}
	}
	return nil
}

// debug ejecuta `opencode debug <args> --pure` con la configuración dada y
// devuelve su JSON.
func (o *OpenCode) debug(ctx context.Context, cfg string, args ...string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "kling-db-ask-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, o.Bin, append(append([]string{"debug"}, args...), "--pure")...)
	cmd.Dir = dir
	cmd.Env = entornoHijo(os.Environ(), cfg)
	setProcessGroup(cmd)
	var out bytes.Buffer
	var stderr limitBuf
	cmd.Stdout = &limitWriter{w: &out, n: maxDebugOut}
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%v%s", err, stderr.hint())
	}
	// Puede haber avisos antes del JSON: se toma el primer objeto.
	b := out.Bytes()
	i := bytes.IndexByte(b, '{')
	if i < 0 {
		return nil, errors.New("no JSON in its output")
	}
	var v json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(b[i:])).Decode(&v); err != nil {
		return nil, errors.New("its output is not valid JSON")
	}
	return v, nil
}

// limitWriter guarda hasta n bytes y descarta el resto.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(l.n, len(p))
		if _, err := l.w.Write(p[:k]); err != nil {
			return 0, err
		}
		l.n -= k
	}
	return len(p), nil
}

// entornoHijo es el entorno de opencode: el del usuario (le hacen falta su
// configuración y las claves de su proveedor) sin lo que no le toca ver —las
// credenciales de kindling y de Postgres— ni lo que podría reabrir los
// permisos, y con la configuración cerrada.
func entornoHijo(env []string, cfg string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "KLING_"), strings.HasPrefix(k, "PG"), k == "DATABASE_URL",
			k == "OPENCODE_CONFIG_CONTENT", k == "OPENCODE_PERMISSION", k == "OPENCODE_DISABLE_PROJECT_CONFIG":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "OPENCODE_CONFIG_CONTENT="+cfg, "OPENCODE_DISABLE_PROJECT_CONFIG=1")
}
