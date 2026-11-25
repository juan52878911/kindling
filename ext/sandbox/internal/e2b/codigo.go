package e2b

// Intérprete de código: el servidor de e2b-code-interpreter (puerto 49999 del
// sandbox: POST /execute y /contexts) sobre /v1/sandboxes/{id}/code del
// gateway. Solo el camino de resultados de texto y los tipos MIME que da el
// gateway (text/plain, text/html, image/png…): sin gráficos (`chart`) ni
// `env_vars` por ejecución ni reinicio de contextos.

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// tokenTrafico es el trafficAccessToken de un sandbox. El SDK del intérprete
// lo manda (E2B-Traffic-Access-Token) en todas sus peticiones, también en las
// de /contexts, que no llevan X-Access-Token.
func (s *Servidor) tokenTrafico(id string) string {
	h := hmac.New(sha256.New, []byte(s.Clave))
	h.Write([]byte("traffic:" + id))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// codigo envuelve un manejador del intérprete: resuelve el sandbox como envd
// (host `49999-<id>.<dominio>` o cabecera E2b-Sandbox-Id) y exige el token de
// envd, el de tráfico o la clave de la API.
func (s *Servidor) codigo(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.idEnvd(r)
		if !ok {
			errE2B(w, http.StatusNotFound, "sandbox not identified: use the <port>-<sandboxID>.<domain> host or the E2b-Sandbox-Id header with the sandboxID returned by create")
			return
		}
		if s.Clave == "" || !(s.claveValida(r) || igual(r.Header.Get("X-Access-Token"), s.tokenEnvd(id)) ||
			igual(r.Header.Get("E2B-Traffic-Access-Token"), s.tokenTrafico(id))) {
			errE2B(w, http.StatusUnauthorized, "invalid or missing X-Access-Token / E2B-Traffic-Access-Token")
			return
		}
		h(w, r, id)
	}
}

// lenguajeGW pasa el lenguaje de E2B al del gateway (python, bash, javascript).
// Los que el gateway no tiene (r, java, typescript…) pasan tal cual y el
// gateway los rechaza con su mensaje.
func lenguajeGW(l string) string {
	switch strings.ToLower(l) {
	case "", "python", "python3":
		return "python"
	case "js", "javascript", "node":
		return "javascript"
	case "bash", "sh":
		return "bash"
	}
	return l
}

type gwContexto struct {
	ID       string `json:"id"`
	Language string `json:"language"`
	Cwd      string `json:"cwd"`
}

func (s *Servidor) contextoE2B(c gwContexto) map[string]string {
	cwd := c.Cwd
	if cwd == "" {
		cwd = s.Home
	}
	return map[string]string{"id": c.ID, "language": c.Language, "cwd": cwd}
}

func (s *Servidor) ctxCrear(w http.ResponseWriter, r *http.Request, id string) {
	var p struct {
		Language string `json:"language"`
		Cwd      string `json:"cwd"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p) // cuerpo opcional
	pet := map[string]string{"language": lenguajeGW(p.Language)}
	if p.Cwd != "" {
		pet["cwd"] = s.resolver(p.Cwd)
	}
	var c gwContexto
	if err := s.llamarGW(r.Context(), "POST", rutaSB(id)+"/code/contexts", pet, &c); err != nil {
		s.falloE2B(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, s.contextoE2B(c))
}

func (s *Servidor) ctxListar(w http.ResponseWriter, r *http.Request, id string) {
	var cs []gwContexto
	if err := s.llamarGW(r.Context(), "GET", rutaSB(id)+"/code/contexts", nil, &cs); err != nil {
		s.falloE2B(w, err)
		return
	}
	out := []map[string]string{}
	for _, c := range cs {
		out = append(out, s.contextoE2B(c))
	}
	escribirJSON(w, http.StatusOK, out)
}

func (s *Servidor) ctxBorrar(w http.ResponseWriter, r *http.Request, id string) {
	cid := r.PathValue("cid")
	if err := s.llamarGW(r.Context(), "DELETE", rutaSB(id)+"/code/contexts/"+url.PathEscape(cid), nil, nil); err != nil {
		s.falloE2B(w, err)
		return
	}
	s.mu.Lock()
	for k, v := range s.contextos {
		if v == cid && strings.HasPrefix(k, id+"\x00") {
			delete(s.contextos, k)
		}
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// ctxDefecto es el contexto persistente que usa /execute sin context_id, uno
// por sandbox y lenguaje: en E2B el estado (variables) sobrevive entre
// run_code. El gateway sin context_id usaría uno efímero.
func (s *Servidor) ctxDefecto(ctx context.Context, id, idioma string, nuevo bool) (string, error) {
	k := id + "\x00" + idioma
	s.mu.Lock()
	cid := s.contextos[k]
	s.mu.Unlock()
	if cid != "" && !nuevo {
		return cid, nil
	}
	var c gwContexto
	if err := s.llamarGW(ctx, "POST", rutaSB(id)+"/code/contexts", map[string]string{"language": idioma}, &c); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.contextos[k] = c.ID
	s.mu.Unlock()
	return c.ID, nil
}

// eventoCodigo es una línea del NDJSON de POST …/code del gateway.
type eventoCodigo struct {
	Type           string            `json:"type"`
	Text           string            `json:"text"`
	Ename          string            `json:"ename"`
	Evalue         string            `json:"evalue"`
	Traceback      []string          `json:"traceback"`
	Results        map[string]string `json:"results"`
	ExecutionCount int               `json:"execution_count"`
}

// mimeE2B son los campos de Result de E2B para cada tipo MIME; el resto va a
// `extra`.
var mimeE2B = map[string]string{
	"text/plain": "text", "text/html": "html", "text/markdown": "markdown", "image/svg+xml": "svg",
	"image/png": "png", "image/jpeg": "jpeg", "application/pdf": "pdf", "text/latex": "latex",
	"application/json": "json", "application/javascript": "javascript",
}

// plazoCodigo es el timeout_seconds que se pide al gateway (su máximo): en E2B
// el plazo de run_code lo pone el cliente al cortar la conexión.
const plazoCodigo = 600

// codigoEjecutar (POST /execute) corre código y responde el NDJSON del servidor del
// intérprete de E2B: stdout, stderr, result, error y number_of_executions.
func (s *Servidor) codigoEjecutar(w http.ResponseWriter, r *http.Request, id string) {
	var p struct {
		Code      string            `json:"code"`
		ContextID *string           `json:"context_id"`
		Language  *string           `json:"language"`
		EnvVars   map[string]string `json:"env_vars"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&p); err != nil {
		errE2B(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(p.EnvVars) > 0 {
		errE2B(w, http.StatusNotImplemented, "env_vars per execution are not supported by the kindling gateway: pass envs to Sandbox.create (the code interpreter sees them)")
		return
	}
	idioma, cid := "python", ""
	if p.Language != nil {
		idioma = lenguajeGW(*p.Language)
	}
	if p.ContextID != nil {
		cid = *p.ContextID
	}
	porDefecto := cid == ""
	var resp *http.Response
	for intento := 0; ; intento++ {
		if porDefecto {
			var err error
			if cid, err = s.ctxDefecto(r.Context(), id, idioma, intento > 0); err != nil {
				s.falloE2B(w, err)
				return
			}
		}
		pet := map[string]any{"code": p.Code, "context_id": cid, "timeout_seconds": plazoCodigo}
		if !porDefecto && p.Language != nil {
			pet["language"] = idioma
		}
		var err error
		resp, err = s.gw(r.Context(), "POST", rutaSB(id)+"/code", pet)
		if err != nil {
			errE2B(w, http.StatusBadGateway, err.Error())
			return
		}
		if resp.StatusCode < 300 {
			break
		}
		eg := s.leerError(resp)
		resp.Body.Close()
		if eg.Status == http.StatusNotFound && porDefecto && intento == 0 {
			continue // el contexto por defecto murió: se crea otro
		}
		s.falloE2B(w, eg)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	fl, _ := w.(http.Flusher)
	enviar := func(v map[string]any) {
		_ = enc.Encode(v)
		if fl != nil {
			fl.Flush()
		}
	}
	cuenta := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	for sc.Scan() {
		var ev eventoCodigo
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.ExecutionCount > 0 {
			cuenta = ev.ExecutionCount
		}
		switch ev.Type {
		case "stdout", "stderr":
			if ev.Text != "" {
				enviar(map[string]any{"type": ev.Type, "text": s.redactar(ev.Text), "timestamp": time.Now().UnixNano()})
			}
		case "result":
			res := map[string]any{"type": "result", "is_main_result": true}
			extra := map[string]any{}
			for k, v := range ev.Results {
				campo, ok := mimeE2B[k]
				switch {
				case !ok:
					extra[k] = v
				case campo == "json":
					var j any
					if json.Unmarshal([]byte(v), &j) == nil {
						res[campo] = j
					}
				default:
					res[campo] = v
				}
			}
			if _, ok := res["text"]; !ok && ev.Text != "" {
				res["text"] = ev.Text
			}
			if len(extra) > 0 {
				res["extra"] = extra
			}
			enviar(res)
		case "error":
			enviar(map[string]any{"type": "error", "name": ev.Ename, "value": s.redactar(ev.Evalue),
				"traceback": s.redactar(strings.Join(ev.Traceback, "\n"))})
		case "done":
			if cuenta > 0 {
				enviar(map[string]any{"type": "number_of_executions", "execution_count": cuenta})
			}
			return
		}
	}
	if r.Context().Err() != nil {
		// El cliente se fue (run_code con timeout, o cancelado): como el
		// servidor de E2B, se interrumpe la ejecución; el contexto sigue.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()
		if err := s.llamarGW(ctx, "DELETE", rutaSB(id)+"/code/contexts/"+url.PathEscape(cid)+"/execution", nil, nil); err != nil {
			s.logf("e2b: interrupting code context %s: %v", cid, err)
		}
		return
	}
	enviar(map[string]any{"type": "error", "name": "GatewayError", "value": "the gateway closed the stream before the code finished", "traceback": ""})
}
