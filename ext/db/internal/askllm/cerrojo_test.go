package askllm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Un opencode falso escrito en Go (el propio binario de test, relanzado con
// FAKE_OC_GO=1) que imita lo que importa del de verdad, comprobado a mano con
// opencode 1.18.34:
//   - la configuración del usuario (FAKE_OC_USERCFG) y la de
//     OPENCODE_CONFIG_CONTENT se fusionan clave a clave, ganando la segunda;
//   - `run --agent X` usa X si existe y es primary; si no, el agente build;
//   - si al agente le queda bash permitido, el modelo lo usa y la herramienta
//     SE EJECUTA (deja FAKE_OC_SENTINEL) antes de emitir su evento;
//   - cada MCP que no esté a enabled:false se arranca (lo apunta).
//
// FAKE_OC_FORCE_ALLOW=1 imita una capa que se aplica después de la nuestra
// (una configuración gestionada, por ejemplo) y reabre bash.

func TestMain(m *testing.M) {
	if os.Getenv("FAKE_OC_GO") == "1" {
		os.Exit(fakeOpencode(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fusionar(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		va, oka := out[k].(map[string]any)
		vb, okb := v.(map[string]any)
		if oka && okb {
			out[k] = fusionar(va, vb)
		} else {
			out[k] = v
		}
	}
	return out
}

func leerJSON(s string) map[string]any {
	m := map[string]any{}
	if s != "" {
		_ = json.Unmarshal([]byte(s), &m)
	}
	return m
}

func sub(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	if v == nil {
		v = map[string]any{}
	}
	return v
}

// bashPermitido: el permiso de bash del agente o, si no lo fija, el general;
// sin nada, allow (lo que trae opencode). Solo un "deny" literal lo cierra.
func bashPermitido(cfg map[string]any, agente string) bool {
	ap := sub(sub(sub(cfg, "agent"), agente), "permission")
	tp := sub(cfg, "permission")
	for _, p := range []map[string]any{ap, tp} {
		for _, k := range []string{"bash", "*"} {
			if v, ok := p[k]; ok {
				return v != "deny"
			}
		}
	}
	return true
}

func fakeOpencode(args []string) int {
	cfg := fusionar(leerJSON(os.Getenv("FAKE_OC_USERCFG")), leerJSON(os.Getenv("OPENCODE_CONFIG_CONTENT")))
	if os.Getenv("FAKE_OC_FORCE_ALLOW") == "1" {
		cfg = fusionar(cfg, map[string]any{"permission": map[string]any{"bash": "allow"},
			"agent": map[string]any{agenteAsk: map[string]any{"permission": map[string]any{"bash": "allow"}}}})
	}
	if len(args) >= 2 && args[0] == "debug" {
		if os.Getenv("FAKE_OC_NO_DEBUG") == "1" {
			fmt.Fprintln(os.Stderr, "unknown command")
			return 1
		}
		switch args[1] {
		case "config":
			_ = json.NewEncoder(os.Stdout).Encode(cfg)
		case "agent":
			a, ok := sub(cfg, "agent")[args[2]].(map[string]any)
			if !ok {
				fmt.Fprintln(os.Stderr, "agent not found")
				return 1
			}
			mode, _ := a["mode"].(string)
			permitido := bashPermitido(cfg, args[2])
			reglas := []map[string]string{{"permission": "*", "action": "allow", "pattern": "*"}}
			if !permitido {
				reglas = append(reglas, map[string]string{"permission": "*", "action": "deny", "pattern": "*"})
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"name": args[2], "mode": mode,
				"permission": reglas, "tools": map[string]bool{"bash": permitido, "read": permitido}})
		}
		return 0
	}
	// run
	if f := os.Getenv("FAKE_OC_RUNLOG"); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(args, " ")+"\nKLING_TOKEN="+os.Getenv("KLING_TOKEN")+"\n"), 0o600)
	}
	agente := "build"
	for i, a := range args {
		if a == "--agent" && i+1 < len(args) {
			if ac, ok := sub(cfg, "agent")[args[i+1]].(map[string]any); ok && ac["mode"] == "primary" {
				agente = args[i+1]
			}
		}
	}
	var arrancados []string
	for name, v := range sub(cfg, "mcp") {
		if m, _ := v.(map[string]any); m["enabled"] != false {
			arrancados = append(arrancados, name)
		}
	}
	if f := os.Getenv("FAKE_OC_MCPLOG"); f != "" && len(arrancados) > 0 {
		_ = os.WriteFile(f, []byte(strings.Join(arrancados, " ")), 0o600)
	}
	fmt.Println(`{"type":"step_start","part":{"type":"step-start"}}`)
	if bashPermitido(cfg, agente) {
		_ = os.WriteFile(os.Getenv("FAKE_OC_SENTINEL"), []byte("ran"), 0o600)
		fmt.Println(`{"type":"tool_use","part":{"type":"tool","tool":"bash","state":{"status":"completed"}}}`)
	}
	fmt.Println(`{"type":"text","part":{"type":"text","text":"SELECT 1"}}`)
	fmt.Println(`{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}`)
	return 0
}

// goFakeOC prepara el opencode falso en Go con la configuración de usuario dada.
func goFakeOC(t *testing.T, userCfg string) (o *OpenCode, sentinel, runlog, mcplog string) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	sentinel, runlog, mcplog = filepath.Join(d, "sentinel"), filepath.Join(d, "run"), filepath.Join(d, "mcp")
	t.Setenv("FAKE_OC_GO", "1")
	t.Setenv("FAKE_OC_USERCFG", userCfg)
	t.Setenv("FAKE_OC_SENTINEL", sentinel)
	t.Setenv("FAKE_OC_RUNLOG", runlog)
	t.Setenv("FAKE_OC_MCPLOG", mcplog)
	return NewOpenCode(bin, "", 20*time.Second), sentinel, runlog, mcplog
}

// La configuración del usuario lo permite todo, también en un agente con
// nuestro nombre, y trae un MCP: aun así no se ejecuta ninguna herramienta ni
// arranca el MCP, y no se le pasan las credenciales de kindling.
func TestOpenCodeNoEjecutaHerramientas(t *testing.T) {
	user := `{"permission":{"*":"allow","bash":{"*":"allow"}},"tools":{"bash":true},
	  "agent":{"build":{"permission":{"bash":"allow"}},"kling-db-ask":{"mode":"subagent","permission":{"bash":"allow"}}},
	  "mcp":{"notas":{"type":"local","command":["x"]}}}`
	o, sentinel, runlog, mcplog := goFakeOC(t, user)
	t.Setenv("KLING_TOKEN", "secreto-de-prueba")
	t.Setenv("OPENCODE_PERMISSION", `{"bash":"allow"}`)
	text, err := o.Complete(context.Background(), "sys", "q")
	// Lo primero: cortar al ver el evento no basta, la herramienta ya corrió.
	if _, serr := os.Stat(sentinel); serr == nil {
		t.Fatalf("a tool ran: the sentinel exists (Complete: %v)", err)
	}
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if text != "SELECT 1" {
		t.Fatalf("text = %q", text)
	}
	if b, err := os.ReadFile(mcplog); err == nil {
		t.Fatalf("MCP servers started: %s", b)
	}
	log, err := os.ReadFile(runlog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "--agent "+agenteAsk) {
		t.Fatalf("run without the locked agent: %s", log)
	}
	if strings.Contains(string(log), "secreto-de-prueba") {
		t.Fatal("KLING_TOKEN reached opencode")
	}
}

// Si algo reabre una herramienta después de nuestra capa, o el opencode no
// sabe describir su configuración, no se llama al modelo.
func TestOpenCodeFallaCerrado(t *testing.T) {
	for _, env := range []string{"FAKE_OC_FORCE_ALLOW", "FAKE_OC_NO_DEBUG"} {
		t.Run(env, func(t *testing.T) {
			o, sentinel, runlog, _ := goFakeOC(t, `{"permission":{"*":"allow"}}`)
			t.Setenv(env, "1")
			_, err := o.Complete(context.Background(), "sys", "q")
			if err == nil || !strings.Contains(err.Error(), "without tools") {
				t.Fatalf("err = %v", err)
			}
			if _, err := os.Stat(runlog); err == nil {
				t.Fatal("opencode run was started")
			}
			if _, err := os.Stat(sentinel); err == nil {
				t.Fatal("a tool ran")
			}
		})
	}
}

func TestComprobarAgente(t *testing.T) {
	ok := `{"name":"kling-db-ask","mode":"primary","tools":{"bash":false},"permission":[` +
		`{"permission":"*","action":"allow","pattern":"*"},{"permission":"*","action":"deny","pattern":"*"},` +
		`{"permission":"external_directory","action":"allow","pattern":"/x/*"}]}`
	if err := comprobarAgente([]byte(ok)); err != nil {
		t.Fatalf("valid agent rejected: %v", err)
	}
	malos := map[string]string{
		"herramienta": strings.Replace(ok, `"bash":false`, `"bash":true`, 1),
		"sin tools":   strings.Replace(ok, `{"bash":false}`, `{}`, 1),
		"subagente":   strings.Replace(ok, `"primary"`, `"subagent"`, 1),
		"otro nombre": strings.Replace(ok, `"kling-db-ask"`, `"build"`, 1),
		"sin deny":    strings.Replace(ok, `"action":"deny"`, `"action":"allow"`, 1),
		"allow tras":  strings.Replace(ok, `"permission":"external_directory"`, `"permission":"bash"`, 1),
		"no json":     `nope`,
	}
	for name, s := range malos {
		if err := comprobarAgente([]byte(s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEntornoHijo(t *testing.T) {
	env := entornoHijo([]string{"HOME=/h", "KLING_TOKEN=t", "PGPASSWORD=p", "DATABASE_URL=d",
		"OPENCODE_PERMISSION={}", "OPENCODE_CONFIG_CONTENT=viejo", "MINIMAX_API_KEY=k"}, "{}")
	got := strings.Join(env, " ")
	for _, no := range []string{"KLING_TOKEN", "PGPASSWORD", "DATABASE_URL", "OPENCODE_PERMISSION", "viejo"} {
		if strings.Contains(got, no) {
			t.Errorf("%s reached the child: %s", no, got)
		}
	}
	for _, si := range []string{"HOME=/h", "MINIMAX_API_KEY=k", "OPENCODE_CONFIG_CONTENT={}"} {
		if !strings.Contains(got, si) {
			t.Errorf("%s missing: %s", si, got)
		}
	}
}
