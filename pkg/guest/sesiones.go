package guest

// Sesiones: un cwd y un entorno con nombre que persisten entre execs.
//
// Un proceso hijo no puede cambiar el directorio ni el entorno de quien lo
// lanzó, así que con una sesión el comando corre DENTRO de una shell que, al
// salir (trap EXIT), escribe su `pwd` y su `env -0` por un descriptor aparte
// (el 3, no el stdout del comando, que es del usuario). El agente lo lee y lo
// guarda como el estado de la sesión: el siguiente exec arranca desde ahí.
//
// Viven en el agente, en memoria: una máquina congelada y descongelada las
// conserva; un reinicio, y una copia de un snapshot (resync), no. Caducan por
// inactividad (api.SessionDefaultTTL) y hay un tope de sesiones.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

type sesion struct {
	id      string
	dir     string
	set     map[string]string
	unset   map[string]bool
	created time.Time
	expires time.Time
	ttl     time.Duration
	execs   int
}

var sesiones = struct {
	mu sync.Mutex
	m  map[string]*sesion
}{m: map[string]*sesion{}}

// ahora es time.Now; variable para los tests de caducidad.
var ahora = time.Now

// barrerSesiones retira las caducadas. Con el cerrojo tomado.
func barrerSesiones() {
	t := ahora()
	for id, s := range sesiones.m {
		if !t.Before(s.expires) {
			delete(sesiones.m, id)
		}
	}
}

func (s *sesion) api() api.Session {
	out := api.Session{ID: s.id, Dir: s.dir, CreatedAt: s.created, ExpiresAt: s.expires, Execs: s.execs}
	for k, v := range s.set {
		out.Env = append(out.Env, k+"="+v)
	}
	for k := range s.unset {
		out.Unset = append(out.Unset, k)
	}
	sort.Strings(out.Env)
	sort.Strings(out.Unset)
	return out
}

var idSesionOK = func(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func parseEnvKV(env []string) (map[string]string, error) {
	out := map[string]string{}
	n := 0
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			return nil, fmt.Errorf("env entry %q is not KEY=value", kv)
		}
		n += len(kv)
		out[kv[:i]] = kv[i+1:]
	}
	if n > api.SessionMaxEnv {
		return nil, fmt.Errorf("session env is %d bytes; the limit is %d", n, api.SessionMaxEnv)
	}
	return out, nil
}

// crearSesion da de alta una sesión. Errores con su código HTTP.
func crearSesion(req api.SessionRequest) (*sesion, int, error) {
	set, err := parseEnvKV(req.Env)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if req.TTLSeconds < 0 || req.TTLSeconds > api.SessionMaxTTL {
		return nil, http.StatusBadRequest, fmt.Errorf("ttl_seconds must be between 0 and %d", api.SessionMaxTTL)
	}
	ttl := time.Duration(api.SessionDefaultTTL) * time.Second
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	id := req.ID
	if id != "" && !idSesionOK(id) {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid session id %q (letters, digits, . _ -; up to 64)", id)
	}
	sesiones.mu.Lock()
	defer sesiones.mu.Unlock()
	barrerSesiones()
	if id == "" {
		var b [6]byte
		_, _ = rand.Read(b[:])
		id = "s-" + hex.EncodeToString(b[:])
	} else if _, dup := sesiones.m[id]; dup {
		return nil, http.StatusConflict, fmt.Errorf("session %q already exists", id)
	}
	if len(sesiones.m) >= api.SessionMax {
		return nil, http.StatusTooManyRequests, fmt.Errorf("too many sessions in this microVM (limit %d)", api.SessionMax)
	}
	t := ahora()
	s := &sesion{id: id, dir: req.Dir, set: set, unset: map[string]bool{}, created: t, expires: t.Add(ttl), ttl: ttl}
	sesiones.m[id] = s
	return s, 0, nil
}

// instantanea es una copia del estado de una sesión, para usarla sin cerrojo.
type instantanea struct {
	dir   string
	set   map[string]string
	unset map[string]bool
}

// usarSesion devuelve el estado de id y renueva su plazo. false si no existe o
// caducó.
func usarSesion(id string) (instantanea, bool) {
	sesiones.mu.Lock()
	defer sesiones.mu.Unlock()
	barrerSesiones()
	s, ok := sesiones.m[id]
	if !ok {
		return instantanea{}, false
	}
	s.expires = ahora().Add(s.ttl)
	in := instantanea{dir: s.dir, set: map[string]string{}, unset: map[string]bool{}}
	for k, v := range s.set {
		in.set[k] = v
	}
	for k := range s.unset {
		in.unset[k] = true
	}
	return in, true
}

// entornoDe aplica la sesión a un entorno base.
func (in instantanea) entornoDe(base []string) []string {
	out := make([]string, 0, len(base)+len(in.set))
	for _, kv := range base {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if in.unset[k] {
			continue
		}
		if _, ok := in.set[k]; ok {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(in.set))
	for k := range in.set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+in.set[k])
	}
	return out
}

// envIgnorado son las variables que la propia shell mueve y que no son de la
// sesión.
var envIgnorado = map[string]bool{"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true}

// guardarEstado aplica a la sesión lo que cambió un exec: dir es donde acabó
// y final el entorno con que acabó, frente a inicio con el que empezó.
func guardarEstado(id, dir string, inicio, final map[string]string) {
	sesiones.mu.Lock()
	defer sesiones.mu.Unlock()
	s, ok := sesiones.m[id]
	if !ok {
		return // borrada mientras corría el comando
	}
	s.execs++
	if dir != "" {
		s.dir = dir
	}
	for k, v := range final {
		if envIgnorado[k] {
			continue
		}
		if old, ok := inicio[k]; !ok || old != v {
			s.set[k] = v
			delete(s.unset, k)
		}
	}
	for k := range inicio {
		if envIgnorado[k] {
			continue
		}
		if _, ok := final[k]; !ok {
			delete(s.set, k)
			s.unset[k] = true
		}
	}
}

// citar escapa s para una shell POSIX entre comillas simples.
func citar(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// guionDe devuelve el script de shell de un argv: ["sh","-c",S] es S; otro
// argv, sus palabras citadas.
func guionDe(argv []string) string {
	if len(argv) == 3 && argv[1] == "-c" {
		switch path.Base(argv[0]) {
		case "sh", "bash", "dash", "ash", "zsh":
			return argv[2]
		}
	}
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = citar(a)
	}
	return strings.Join(q, " ")
}

// envolturaSesion es la shell que corre el guion y, al salir por donde sea
// (también con `exit N`), vuelca pwd y el entorno por el descriptor 3 sin
// cambiar el código de salida.
const envolturaSesion = `trap '__kling_rc=$?; { pwd; echo; env -0; } >&3 2>/dev/null; exit $__kling_rc' EXIT
eval "$1"`

// estadoCapturado es lo que el trap escribió por el descriptor 3.
type estadoCapturado struct {
	dir string
	env map[string]string
}

// leerEstado interpreta "pwd\n\nK=V\0K=V\0…".
func leerEstado(b []byte) (estadoCapturado, bool) {
	i := bytes.Index(b, []byte("\n\n"))
	if i < 0 {
		return estadoCapturado{}, false
	}
	st := estadoCapturado{dir: string(b[:i]), env: map[string]string{}}
	for _, rec := range bytes.Split(b[i+2:], []byte{0}) {
		kv := string(rec)
		if j := strings.IndexByte(kv, '='); j > 0 {
			st.env[kv[:j]] = kv[j+1:]
		}
	}
	return st, true
}

// capturaSesion lee el descriptor 3 mientras corre el comando (si no, un
// entorno de más de 64 KiB bloquearía a la shell al escribir).
type capturaSesion struct {
	pw   *os.File // lo que hereda el hijo
	pr   *os.File
	done chan struct{}
	buf  bytes.Buffer
}

func nuevaCaptura() (*capturaSesion, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	c := &capturaSesion{pw: pw, pr: pr, done: make(chan struct{})}
	return c, nil
}

// arrancada se llama tras Start: el padre suelta su copia de escritura (sin
// eso no habría EOF) y empieza a leer.
func (c *capturaSesion) arrancada() {
	_ = c.pw.Close()
	go func() {
		defer close(c.done)
		_, _ = io.Copy(&c.buf, io.LimitReader(c.pr, api.SessionMaxEnv+4096))
	}()
}

// resultado espera un poco el final de la lectura (un nieto con el descriptor
// heredado la alargaría para siempre) y la interpreta.
func (c *capturaSesion) resultado() (estadoCapturado, bool) {
	select {
	case <-c.done:
	case <-time.After(500 * time.Millisecond):
		_ = c.pr.SetReadDeadline(time.Now())
		<-c.done
	}
	_ = c.pr.Close()
	return leerEstado(c.buf.Bytes())
}

func (c *capturaSesion) abortar() {
	_ = c.pw.Close()
	_ = c.pr.Close()
}

func envComoMapa(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// SessionsHandler sirve /sessions y /sessions/{id}.
func SessionsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, api.GuestSessionsPath), "/")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case id == "" && r.Method == http.MethodPost:
			var req api.SessionRequest
			if err := json.NewDecoder(io.LimitReader(r.Body, 2*api.SessionMaxEnv)).Decode(&req); err != nil {
				http.Error(w, fmt.Sprintf("invalid body: %v", err), http.StatusBadRequest)
				return
			}
			s, code, err := crearSesion(req)
			if err != nil {
				http.Error(w, err.Error(), code)
				return
			}
			sesiones.mu.Lock()
			out := s.api()
			sesiones.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(out)
		case id == "" && r.Method == http.MethodGet:
			sesiones.mu.Lock()
			barrerSesiones()
			out := make([]api.Session, 0, len(sesiones.m))
			for _, s := range sesiones.m {
				out = append(out, s.api())
			}
			sesiones.mu.Unlock()
			sort.Slice(out, func(i, j int) bool {
				return out[i].CreatedAt.Before(out[j].CreatedAt) || out[i].CreatedAt.Equal(out[j].CreatedAt) && out[i].ID < out[j].ID
			})
			_ = json.NewEncoder(w).Encode(out)
		case id != "" && r.Method == http.MethodGet:
			sesiones.mu.Lock()
			barrerSesiones()
			s, ok := sesiones.m[id]
			var out api.Session
			if ok {
				out = s.api()
			}
			sesiones.mu.Unlock()
			if !ok {
				http.Error(w, fmt.Sprintf("no session %q", id), http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(out)
		case id != "" && r.Method == http.MethodDelete:
			sesiones.mu.Lock()
			_, ok := sesiones.m[id]
			delete(sesiones.m, id)
			sesiones.mu.Unlock()
			if !ok {
				http.Error(w, fmt.Sprintf("no session %q", id), http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// olvidarSesiones las borra todas: lo que hace el resync de una copia.
func olvidarSesiones() int {
	sesiones.mu.Lock()
	defer sesiones.mu.Unlock()
	n := len(sesiones.m)
	sesiones.m = map[string]*sesion{}
	return n
}
