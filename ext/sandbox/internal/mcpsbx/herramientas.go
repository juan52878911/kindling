package mcpsbx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// API es lo que el servidor necesita del cliente HTTP del gateway: hacer una
// petición ya autenticada. cuerpo y tipo pueden ser nulos.
type API interface {
	Do(ctx context.Context, metodo, ruta string, cuerpo io.Reader, tipo string) (*http.Response, error)
}

// Topes de lo que vuelve al agente: un fichero o una salida enorme no deben
// reventar su contexto.
const (
	MaxSalida  = 64 << 10 // bytes por flujo (stdout, stderr) y por fichero leído
	maxCuerpo  = 16 << 20 // lo máximo que se lee del gateway
	maxEscribe = 8 << 20  // fichero a escribir
)

type herramienta struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func esquema(req []string, props map[string]any) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(req) > 0 {
		m["required"] = req
	}
	return m
}

var (
	pStr   = func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	pInt   = func(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }
	pID    = pStr("sandbox id, as returned by sandbox_create (host/machine)")
	pLabel = map[string]any{"type": "object", "description": "labels", "additionalProperties": map[string]any{"type": "string"}}
)

var herramientas = []herramienta{
	{"sandbox_create", "Create a sandbox from a template. Returns its JSON (id, host, state, expires_at).",
		esquema(nil, map[string]any{"template": pStr("template name"), "ttl_seconds": pInt("idle time before it is frozen"), "labels": pLabel})},
	{"sandbox_list", "List your sandboxes.", esquema(nil, map[string]any{})},
	{"sandbox_exec", "Run a command inside a sandbox and wait for it. Returns exit code, stdout and stderr (each capped; a notice says when truncated).",
		esquema([]string{"id", "command"}, map[string]any{"id": pID,
			"command":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "argv, e.g. [\"ls\",\"-l\"]"},
			"cwd":             pStr("working directory"),
			"env":             map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "extra environment"},
			"timeout_seconds": pInt("kill the command after this long")})},
	{"sandbox_exec_background", "Start a command in the background inside a sandbox and return at once with its pid. It keeps running after this call; read its output with sandbox_process_output.",
		esquema([]string{"id", "command"}, map[string]any{"id": pID,
			"command":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "argv, e.g. [\"python3\",\"-m\",\"http.server\"]"},
			"cwd":             pStr("working directory"),
			"env":             map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "extra environment"},
			"timeout_seconds": pInt("kill the process after this long (default: runs until it ends or is deleted)")})},
	{"sandbox_process_output", "Read the output of a background process from an offset, and its state (running or exited with its exit code). Returns next_offset: pass it back to read only what is new. Optionally wait for it to finish first.",
		esquema([]string{"id", "pid"}, map[string]any{"id": pID, "pid": pInt("pid returned by sandbox_exec_background"),
			"offset":       pInt("byte offset to read from (default 0 = everything still kept)"),
			"wait_seconds": pInt("wait up to this long (max 60) for the process to end before reading")})},
	{"sandbox_read_file", "Read a text file from a sandbox (capped).",
		esquema([]string{"id", "path"}, map[string]any{"id": pID, "path": pStr("absolute path inside the sandbox")})},
	{"sandbox_write_file", "Write a file inside a sandbox (overwrites).",
		esquema([]string{"id", "path", "content"}, map[string]any{"id": pID, "path": pStr("absolute path"), "content": pStr("file content"), "mode": pStr("octal permissions, e.g. 0644")})},
	{"sandbox_delete", "Delete a sandbox.", esquema([]string{"id"}, map[string]any{"id": pID})},
	{"sandbox_renew", "Extend a sandbox's lifetime.",
		esquema([]string{"id"}, map[string]any{"id": pID, "ttl_seconds": pInt("new idle time; default keeps the current one")})},
}

type manejador func(ctx context.Context, api API, args json.RawMessage) (string, error)

var manejadores = map[string]manejador{
	"sandbox_create": func(ctx context.Context, a API, raw json.RawMessage) (string, error) {
		var in struct {
			Template   string            `json:"template"`
			TTLSeconds int               `json:"ttl_seconds"`
			Labels     map[string]string `json:"labels"`
		}
		if err := args(raw, &in); err != nil {
			return "", err
		}
		body := map[string]any{}
		if in.Template != "" {
			body["template"] = in.Template
		}
		if in.TTLSeconds > 0 {
			body["ttl_seconds"] = in.TTLSeconds
		}
		if len(in.Labels) > 0 {
			body["labels"] = in.Labels
		}
		b, _, err := llamarJSON(ctx, a, "POST", "/v1/sandboxes", body)
		return string(b), err
	},
	"sandbox_list": func(ctx context.Context, a API, raw json.RawMessage) (string, error) {
		b, _, err := llamarJSON(ctx, a, "GET", "/v1/sandboxes", nil)
		return string(b), err
	},
	"sandbox_exec": func(ctx context.Context, a API, raw json.RawMessage) (string, error) {
		var in struct {
			ID      string            `json:"id"`
			Command []string          `json:"command"`
			Cwd     string            `json:"cwd"`
			Env     map[string]string `json:"env"`
			Timeout int               `json:"timeout_seconds"`
		}
		if err := args(raw, &in); err != nil {
			return "", err
		}
		if err := validarID(in.ID); err != nil {
			return "", err
		}
		if len(in.Command) == 0 {
			return "", errors.New("command is required")
		}
		body := map[string]any{"cmd": in.Command}
		if in.Cwd != "" {
			body["dir"] = in.Cwd
		}
		if len(in.Env) > 0 {
			var env []string
			for k, v := range in.Env {
				env = append(env, k+"="+v)
			}
			body["env"] = env
		}
		if in.Timeout > 0 {
			body["timeout_seconds"] = in.Timeout
		}
		b, _, err := llamarJSON(ctx, a, "POST", "/v1/sandboxes/"+in.ID+"/exec?wait=1", body)
		if err != nil {
			return "", err
		}
		var r struct {
			ExitCode   int    `json:"exit_code"`
			Stdout     []byte `json:"stdout"`
			Stderr     []byte `json:"stderr"`
			DurationMS int64  `json:"duration_ms"`
			Truncated  bool   `json:"truncated"`
			TimedOut   bool   `json:"timed_out"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return "", fmt.Errorf("unreadable exec result: %w", err)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "exit: %d\n", r.ExitCode)
		if r.TimedOut {
			sb.WriteString("timed out\n")
		}
		if r.Truncated {
			sb.WriteString("notice: the sandbox already truncated the output\n")
		}
		sb.WriteString("--- stdout ---\n" + tope(r.Stdout, "stdout") + "\n--- stderr ---\n" + tope(r.Stderr, "stderr"))
		return sb.String(), nil
	},
	"sandbox_exec_background": func(ctx context.Context, a API, raw json.RawMessage) (string, error) {
		var in struct {
			ID      string            `json:"id"`
			Command []string          `json:"command"`
			Cwd     string            `json:"cwd"`
			Env     map[string]string `json:"env"`
			Timeout int               `json:"timeout_seconds"`
		}
		if err := args(raw, &in); err != nil {
			return "", err
		}
		if err := validarID(in.ID); err != nil {
			return "", err
		}
		if len(in.Command) == 0 {
			return "", errors.New("command is required")
		}
		body := map[string]any{"cmd": in.Command}
		if in.Cwd != "" {
			body["dir"] = in.Cwd
		}
		if len(in.Env) > 0 {
			var env []string
			for k, v := range in.Env {
				env = append(env, k+"="+v)
			}
			body["env"] = env
		}
		if in.Timeout > 0 {
			body["timeout_seconds"] = in.Timeout
		}
		b, _, err := llamarJSON(ctx, a, "POST", "/v1/sandboxes/"+in.ID+"/processes", body)
		return string(b), err
	},
	"sandbox_process_output": func(ctx context.Context, a API, raw json.RawMessage) (string, error) {
		var in struct {
			ID          string `json:"id"`
			PID         int    `json:"pid"`
			Offset      int64  `json:"offset"`
			WaitSeconds int    `json:"wait_seconds"`
		}
		if err := args(raw, &in); err != nil {
			return "", err
		}
		if err := validarID(in.ID); err != nil {
			return "", err
		}
		if in.PID <= 0 {
			return "", errors.New("pid is required")
		}
		if in.Offset < 0 {
			return "", errors.New("offset can't be negative")
		}
		base := "/v1/sandboxes/" + in.ID + "/processes/" + strconv.Itoa(in.PID)
		if in.WaitSeconds > 0 {
			// El gateway devuelve el estado cuando acaba o cuando vence el plazo.
			if _, _, err := llamarJSON(ctx, a, "GET", base+"?wait="+strconv.Itoa(min(in.WaitSeconds, maxEsperaProc)), nil); err != nil {
				return "", err
			}
		}
		b, _, err := llamarJSON(ctx, a, "GET", base+"/output?offset="+strconv.FormatInt(in.Offset, 10), nil)
		if err != nil {
			return "", err
		}
		var stdout, stderr []byte
		next, dropped := in.Offset, int64(0)
		for _, l := range bytes.Split(b, []byte("\n")) {
			if len(bytes.TrimSpace(l)) == 0 {
				continue
			}
			var ev struct {
				Stream  string `json:"stream"`
				Data    []byte `json:"data"`
				Offset  int64  `json:"offset"`
				Dropped int64  `json:"dropped"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(l, &ev); err != nil {
				return "", fmt.Errorf("unreadable process output: %w", err)
			}
			if ev.Error != "" {
				return "", errors.New(ev.Error)
			}
			next = max(next, ev.Offset)
			dropped += ev.Dropped
			// El cuerpo ya viene acotado por maxCuerpo; tope() recorta y avisa.
			switch ev.Stream {
			case "stdout":
				stdout = append(stdout, ev.Data...)
			case "stderr":
				stderr = append(stderr, ev.Data...)
			}
		}
		// El estado va DESPUÉS de la salida: si acabó entre medias, next_offset
		// no pasa de lo leído y la siguiente llamada trae el resto.
		sb, _, err := llamarJSON(ctx, a, "GET", base, nil)
		if err != nil {
			return "", err
		}
		var st struct {
			State    string `json:"state"`
			ExitCode *int   `json:"exit_code"`
			Signal   string `json:"signal"`
			TimedOut bool   `json:"timed_out"`
		}
		if err := json.Unmarshal(sb, &st); err != nil {
			return "", fmt.Errorf("unreadable process state: %w", err)
		}
		var o strings.Builder
		fmt.Fprintf(&o, "state: %s\n", st.State)
		if st.ExitCode != nil {
			fmt.Fprintf(&o, "exit: %d\n", *st.ExitCode)
		}
		if st.Signal != "" {
			fmt.Fprintf(&o, "signal: %s\n", st.Signal)
		}
		if st.TimedOut {
			o.WriteString("timed out\n")
		}
		if dropped > 0 {
			fmt.Fprintf(&o, "notice: %d bytes at the start were already discarded by the sandbox\n", dropped)
		}
		fmt.Fprintf(&o, "next_offset: %d\n", next)
		o.WriteString("--- stdout ---\n" + tope(stdout, "stdout") + "\n--- stderr ---\n" + tope(stderr, "stderr"))
		return o.String(), nil
	},
	"sandbox_read_file": func(ctx context.Context, a API, raw json.RawMessage) (string, error) {
		var in struct{ ID, Path string }
		if err := args(raw, &in); err != nil {
			return "", err
		}
		if err := validarID(in.ID); err != nil {
			return "", err
		}
		if in.Path == "" {
			return "", errors.New("path is required")
		}
		b, _, err := pedir(ctx, a, "GET", "/v1/sandboxes/"+in.ID+"/files?path="+url.QueryEscape(in.Path), nil, "")
		if err != nil {
			return "", err
		}
		return tope(b, "file"), nil
	},
	"sandbox_write_file": func(ctx context.Context, a API, raw json.RawMessage) (string, error) {
		var in struct{ ID, Path, Content, Mode string }
		if err := args(raw, &in); err != nil {
			return "", err
		}
		if err := validarID(in.ID); err != nil {
			return "", err
		}
		if in.Path == "" {
			return "", errors.New("path is required")
		}
		if len(in.Content) > maxEscribe {
			return "", fmt.Errorf("content is %d bytes; the limit is %d", len(in.Content), maxEscribe)
		}
		q := "?path=" + url.QueryEscape(in.Path)
		if in.Mode != "" {
			q += "&mode=" + url.QueryEscape(in.Mode)
		}
		if _, _, err := pedir(ctx, a, "PUT", "/v1/sandboxes/"+in.ID+"/files"+q, strings.NewReader(in.Content), "application/octet-stream"); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), nil
	},
	"sandbox_delete": func(ctx context.Context, a API, raw json.RawMessage) (string, error) {
		var in struct{ ID string }
		if err := args(raw, &in); err != nil {
			return "", err
		}
		if err := validarID(in.ID); err != nil {
			return "", err
		}
		if _, _, err := pedir(ctx, a, "DELETE", "/v1/sandboxes/"+in.ID, nil, ""); err != nil {
			return "", err
		}
		return "deleted " + in.ID, nil
	},
	"sandbox_renew": func(ctx context.Context, a API, raw json.RawMessage) (string, error) {
		var in struct {
			ID         string `json:"id"`
			TTLSeconds int    `json:"ttl_seconds"`
		}
		if err := args(raw, &in); err != nil {
			return "", err
		}
		if err := validarID(in.ID); err != nil {
			return "", err
		}
		body := map[string]any{}
		if in.TTLSeconds > 0 {
			body["ttl_seconds"] = in.TTLSeconds
		}
		b, _, err := llamarJSON(ctx, a, "POST", "/v1/sandboxes/"+in.ID+"/renew", body)
		return string(b), err
	},
}

func args(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	// Los nombres de campo son case-insensitive en encoding/json: id/ID valen.
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// validarID: el id del frontal es host/máquina; nada de segmentos raros que
// cambien la ruta.
func validarID(id string) error {
	if id == "" {
		return errors.New("id is required")
	}
	if strings.Count(id, "/") != 1 || strings.ContainsAny(id, "?#\\ ") || strings.Contains(id, "..") {
		return fmt.Errorf("invalid sandbox id %q (expected host/machine)", id)
	}
	return nil
}

// maxEsperaProc es lo más que sandbox_process_output espera a que acabe.
const maxEsperaProc = 60

func tope(b []byte, que string) string {
	if len(b) <= MaxSalida {
		return string(b)
	}
	return string(b[:MaxSalida]) + fmt.Sprintf("\n[truncated: %s is %d bytes, showing the first %d]", que, len(b), MaxSalida)
}

func llamarJSON(ctx context.Context, a API, metodo, ruta string, body any) ([]byte, int, error) {
	var r io.Reader
	tipo := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		r, tipo = bytes.NewReader(b), "application/json"
	}
	return pedir(ctx, a, metodo, ruta, r, tipo)
}

// pedir hace la petición; un estado >= 300 es un error con el mensaje del
// gateway y su código (y Retry-After si lo hay).
func pedir(ctx context.Context, a API, metodo, ruta string, cuerpo io.Reader, tipo string) ([]byte, int, error) {
	resp, err := a.Do(ctx, metodo, ruta, cuerpo, tipo)
	if err != nil {
		return nil, 0, fmt.Errorf("gateway unreachable: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxCuerpo))
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		if e.Message == "" {
			e.Message = resp.Status
		}
		if len(e.Message) > 2048 {
			e.Message = e.Message[:2048] + "…"
		}
		s := fmt.Sprintf("%s (HTTP %d", e.Message, resp.StatusCode)
		if ra, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && ra > 0 {
			s += fmt.Sprintf(", retry after %ds", ra)
		}
		return nil, resp.StatusCode, errors.New(s + ")")
	}
	return b, resp.StatusCode, nil
}
