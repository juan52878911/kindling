package auditoria

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/juan52878911/kindling/ext/sandbox/internal/telemetria"
)

// Topes del argv que se guarda: lo bastante para saber QUÉ se ejecutó, no
// para llevarse un script entero ni un secreto largo.
const (
	maxArgs     = 32
	maxArgBytes = 256
)

// Observador devuelve la función que se registra en telemetria.Config para
// auditar cada acción con efecto de un inquilino identificado.
func Observador(r *Registro) func(*telemetria.Evento) {
	return func(ev *telemetria.Evento) {
		if ev.Ruta.Accion == "" || ev.Tenant == "" {
			return
		}
		l := Linea{
			Ts: ev.Inicio.UTC(), Tenant: ev.Tenant, Accion: ev.Ruta.Accion, Sandbox: ev.Ruta.Sandbox,
			Resultado: resultado(ev.Estado), Estado: ev.Estado, DuracionMs: ev.Duracion.Milliseconds(),
			Traza: ev.Traza.TraceID, Detalle: detalle(ev),
		}
		if ev.Ruta.Accion == "sandbox.create" {
			if ids := idsCreados(ev.Respuesta); len(ids) == 1 {
				l.Sandbox = ids[0]
			} else if len(ids) > 1 {
				if l.Detalle == nil {
					l.Detalle = map[string]any{}
				}
				l.Detalle["sandboxes"] = ids
			}
		}
		r.Escribir(l)
	}
}

func resultado(estado int) string {
	switch {
	case estado >= 200 && estado < 400:
		return "ok"
	case estado == http.StatusUnauthorized || estado == http.StatusForbidden || estado == http.StatusTooManyRequests:
		return "denied"
	}
	return "error"
}

func idsCreados(b []byte) []string {
	var v struct {
		ID        string `json:"id"`
		Sandboxes []struct {
			ID string `json:"id"`
		} `json:"sandboxes"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	if v.ID != "" {
		return []string{v.ID}
	}
	var out []string
	for i, s := range v.Sandboxes {
		if i == 32 {
			break
		}
		out = append(out, s.ID)
	}
	return out
}

func detalle(ev *telemetria.Evento) map[string]any {
	d := map[string]any{}
	p := ev.Ruta.Param
	if ev.CuerpoCortado {
		d["body_truncated"] = true
	}
	switch ev.Ruta.Accion {
	case "exec", "process.start":
		var b struct {
			Cmd     []string `json:"cmd"`
			Dir     string   `json:"dir"`
			Env     []string `json:"env"`
			User    string   `json:"user"`
			Session string   `json:"session"`
		}
		if json.Unmarshal(ev.Cuerpo, &b) == nil {
			d["argv"], d["argc"] = Argv(b.Cmd), len(b.Cmd)
			if b.Dir != "" {
				d["dir"] = cortar(b.Dir)
			}
			if names := NombresEnv(b.Env); len(names) > 0 {
				d["env_names"] = names
			}
			if b.User != "" {
				d["user"] = cortar(b.User)
			}
			if b.Session != "" {
				d["session"] = cortar(b.Session)
			}
		}
	case "file.write":
		d["path"] = cortar(ev.Query.Get("path"))
		d["bytes"] = ev.BytesEntrada
	case "file.delete":
		d["path"] = cortar(ev.Query.Get("path"))
	case "process.kill":
		d["pid"] = p["pid"]
	case "process.signal":
		d["pid"] = p["pid"]
		var b struct {
			Signal string `json:"signal"`
		}
		if json.Unmarshal(ev.Cuerpo, &b) == nil && b.Signal != "" {
			d["signal"] = cortar(b.Signal)
		}
	case "session.create":
		var b struct {
			ID  string `json:"id"`
			Dir string `json:"dir"`
		}
		if json.Unmarshal(ev.Cuerpo, &b) == nil {
			if b.ID != "" {
				d["session"] = cortar(b.ID)
			}
			if b.Dir != "" {
				d["dir"] = cortar(b.Dir)
			}
		}
	case "session.delete":
		d["session"] = cortar(p["session"])
	case "endpoint.create":
		var b struct {
			Port       int  `json:"port"`
			TTLSeconds int  `json:"ttl_seconds"`
			Public     bool `json:"public"`
		}
		if json.Unmarshal(ev.Cuerpo, &b) == nil {
			d["port"], d["ttl_seconds"], d["public"] = b.Port, b.TTLSeconds, b.Public
		}
	case "sandbox.create":
		var b struct {
			Template string `json:"template"`
			Image    string `json:"image"`
			TTL      int    `json:"ttl_seconds"`
			MemMiB   int    `json:"mem_mib"`
			VCPUs    int    `json:"vcpus"`
			Egress   string `json:"egress"`
			Count    *int   `json:"count"`
		}
		if json.Unmarshal(ev.Cuerpo, &b) == nil {
			for k, v := range map[string]string{"template": b.Template, "image": b.Image, "egress": b.Egress} {
				if v != "" {
					d[k] = cortar(v)
				}
			}
			for k, v := range map[string]int{"ttl_seconds": b.TTL, "mem_mib": b.MemMiB, "vcpus": b.VCPUs} {
				if v != 0 {
					d[k] = v
				}
			}
			if b.Count != nil {
				d["count"] = *b.Count
			}
		}
	case "tenant.create", "tenant.update", "tenant.delete", "key.create", "key.rotate", "key.revoke":
		if p["tenant"] != "" {
			d["target_tenant"] = cortar(p["tenant"])
		}
		if p["key"] != "" {
			d["key_id"] = cortar(p["key"])
		}
	case "volume.create", "volume.delete":
		if p["volume"] != "" {
			d["volume"] = cortar(p["volume"])
		}
	case "graph.release":
		d["graph"] = cortar(p["graph"])
	}
	if len(d) == 0 {
		return nil
	}
	return d
}

var secretoEnArg = regexp.MustCompile(`(?i)((?:bearer|basic|token|secret|passw(?:or)?d|passwd|api[_-]?key|access[_-]?key|authorization)[\s=:]+(?:(?:bearer|basic)\s+)?)\S+`)

// Argv devuelve el argv listo para el registro: como mucho maxArgs
// argumentos de maxArgBytes bytes, con lo que parece un secreto tapado
// ("password=…", "Bearer …"). Es una red de seguridad, no una garantía: un
// secreto pasado como argumento suelto no se puede reconocer, y por eso la
// documentación aconseja pasarlo por stdin o por fichero.
func Argv(cmd []string) []string {
	n := min(len(cmd), maxArgs)
	out := make([]string, 0, n+1)
	for _, a := range cmd[:n] {
		out = append(out, secretoEnArg.ReplaceAllString(cortar(a), "${1}[redacted]"))
	}
	if len(cmd) > n {
		out = append(out, "…("+strconv.Itoa(len(cmd)-n)+" more)")
	}
	return out
}

// NombresEnv deja solo los nombres de KEY=value: los valores no se guardan nunca.
func NombresEnv(env []string) []string {
	var out []string
	for i, e := range env {
		if i == maxArgs {
			break
		}
		k, _, _ := strings.Cut(e, "=")
		out = append(out, cortar(k))
	}
	return out
}

func cortar(s string) string {
	if len(s) <= maxArgBytes {
		return s
	}
	s = s[:maxArgBytes]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s + "…"
}
